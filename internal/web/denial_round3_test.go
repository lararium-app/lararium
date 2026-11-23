package web

// #320 review round 3: the browse act gate must route refused approvals
// exactly like loop.denialGate, and the routing must be pinned end to end
// through enforceActsGate (not just the helper).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func auditReturning(reason string) DenialAudit {
	return func(string) (json.RawMessage, bool) {
		return json.RawMessage(`{"decision":"denied","reason":"` + reason + `","source":"hub"}`), true
	}
}

// Every non-verdict hub cause names its real gate; only an explicit
// verdict blames the user.
func TestBrowseDenialGateRouting(t *testing.T) {
	hubCases := map[string]string{
		"ok":                 "denied",
		"denied":             "denied",
		"timeout":            "timeout",
		"timed_out":          "timeout",
		"disconnected":       "disconnected",
		"undeliverable":      "disconnected",
		"custos_locked":      "custos_locked",
		"credential_revoked": "custos_locked",
		"cancelled":          "cancelled",
		"shutdown":           "cancelled",
		"something_else":     "denied",
	}
	for reason, want := range hubCases {
		ctx := ContextWithDenialAudit(context.Background(), auditReturning(reason))
		if got := browseDenialGate(ctx, "a_1"); got != want {
			t.Errorf("hub reason %q -> gate %q, want %q", reason, got, want)
		}
	}

	// no-approver wins over the hub record, as in loop.denialGate.
	ctx := ContextWithDenialAudit(context.Background(), auditReturning("disconnected"))
	if got := browseDenialGate(ctx, "no-approver"); got != "no-approver" {
		t.Errorf("no-approver gate = %q", got)
	}

	// A hook that cannot find the record falls back to "denied".
	missing := ContextWithDenialAudit(context.Background(), func(string) (json.RawMessage, bool) { return nil, false })
	if got := browseDenialGate(missing, "a_1"); got != "denied" {
		t.Errorf("missing record gate = %q, want denied", got)
	}

	// Substring fallback (no hook on the context).
	fallback := map[string]string{
		"a_timeout":            "timeout",
		"a_timed_out":          "timeout",
		"a_disconnected":       "disconnected",
		"a_undeliverable":      "disconnected",
		"a_custos_locked":      "custos_locked",
		"a_credential_revoked": "custos_locked",
		"a_cancelled":          "cancelled",
		"a_shutdown":           "cancelled",
		"a_plain":              "denied",
	}
	for id, want := range fallback {
		if got := browseDenialGate(context.Background(), id); got != want {
			t.Errorf("fallback id %q -> gate %q, want %q", id, got, want)
		}
	}
}

// End to end through enforceActsGate: a refused act card whose hub record
// names a non-verdict cause hands the model guidance that does not blame
// the user, and a real denial still does.
func TestEnforceActsGateNamesRealCause(t *testing.T) {
	acts := []Act{{Kind: "click", Selector: "#go", Intent: "press the go button"}}
	for _, tc := range []struct{ reason, gate string }{
		{"denied", "denied"},
		{"cancelled", "cancelled"},
		{"shutdown", "cancelled"},
		{"timeout", "timeout"},
		{"custos_locked", "custos_locked"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			approver := func(string, string) (bool, string) { return false, "a_9" }
			b := NewBrowser(t.TempDir(), Config{Enabled: true}, WithApprover(approver))
			ctx := ContextWithDenialAudit(context.Background(), auditReturning(tc.reason))

			got, refused := b.enforceActsGate(ctx, "https://example.com", "https://example.com/p", "2026-11-23", acts)
			if !refused {
				t.Fatal("refused approval must stop the act")
			}
			if want := denialGuidance(tc.gate); got != want {
				t.Fatalf("guidance for hub reason %q = %q, want gate %q guidance", tc.reason, got, tc.gate)
			}
			if blamesUser := strings.Contains(got, "the user denied"); blamesUser != (tc.gate == "denied") {
				t.Fatalf("blamesUser = %v for hub reason %q", blamesUser, tc.reason)
			}
		})
	}
}
