package loop

import (
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
