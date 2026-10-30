package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

// redactStub issues one echo tool call with scripted args, then answers.
type redactStub struct {
	n    int32
	args string
}

func (*redactStub) Name() string { return "redactstub" }
func (*redactStub) Capabilities(context.Context) (router.Caps, error) {
	return router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"}, nil
}

func (s *redactStub) Complete(_ context.Context, _ []router.Message, opts router.Options) (*router.Completion, error) {
	if atomic.AddInt32(&s.n, 1) == 1 && len(opts.Tools) > 0 {
		return &router.Completion{ToolCalls: []router.ToolCall{{ID: "c1", Name: "echo", ArgsJSON: s.args}}, Model: "redactstub"}, nil
	}
	return &router.Completion{Text: "ok", Model: "redactstub"}, nil
}

// runRedact drives one tool turn whose args and result both carry needle.
func runRedact(t *testing.T, secrets func() []string, argsJSON, result string) (*Session, string) {
	t.Helper()
	dir := t.TempDir()
	log, err := penatus.OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	ts := &redactStub{args: argsJSON}
	rt := router.NewRouter(router.Profile{Chain: []router.Target{{Provider: ts}}})
	rt.SetProfile("compact", router.Profile{Chain: []router.Target{{Provider: ts}}})
	s := &Session{Log: log, Sys: "SYS", Router: rt, CompactProfile: "compact", TriggerPct: 80, Secrets: secrets}
	tools := []Tool{{
		Spec:    router.ToolSpec{Name: "echo", ParamsJSONSchema: `{"type":"object"}`},
		Trusted: true,
		Run:     func(context.Context, string) (string, error) { return result, nil },
	}}
	if _, err := s.RunTurnTools(context.Background(), "go", nil, tools, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return s, string(raw)
}

func TestRedactB7_ArgsAndResult(t *testing.T) {
	const needle = "sk-live-SECRET9876"
	s, raw := runRedact(t, func() []string { return []string{needle} },
		`{"v":"`+needle+`","n":[1,"x `+needle+`"]}`, "result "+needle+" end")
	if strings.Contains(raw, needle) {
		t.Fatalf("needle in events.jsonl:\n%s", raw)
	}
	if !strings.Contains(raw, "[redacted]") {
		t.Fatalf("no redaction marker:\n%s", raw)
	}
	var sawResult bool
	for _, m := range s.messages() {
		if strings.Contains(m.Content, needle) {
			t.Fatalf("needle in replay: %+v", m)
		}
		for _, c := range m.ToolCalls {
			if strings.Contains(c.ArgsJSON, needle) || !strings.Contains(c.ArgsJSON, "[redacted]") {
				t.Fatalf("replayed args = %s", c.ArgsJSON)
			}
		}
		if m.Role == router.RoleTool {
			sawResult = true
			if m.Content != "result [redacted] end" {
				t.Fatalf("replayed result = %q", m.Content)
			}
		}
	}
	if !sawResult {
		t.Fatal("no tool result in replay")
	}
}

func TestRedactB8_EscapeTrap(t *testing.T) {
	needle := "p\"w\\x\nz9876"
	enc, err := json.Marshal(needle)
	if err != nil {
		t.Fatal(err)
	}
	_, raw := runRedact(t, func() []string { return []string{needle} },
		`{"v":`+string(enc)+`}`, "got "+needle)
	if strings.Contains(raw, string(enc[1:len(enc)-1])) || strings.Contains(raw, needle) {
		t.Fatalf("escaped needle survived:\n%s", raw)
	}
	if !strings.Contains(raw, "[redacted]") {
		t.Fatalf("no redaction marker:\n%s", raw)
	}
}

func TestRedactB9_DisabledByteIdentical(t *testing.T) {
	fields := map[string]json.RawMessage{
		"args": json.RawMessage(`{"a":"<x> & \"q\"","n":1.50}`),
		"text": json.RawMessage(`"plain"`),
	}
	cases := []struct {
		name    string
		secrets func() []string
	}{
		{"nil", nil},
		{"empty", func() []string { return nil }},
		{"empty_needle", func() []string { return []string{""} }},
		{"no_match", func() []string { return []string{"absent9876"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Session{Secrets: tc.secrets}
			got := s.redactFields(fields)
			for k, v := range fields {
				if !bytes.Equal(got[k], v) {
					t.Fatalf("%s changed: %s != %s", k, got[k], v)
				}
			}
		})
	}
	_, base := runRedact(t, nil, `{"v":"hi"}`, "res")
	_, empty := runRedact(t, func() []string { return []string{""} }, `{"v":"hi"}`, "res")
	if strings.Contains(empty, "[redacted]") || strings.Count(base, "\n") != strings.Count(empty, "\n") {
		t.Fatalf("disabled run differs:\n%s\n--\n%s", base, empty)
	}
}
