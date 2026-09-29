package loop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lararium-app/lararium/internal/router"
)

// Tool is one callable tool: its wire spec plus an executor.
type Tool struct {
	Spec router.ToolSpec
	// Trusted tools run without asking; untrusted ones go through the
	// approval hook (spec §2: approval = auto | approved:<id> | denied:<id>).
	Trusted bool
	Run     func(ctx context.Context, argsJSON string) (string, error)
}

// Approver is asked for permission to run an untrusted tool. It receives a
// human-readable summary and must return a decision. approvalID identifies
// this approval in the log.
type Approver func(name, argsSummary string) (ok bool, approvalID string)

// toolEngine dispatches tool calls against a registry.
type toolEngine struct {
	byName map[string]*Tool
	// blobsDir holds result_ref payloads over 8 KB (spec §2 tool_result).
	blobsDir string
	approver Approver // nil = deny all untrusted
}

func newToolEngine(tools []Tool, sessionDir string, ap Approver) *toolEngine {
	e := &toolEngine{byName: map[string]*Tool{}, blobsDir: filepath.Join(sessionDir, "blobs"), approver: ap}
	for i := range tools {
		t := tools[i]
		e.byName[t.Spec.Name] = &t
	}
	return e
}

func (e *toolEngine) specs() []router.ToolSpec {
	var out []router.ToolSpec
	for _, t := range e.byName {
		out = append(out, t.Spec)
	}
	return out
}

// ToolOutcome is what Run records in the event log + returns for the wire.
type ToolOutcome struct {
	Approval  string `json:"approval"` // auto | approved:<id> | denied:<id>
	OK        bool   `json:"ok"`
	Text      string `json:"-"` // full text sent to the model
	Digest    string `json:"digest"`
	ResultRef string `json:"result_ref,omitempty"`
}

const blobThreshold = 8 * 1024

// Gate makes the approval decision for a call WITHOUT running it, so the
// dispatch can be logged before execution (a hung tool must still have its
// dispatch record). Returns the approval string, the tool (nil if unknown/
// denied), and denyText (non-empty if this call must not run).
func (e *toolEngine) Gate(call router.ToolCall) (approval string, t *Tool, denyText string) {
	t, known := e.byName[call.Name]
	if !known {
		return "auto", nil, fmt.Sprintf("unknown tool %q", call.Name)
	}
	if t.Trusted {
		return "auto", t, ""
	}
	if e.approver == nil {
		return "denied:no-approver", nil, "tool requires approval but no approver is wired"
	}
	ok, id := e.approver(call.Name, summarize(call.ArgsJSON))
	if !ok {
		return "denied:" + orNone(id), nil, "user denied this tool call"
	}
	return "approved:" + orNone(id), t, ""
}

// Execute runs an approved tool call and prepares its outcome (blob spill
// for results over 8 KB, per spec §2).
func (e *toolEngine) Execute(ctx context.Context, call router.ToolCall, t *Tool, approval string) ToolOutcome {
	text, err := t.Run(ctx, call.ArgsJSON)
	if err != nil {
		return ToolOutcome{Approval: approval, OK: false, Text: "tool error: " + err.Error(), Digest: digest(call.ArgsJSON)}
	}
	out := ToolOutcome{Approval: approval, OK: true, Text: text, Digest: digest(text)}
	if len(text) > blobThreshold {
		if mkErr := os.MkdirAll(e.blobsDir, 0o755); mkErr == nil {
			ref := filepath.Join(e.blobsDir, out.Digest[:16]+".txt")
			if wErr := os.WriteFile(ref, []byte(text), 0o600); wErr == nil {
				rel, _ := filepath.Rel(filepath.Dir(e.blobsDir), ref)
				out.ResultRef = rel
			}
		}
	}
	return out
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:32]
}

func summarize(argsJSON string) string {
	if len(argsJSON) > 200 {
		return argsJSON[:200] + "…"
	}
	return argsJSON
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

var _ = json.Marshal // used by session.go helpers via same package
