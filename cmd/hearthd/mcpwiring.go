package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/lararium-app/lararium/internal/custosdoor"
	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/router"
)

// mcpToolSource builds the dynamic tool provider for the web-chat Hub
// from the custody door client's rail state (issue #83): tools of every
// READY connector, pulled fresh at each turn start. Execution rides
// MCP-CALL over doors.sock (MCP-SPEC §9, amendment A3), so approval,
// policy, and scrubbing all stay inside custosd — hearthd only ferries
// the frame. Tools stay untrusted: every call asks the approver.
func mcpToolSource(client *custosdoor.Client) func() []loop.Tool {
	return func() []loop.Tool {
		infos := client.ListTools()
		if len(infos) == 0 {
			return nil
		}
		out := make([]loop.Tool, 0, len(infos))
		seen := make(map[string]bool, len(infos))
		for _, ti := range infos {
			lt := mcpLoopTool(client, ti)
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

// mcpLoopTool adapts one rail tool to the loop registry.
func mcpLoopTool(client *custosdoor.Client, ti custosdoor.ToolInfo) loop.Tool {
	schema := string(ti.InputSchema)
	if schema == "" || schema == "null" {
		schema = `{"type":"object"}`
	}
	connector, tool := ti.Connector, ti.Name
	return loop.Tool{
		Spec: router.ToolSpec{
			Name:             mcpToolName(connector, tool),
			Description:      ti.Title,
			ParamsJSONSchema: schema,
		},
		Trusted: false,
		Run: func(ctx context.Context, argsJSON string) (string, error) {
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
