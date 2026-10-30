package loop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

func TestDenialGuidanceTable(t *testing.T) {
	cases := []struct {
		gate, gateName, allowPath string
	}{
		{gateDenied, "approval policy", "Allow on the approval card"},
		{gateNoApprover, "approval policy", "web UI"},
		{gateNoChannel, "approval policy", "tap Allow"},
		{gateTimeout, "timed out", "serve.approval_timeout"},
		{gateCustos, "custos door", "custos -config ./lararium.yaml unlock"},
	}
	for _, c := range cases {
		got := denialGuidance(c.gate, "web_fetch")
		for _, want := range []string{c.gateName, c.allowPath, "run the same call again", "web_fetch", "Allowing it would"} {
			if !strings.Contains(got, want) {
				t.Errorf("gate %s: %q missing %q", c.gate, got, want)
			}
		}
		if !strings.Contains(strings.ToLower(got), "again") {
			t.Errorf("gate %s: no retry invite", c.gate)
		}
	}
	if strings.Contains(denialGuidance(gateDenied, "x"), "timed out") {
		t.Error("explicit denial must not claim timeout")
	}
}

func TestDeniedToolResultCarriesGuidance(t *testing.T) {
	ts := &toolStub{}
	log, _ := penatus.OpenLog(t.TempDir())
	rt := router.NewRouter(router.Profile{Chain: []router.Target{{Provider: ts}}})
	rt.SetProfile("compact", router.Profile{Chain: []router.Target{{Provider: ts}}})
	s := &Session{
		Log: log, Sys: "SYS", Router: rt, CompactProfile: "compact", TriggerPct: 80,
		AuditApproval: func(string) (json.RawMessage, bool) {
			return json.RawMessage(`{"decision":"denied","reason":"timeout","source":"timer"}`), true
		},
	}
	tools := []Tool{{
		Spec: router.ToolSpec{Name: "echo", ParamsJSONSchema: `{"type":"object"}`},
		Run:  func(context.Context, string) (string, error) { return "no", nil },
	}}
	if _, err := s.RunTurnTools(context.Background(), "go", nil, tools,
		func(string, string) (bool, string) { return false, "a_1" }); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range s.messages() {
		if m.Role == router.RoleTool {
			found = true
			if !strings.Contains(m.Content, "timed out") || !strings.Contains(m.Content, "run the same call again") {
				t.Fatalf("tool_result lacks guidance: %q", m.Content)
			}
		}
	}
	if !found {
		t.Fatal("no tool result")
	}
}
