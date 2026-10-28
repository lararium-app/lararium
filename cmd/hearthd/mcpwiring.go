package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/custosdoor"
	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/router"
)

// PolicyDecider evaluates policy verdicts for connector tool calls (issue #129).
type PolicyDecider interface {
	Decide(connector, tool string) string
}

// MCPCaller executes an MCP tool call (issue #129 seam).
type MCPCaller interface {
	Call(ctx context.Context, connector, tool string, args json.RawMessage) (json.RawMessage, error)
}

// MCPToolClient provides tool listing and execution (issue #129 seam).
type MCPToolClient interface {
	MCPCaller
	ListTools() []custosdoor.ToolInfo
}

// mcpToolSource builds the dynamic tool provider for the web-chat Hub
// from the custody door client's rail state (issue #83): tools of every
// READY connector, pulled fresh at each turn start. Execution rides
// MCP-CALL over doors.sock (MCP-SPEC §9, amendment A3). Gated by the
// policy engine (issue #129): auto silences cards, deny blocks execution,
// ask requires human approval.
func mcpToolSource(client MCPToolClient, deciders ...PolicyDecider) func() []loop.Tool {
	var decider PolicyDecider
	if len(deciders) > 0 {
		decider = deciders[0]
	}
	return func() []loop.Tool {
		infos := client.ListTools()
		if len(infos) == 0 {
			return nil
		}
		out := make([]loop.Tool, 0, len(infos))
		seen := make(map[string]bool, len(infos))
		for _, ti := range infos {
			lt := mcpLoopTool(client, ti, decider)
			// Two connectors exposing tools that sanitize to the
			// same model-facing name: first wins, second skipped
			// (a duplicate spec name would poison the whole list).
			if lt.Spec.Name == "" || seen[lt.Spec.Name] {
				continue
			}
			seen[lt.Spec.Name] = true
			out = append(out, lt)
		}
		return out
	}
}

// mcpToolName maps a connector+tool pair onto a model-facing name that
// satisfies the strictest provider grammar (^[a-zA-Z0-9_-]{1,64}$).
func mcpToolName(connector, tool string) string {
	name := "mcp__" + connector + "__" + tool
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	const maxName = 64
	if len(s) > maxName {
		h := fnv.New32a()
		_, _ = h.Write([]byte(name))
		suffix := fmt.Sprintf("_%08x", h.Sum32())
		s = s[:maxName-len(suffix)] + suffix
	}
	return s
}

// mcpRulePattern reverse-maps a model-facing tool name back onto the
// canonical worker-lane rule pattern "<connector>/<tool>" by matching
// against the rail's live tool list — never by parsing the sanitized
// name, which is lossy (issue #132). ok=false for names that are not
// current rail tools (stale card, local tool): the caller must not mint.
func mcpRulePattern(lister MCPToolLister, modelName string) (string, bool) {
	if lister == nil || modelName == "" {
		return "", false
	}
	for _, ti := range lister.ListTools() {
		if mcpToolName(ti.Connector, ti.Name) == modelName {
			return ti.Connector + "/" + ti.Name, true
		}
	}
	return "", false
}

// mcpConnectorName reverse-maps a model-facing tool name back onto the
// connector name by matching against the rail's live tool list (issue #142).
func mcpConnectorName(lister MCPToolLister, modelName string) (string, bool) {
	if lister == nil || modelName == "" {
		return "", false
	}
	for _, ti := range lister.ListTools() {
		if mcpToolName(ti.Connector, ti.Name) == modelName || ti.Connector+"/"+ti.Name == modelName || ti.Name == modelName {
			return ti.Connector, true
		}
	}
	return "", false
}

// MCPToolLister is the rail tool-listing seam (issue #132).
type MCPToolLister interface {
	ListTools() []custosdoor.ToolInfo
}

// mcpLoopTool adapts one rail tool to the loop registry with policy gating (issue #129).
func mcpLoopTool(client MCPCaller, ti custosdoor.ToolInfo, deciders ...PolicyDecider) loop.Tool {
	schema := string(ti.InputSchema)
	if schema == "" || schema == "null" {
		schema = `{"type":"object"}`
	}
	connector, tool := ti.Connector, ti.Name

	var decider PolicyDecider
	if len(deciders) > 0 {
		decider = deciders[0]
	}

	verdict := custos.VerdictAsk
	if decider != nil {
		verdict = decider.Decide(connector, tool)
	}

	trusted := false
	switch verdict {
	case custos.VerdictAuto:
		trusted = true
	case custos.VerdictDeny:
		// Deny must return an error result to the model without presenting an approval
		// card (issue #129); setting Trusted: true ensures Gate passes execution to Run,
		// which returns the policy rejection error.
		trusted = true
	default:
		// Ask or no matching rule -> untrusted: routes to human approver (fail-closed default).
		trusted = false
	}

	return loop.Tool{
		Spec: router.ToolSpec{
			Name:             mcpToolName(connector, tool),
			Description:      ti.Title,
			ParamsJSONSchema: schema,
		},
		Trusted: trusted,
		Run: func(ctx context.Context, argsJSON string) (string, error) {
			// Re-decide at execution time (issue #129): deny always
			// blocks; a trusted tool whose policy flipped to ask
			// mid-turn must NOT execute silently either — a card was
			// never raised for the new state, so fail closed.
			if decider != nil {
				curVerdict := decider.Decide(connector, tool)
				if curVerdict == custos.VerdictDeny {
					return "", fmt.Errorf("mcp %s/%s: blocked by policy", connector, tool)
				}
				if curVerdict == custos.VerdictAsk && trusted {
					return "", fmt.Errorf("mcp %s/%s: policy changed to ask; retry the turn", connector, tool)
				}
			}

			if argsJSON == "" {
				argsJSON = "{}"
			}
			res, err := client.Call(ctx, connector, tool, json.RawMessage(argsJSON))
			if err != nil {
				return "", fmt.Errorf("mcp %s/%s: %w", connector, tool, err)
			}
			return renderMCPResult(res), nil
		},
	}
}

// renderMCPResult flattens the MCP result frame (§6.3 content[] shape)
// into model-facing text: text blocks joined; a non-text-only frame
// falls back to the compact frame; isError is surfaced inline so the
// model can react without the turn dying.
func renderMCPResult(raw json.RawMessage) string {
	var frame struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		return string(raw)
	}
	var parts []string
	for _, c := range frame.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		} else {
			parts = append(parts, "["+c.Type+" content]")
		}
	}
	text := strings.Join(parts, "\n")
	if text == "" {
		if len(frame.Content) == 0 {
			return "(no output)"
		}
		return string(raw)
	}
	if frame.IsError {
		return "MCP tool error: " + text
	}
	return text
}
