package surface

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func newRPCPolicyClient(t *testing.T, src PolicySource) *testWSClient {
	t.Helper()
	srv, tokStore, ts := newTestRPCServer(t, 2*time.Second)
	if src != nil {
		srv.SetPolicySource(src)
	}
	tok, err := tokStore.Create("rpc-policy-test")
	if err != nil {
		t.Fatal(err)
	}
	return dialAuthenticatedWS(t, ts.URL, tok)
}

func TestRPCPolicyRulesList(t *testing.T) {
	t.Run("nil source returns locked", func(t *testing.T) {
		client := newRPCPolicyClient(t, nil)
		assertRPCErrorReason(t, client.callRPC(t, 1, "policy.rules.list", map[string]any{}), "locked")
	})

	t.Run("happy path returns egress_strict and rules", func(t *testing.T) {
		src := &fakePolicySource{
			egressStrict: true,
			rules: []PolicyRule{
				{Lane: "credential", Pattern: "demo/echo", Verdict: "ask", Always: false, Source: "web"},
			},
		}
		client := newRPCPolicyClient(t, src)
		resp := client.callRPC(t, 2, "policy.rules.list", map[string]any{})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		var out struct {
			EgressStrict bool         `json:"egress_strict"`
			Rules        []PolicyRule `json:"rules"`
		}
		if err := json.Unmarshal(resp.Result, &out); err != nil {
			t.Fatalf("unmarshal result: %v", err)
		}
		if !out.EgressStrict || len(out.Rules) != 1 || out.Rules[0].Pattern != "demo/echo" {
			t.Fatalf("got %+v", out)
		}
	})

	t.Run("nil rules slice encodes as empty array never null", func(t *testing.T) {
		src := &fakePolicySource{rules: nil}
		client := newRPCPolicyClient(t, src)
		resp := client.callRPC(t, 3, "policy.rules.list", map[string]any{})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		if !strings.Contains(string(resp.Result), `"rules":[]`) {
			t.Fatalf("result = %s, want rules:[]", string(resp.Result))
		}
	})

	t.Run("locked, corrupt, and internal errors", func(t *testing.T) {
		src := &fakePolicySource{}
		client := newRPCPolicyClient(t, src)

		src.listErr = ErrPolicyLocked
		assertRPCErrorReason(t, client.callRPC(t, 4, "policy.rules.list", map[string]any{}), "locked")

		src.listErr = ErrPolicyCorrupt
		assertRPCErrorReason(t, client.callRPC(t, 5, "policy.rules.list", map[string]any{}), "policy_corrupt")

		src.listErr = errors.New("unexpected io failure")
		assertRPCErrorReason(t, client.callRPC(t, 6, "policy.rules.list", map[string]any{}), "internal_error")
	})
}

func TestRPCPolicyRulesSet(t *testing.T) {
	t.Run("nil source returns locked before decode", func(t *testing.T) {
		client := newRPCPolicyClient(t, nil)
		assertRPCErrorReason(t, client.callRPC(t, 10, "policy.rules.set", map[string]any{
			"unknown": true,
		}), "locked")
	})

	t.Run("happy path, lane coercion, and nil notes normalized", func(t *testing.T) {
		src := &fakePolicySource{addNotes: []string{"normalized"}}
		client := newRPCPolicyClient(t, src)

		resp := client.callRPC(t, 11, "policy.rules.set", map[string]any{
			"lane":    "egress",
			"pattern": "example.com",
			"verdict": "auto",
		})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		if src.lastLane != "egress" || src.lastPattern != "example.com" || src.lastVerdict != "auto" {
			t.Errorf("AddRule got (%q, %q, %q)", src.lastLane, src.lastPattern, src.lastVerdict)
		}
		var out struct {
			OK    bool     `json:"ok"`
			Notes []string `json:"notes"`
		}
		if err := json.Unmarshal(resp.Result, &out); err != nil || !out.OK || len(out.Notes) != 1 {
			t.Fatalf("result = %s", string(resp.Result))
		}

		// Anything other than "egress" coerces to "credential"; nil notes -> [].
		src.addNotes = nil
		resp2 := client.callRPC(t, 12, "policy.rules.set", map[string]any{
			"lane":    "anything-else",
			"pattern": "demo/echo",
			"verdict": "ask",
		})
		if resp2.Error != nil {
			t.Fatalf("unexpected error: %+v", resp2.Error)
		}
		if src.lastLane != "credential" {
			t.Errorf("coerced lane = %q, want credential", src.lastLane)
		}
		if !strings.Contains(string(resp2.Result), `"notes":[]`) {
			t.Errorf("result = %s, want notes:[]", string(resp2.Result))
		}
	})

	t.Run("unknown params including always rejected with invalid_request", func(t *testing.T) {
		src := &fakePolicySource{}
		client := newRPCPolicyClient(t, src)
		assertRPCErrorReason(t, client.callRPC(t, 13, "policy.rules.set", map[string]any{
			"lane":    "credential",
			"pattern": "demo/echo",
			"verdict": "auto",
			"always":  true,
		}), "invalid_request")
	})

	t.Run("locked, policy_corrupt, and engine invalid_rule message passthrough", func(t *testing.T) {
		src := &fakePolicySource{}
		client := newRPCPolicyClient(t, src)

		src.addErr = ErrPolicyLocked
		assertRPCErrorReason(t, client.callRPC(t, 14, "policy.rules.set", map[string]any{
			"lane": "credential", "pattern": "demo/echo", "verdict": "ask",
		}), "locked")

		src.addErr = ErrPolicyCorrupt
		assertRPCErrorReason(t, client.callRPC(t, 15, "policy.rules.set", map[string]any{
			"lane": "credential", "pattern": "demo/echo", "verdict": "ask",
		}), "policy_corrupt")

		src.addErr = errors.New("pattern must be canonical")
		resp := client.callRPC(t, 16, "policy.rules.set", map[string]any{
			"lane": "credential", "pattern": "bad", "verdict": "ask",
		})
		assertRPCErrorReason(t, resp, "invalid_rule")
		if resp.Error.Message != "pattern must be canonical" {
			t.Errorf("error.message = %q, want %q", resp.Error.Message, "pattern must be canonical")
		}
	})
}

func TestRPCPolicyRulesRemove(t *testing.T) {
	t.Run("gate order: pattern then lane then source", func(t *testing.T) {
		// Source is nil, yet bad pattern must answer not_found first, and bad lane invalid_lane second.
		client := newRPCPolicyClient(t, nil)

		assertRPCErrorReason(t, client.callRPC(t, 20, "policy.rules.remove", map[string]any{
			"pattern": "has space",
			"lane":    "bogus",
		}), "not_found")

		assertRPCErrorReason(t, client.callRPC(t, 21, "policy.rules.remove", map[string]any{
			"pattern": "",
		}), "not_found")

		assertRPCErrorReason(t, client.callRPC(t, 22, "policy.rules.remove", map[string]any{
			"pattern": strings.Repeat("a", 257),
		}), "not_found")

		assertRPCErrorReason(t, client.callRPC(t, 23, "policy.rules.remove", map[string]any{
			"pattern": "demo/echo",
			"lane":    "bogus",
		}), "invalid_lane")

		assertRPCErrorReason(t, client.callRPC(t, 24, "policy.rules.remove", map[string]any{
			"pattern": "demo/echo",
			"lane":    "credential",
		}), "locked")
	})

	t.Run("happy path and error mapping", func(t *testing.T) {
		src := &fakePolicySource{removeOK: true}
		client := newRPCPolicyClient(t, src)

		resp := client.callRPC(t, 25, "policy.rules.remove", map[string]any{
			"pattern": "demo/echo",
		})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		if src.lastLane != "" || src.lastPattern != "demo/echo" {
			t.Errorf("RemoveRule got (%q, %q)", src.lastLane, src.lastPattern)
		}
		var out struct {
			OK      bool `json:"ok"`
			Removed bool `json:"removed"`
		}
		if err := json.Unmarshal(resp.Result, &out); err != nil || !out.OK || !out.Removed {
			t.Fatalf("result = %s", string(resp.Result))
		}

		src.removeOK = false
		resp2 := client.callRPC(t, 26, "policy.rules.remove", map[string]any{
			"pattern": "example.com",
			"lane":    "egress",
		})
		if resp2.Error != nil {
			t.Fatalf("unexpected error: %+v", resp2.Error)
		}
		if src.lastLane != "egress" {
			t.Errorf("lastLane = %q, want egress", src.lastLane)
		}
		if err := json.Unmarshal(resp2.Result, &out); err != nil || !out.OK || out.Removed {
			t.Fatalf("result = %s, want ok=true removed=false", string(resp2.Result))
		}

		src.removeErr = ErrPolicyLocked
		assertRPCErrorReason(t, client.callRPC(t, 27, "policy.rules.remove", map[string]any{
			"pattern": "demo/echo",
		}), "locked")

		src.removeErr = ErrPolicyCorrupt
		assertRPCErrorReason(t, client.callRPC(t, 28, "policy.rules.remove", map[string]any{
			"pattern": "demo/echo",
		}), "policy_corrupt")

		src.removeErr = errors.New("bad engine pattern")
		resp3 := client.callRPC(t, 29, "policy.rules.remove", map[string]any{
			"pattern": "demo/echo",
		})
		assertRPCErrorReason(t, resp3, "invalid_rule")
		if resp3.Error.Message != "bad engine pattern" {
			t.Errorf("error.message = %q, want %q", resp3.Error.Message, "bad engine pattern")
		}
	})
}
