package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

// toolStub replies with a scripted sequence: tool call first, final text after.
type toolStub struct {
	n       int32
	gotMsgs [][]router.Message
	sawTool bool
}

func (s *toolStub) Name() string { return "toolstub" }
func (s *toolStub) Capabilities(context.Context) (router.Caps, error) {
	c := router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"}
	return c, nil
}

func (s *toolStub) Complete(_ context.Context, msgs []router.Message, opts router.Options) (*router.Completion, error) {
	s.gotMsgs = append(s.gotMsgs, msgs)
	i := atomic.AddInt32(&s.n, 1)
	// Any tool results in the request -> answer with text.
	for _, m := range msgs {
		if m.Role == router.RoleTool {
			s.sawTool = true
			// echo the tool text so the test can assert on it
			return &router.Completion{Text: "final:" + m.Content, Model: "toolstub"}, nil
		}
	}
	if i == 1 && len(opts.Tools) > 0 {
		return &router.Completion{
			ToolCalls: []router.ToolCall{{ID: "call_1", Name: "echo", ArgsJSON: `{"v":"hi there"}`}},
			Model:     "toolstub",
		}, nil
	}
	return &router.Completion{Text: "done without tools", Model: "toolstub"}, nil
}

func TestToolLoopDispatchAndProjection(t *testing.T) {
	ts := &toolStub{}
	dir := t.TempDir()
	log, err := penatus.OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.NewRouter(router.Profile{Chain: []router.Target{{Provider: ts}}})
	rt.SetProfile("compact", router.Profile{Chain: []router.Target{{Provider: ts}}})
	s := &Session{Log: log, Sys: "SYS", Router: rt, CompactProfile: "compact", TriggerPct: 80}

	calls := map[string]string{}
	tools := []Tool{{
		Spec:    router.ToolSpec{Name: "echo", ParamsJSONSchema: `{"type":"object"}`},
		Trusted: true,
		Run: func(_ context.Context, args string) (string, error) {
			calls["echo"] = args
			return "ECHOED:hi there", nil
		},
	}}

	text, err := s.RunTurnTools(context.Background(), "go", nil, tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	if text != "final:ECHOED:hi there" {
		t.Fatalf("final text = %q, want tool result woven in", text)
	}
	if calls["echo"] != `{"v":"hi there"}` {
		t.Fatalf("tool got args %q", calls["echo"])
	}

	// Event log: msg, tool_call (approval=auto), tool_result, msg.
	// (user msg, assistant tool msg NOT logged — dispatch events carry it)
	var kinds []string
	for _, ev := range log.Live() {
		kinds = append(kinds, ev.T)
	}
	want := []string{"msg", "tool_call", "tool_result", "msg"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}

	// Projection: replay must produce assistant-with-tool_calls + tool result.
	msgs := s.messages()
	var foundCall, foundResult bool
	for i, m := range msgs {
		if len(m.ToolCalls) > 0 && m.ToolCalls[0].Name == "echo" {
			foundCall = true
			if i+1 < len(msgs) && msgs[i+1].Role == router.RoleTool && msgs[i+1].Content == "ECHOED:hi there" && msgs[i+1].ToolCallID == "call_1" {
				foundResult = true
			}
		}
	}
	if !foundCall || !foundResult {
		t.Fatalf("projection missing tool pair (call=%v result=%v): %+v", foundCall, foundResult, msgs)
	}
}

func TestDeniedToolNotExecuted(t *testing.T) {
	ts := &toolStub{}
	dir := t.TempDir()
	log, _ := penatus.OpenLog(dir)
	rt := router.NewRouter(router.Profile{Chain: []router.Target{{Provider: ts}}})
	rt.SetProfile("compact", router.Profile{Chain: []router.Target{{Provider: ts}}})
	s := &Session{Log: log, Sys: "SYS", Router: rt, CompactProfile: "compact", TriggerPct: 80}

	ran := false
	tools := []Tool{{
		Spec: router.ToolSpec{Name: "echo", ParamsJSONSchema: `{"type":"object"}`},
		Run: func(context.Context, string) (string, error) {
			ran = true
			return "should not run", nil
		},
	}}
	_, err := s.RunTurnTools(context.Background(), "go", nil, tools,
		func(string, string) (bool, string) { return false, "" })
	if err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("denied tool executed")
	}
	for _, ev := range log.Live() {
		if ev.T == "tool_call" {
			var f struct {
				Approval string `json:"approval"`
			}
			b, _ := ev.MarshalJSON()
			_ = json.Unmarshal(b, &f)
			if !strings.HasPrefix(f.Approval, "denied:") {
				t.Fatalf("approval = %q, want denied:*", f.Approval)
			}
		}
	}
}

func TestLargeResultSpillsBlob(t *testing.T) {
	// Drive the engine directly (loop covered above).
	dir := t.TempDir()
	e := newToolEngine([]Tool{{
		Spec:    router.ToolSpec{Name: "big", ParamsJSONSchema: `{}`},
		Trusted: true,
		Run: func(context.Context, string) (string, error) {
			return strings.Repeat("x", blobThreshold+10), nil
		},
	}}, dir, nil)
	out := e.Execute(context.Background(), router.ToolCall{ID: "c", Name: "big"}, e.byName["big"], "auto")
	if out.ResultRef == "" {
		t.Fatal("expected result_ref for large result")
	}
	b, err := os.ReadFile(filepath.Join(dir, out.ResultRef))
	if err != nil || len(b) != blobThreshold+10 {
		t.Fatalf("blob unreadable: %v len=%d", err, len(b))
	}
}

func TestFullArgsPreservedOnCard(t *testing.T) {
	longArgs := `{"phone_number":"+15551234567","extra":"` + strings.Repeat("a", 300) + `"}`
	var capturedSummary string
	ap := func(name, summary string) (bool, string) {
		capturedSummary = summary
		return true, "app-1"
	}

	// Tool with FullArgs: false should truncate to 200 chars + "…"
	eTruncated := newToolEngine([]Tool{{
		Spec:     router.ToolSpec{Name: "action_tool", ParamsJSONSchema: `{}`},
		Trusted:  false,
		FullArgs: false,
	}}, t.TempDir(), ap)
	approval, tool, deny := eTruncated.Gate(router.ToolCall{Name: "action_tool", ArgsJSON: longArgs})
	if approval != "approved:app-1" || tool == nil || deny != "" {
		t.Fatalf("gate failed: approval=%s deny=%s", approval, deny)
	}
	if len(capturedSummary) > 205 || !strings.HasSuffix(capturedSummary, "…") {
		t.Fatalf("expected truncated summary, got len %d: %s", len(capturedSummary), capturedSummary)
	}

	// Tool with FullArgs: true must preserve the entire JSON verbatim
	eFull := newToolEngine([]Tool{{
		Spec:     router.ToolSpec{Name: "action_tool", ParamsJSONSchema: `{}`},
		Trusted:  false,
		FullArgs: true,
	}}, t.TempDir(), ap)
	approval, tool, deny = eFull.Gate(router.ToolCall{Name: "action_tool", ArgsJSON: longArgs})
	if approval != "approved:app-1" || tool == nil || deny != "" {
		t.Fatalf("gate failed: approval=%s deny=%s", approval, deny)
	}
	if capturedSummary != longArgs {
		t.Fatalf("expected full args preserved, got: %s", capturedSummary)
	}
}

// sigStub replies with a tool call and extra_content first, then verifies the follow-up request carries it.
type sigStub struct {
	n        int32
	gotMsgs  [][]router.Message
	sawSig   bool
	sigBytes []byte
}

func (s *sigStub) Name() string { return "sigstub" }
func (s *sigStub) Capabilities(context.Context) (router.Caps, error) {
	return router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"}, nil
}

func (s *sigStub) Complete(_ context.Context, msgs []router.Message, opts router.Options) (*router.Completion, error) {
	s.gotMsgs = append(s.gotMsgs, msgs)
	i := atomic.AddInt32(&s.n, 1)

	for _, m := range msgs {
		if m.Role == router.RoleAssistant && len(m.ToolCalls) > 0 {
			if bytes.Equal(m.ExtraContent, s.sigBytes) {
				s.sawSig = true
			}
		}
	}

	if i == 1 && len(opts.Tools) > 0 {
		return &router.Completion{
			ToolCalls:    []router.ToolCall{{ID: "call_sig_1", Name: "echo", ArgsJSON: `{"v":"hello"}`}},
			ExtraContent: s.sigBytes,
			Model:        "sigstub",
		}, nil
	}
	return &router.Completion{Text: "final:done", Model: "sigstub"}, nil
}

func TestToolLoopExtraContentPassthrough(t *testing.T) {
	wantSig := []byte(`{"google":{"thought_signature":"b64_sig_turn_abc=="}}`)
	ss := &sigStub{sigBytes: wantSig}
	dir := t.TempDir()
	log, err := penatus.OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.NewRouter(router.Profile{Chain: []router.Target{{Provider: ss}}})
	rt.SetProfile("compact", router.Profile{Chain: []router.Target{{Provider: ss}}})
	s := &Session{Log: log, Sys: "SYS", Router: rt, CompactProfile: "compact", TriggerPct: 80}

	calls := map[string]string{}
	tools := []Tool{{
		Spec:    router.ToolSpec{Name: "echo", ParamsJSONSchema: `{"type":"object"}`},
		Trusted: true,
		Run: func(_ context.Context, args string) (string, error) {
			calls["echo"] = args
			return "ECHOED:hello", nil
		},
	}}

	text, err := s.RunTurnTools(context.Background(), "go", nil, tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	if text != "final:done" {
		t.Fatalf("final text = %q, want final:done", text)
	}
	if calls["echo"] != `{"v":"hello"}` {
		t.Fatalf("tool got args %q", calls["echo"])
	}

	// Verify the provider's follow-up request carried the signature.
	if !ss.sawSig {
		t.Fatalf("follow-up request to provider did not contain thought_signature extra_content; saw messages: %+v", ss.gotMsgs)
	}

	// Verify events.jsonl format was NOT changed and contains no thought_signature.
	var kinds []string
	for _, ev := range log.Live() {
		kinds = append(kinds, ev.T)
		b, _ := ev.MarshalJSON()
		if bytes.Contains(b, []byte("thought_signature")) {
			t.Fatalf("events.jsonl must not contain thought_signature, got event: %s", string(b))
		}
	}
	want := []string{"msg", "tool_call", "tool_result", "msg"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}

	// Verify in-memory reconstruction preserves extra_content.
	msgs := s.messages()
	var foundAsstWithSig bool
	for _, m := range msgs {
		if m.Role == router.RoleAssistant && len(m.ToolCalls) > 0 {
			if bytes.Equal(m.ExtraContent, wantSig) {
				foundAsstWithSig = true
			}
		}
	}
	if !foundAsstWithSig {
		t.Fatalf("reconstructed messages missing assistant ExtraContent: %+v", msgs)
	}

	// Drive a second turn on the same session to ensure history replay preserves extra_content across turns.
	ss.sawSig = false
	_, err = s.RunTurnTools(context.Background(), "next turn", nil, tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ss.sawSig {
		t.Fatal("history messages in turn 2 did not preserve thought_signature extra_content")
	}
}
