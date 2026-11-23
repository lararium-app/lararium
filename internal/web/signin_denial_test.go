package web

import (
	"context"
	"strings"
	"testing"
)

// The session_login tool must name the real cause of a refused sign-in card.
// An approval id from the hub is opaque (a_<rand>), so the cause comes from
// the hub terminal record; only an explicit verdict may blame the user.

func runLoginDenied(t *testing.T, ctx context.Context, approvalID string) (string, *SigninManager) {
	t.Helper()
	origLookPath := execLookPath
	t.Cleanup(func() { execLookPath = origLookPath })
	execLookPath = func(file string) (string, error) { return "/usr/bin/" + file, nil }

	w, mgr, _ := setupTestToolEnv(t)
	passLoginGuard(t)
	cfg := DefaultConfig()
	cfg.Signin.NoVNCDir = t.TempDir()

	refuse := func(string, string) (bool, string) { return false, approvalID }
	tool := SessionLoginTool(w.Home(), cfg,
		WithLoginManager(mgr),
		WithLoginWallet(w),
		WithLoginApprover(refuse),
	)
	res, err := tool.Run(ctx, `{"origin": "https://carrier.example.com"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res, mgr
}

func TestSessionLoginDenialNamesRealCause(t *testing.T) {
	for _, tc := range []struct {
		reason string
		gate   string
	}{
		{"denied", "denied"},
		{"something_else", "denied"},
		{"timeout", "timeout"},
		{"timed_out", "timeout"},
		{"disconnected", "disconnected"},
		{"undeliverable", "disconnected"},
		{"custos_locked", "custos_locked"},
		{"credential_revoked", "custos_locked"},
		{"cancelled", "cancelled"},
		{"shutdown", "cancelled"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			ctx := ContextWithDenialAudit(context.Background(), auditReturning(tc.reason))
			// A real hub id carries no hint of the cause.
			res, mgr := runLoginDenied(t, ctx, "a_01HXYZ")

			if want := loginDenialGuidance(tc.gate); res != want {
				t.Fatalf("hub reason %q: got %q, want gate %q guidance %q", tc.reason, res, tc.gate, want)
			}
			if blamesUser := strings.Contains(res, "the user denied"); blamesUser != (tc.gate == "denied") {
				t.Fatalf("hub reason %q: blamesUser = %v in %q", tc.reason, blamesUser, res)
			}
			if mgr.State() != SigninIdle {
				t.Fatalf("mgr state = %s, want Idle (window never opens on a refusal)", mgr.State())
			}
			if tok := mgr.Token(); tok != "" {
				t.Fatalf("pre-minted token = %q, want cleared on every refusal", tok)
			}
		})
	}
}

// A hook that cannot find the record falls back to the user-verdict text,
// the same as the act gate.
func TestSessionLoginDenialMissingRecord(t *testing.T) {
	ctx := ContextWithDenialAudit(context.Background(), nil)
	res, _ := runLoginDenied(t, ctx, "a_01HXYZ")
	if want := loginDenialGuidance("denied"); res != want {
		t.Fatalf("got %q, want %q", res, want)
	}
}

// Without a hook on the context the approval-id substring heuristics still
// apply (standalone use: tests, the repl), and now cover every gate.
func TestSessionLoginDenialSubstringFallback(t *testing.T) {
	for id, gate := range map[string]string{
		"a_timeout":            "timeout",
		"a_disconnected":       "disconnected",
		"a_custos_locked":      "custos_locked",
		"a_credential_revoked": "custos_locked",
		"a_cancelled":          "cancelled",
		"a_shutdown":           "cancelled",
		"denied:user":          "denied",
	} {
		res, _ := runLoginDenied(t, context.Background(), id)
		if want := loginDenialGuidance(gate); res != want {
			t.Errorf("id %q: got %q, want gate %q", id, res, gate)
		}
	}
}

// Every non-verdict gate must read as a non-verdict: it names its cause and
// never claims the user denied the card.
func TestLoginDenialGuidanceText(t *testing.T) {
	for gate, mustHave := range map[string]string{
		"timeout":       "timed out",
		"no-approver":   "no approver is wired",
		"disconnected":  "no approver channel",
		"custos_locked": "custody is locked",
		"cancelled":     "cancelled before a user verdict",
	} {
		got := loginDenialGuidance(gate)
		if !strings.Contains(got, mustHave) {
			t.Errorf("gate %q: %q lacks %q", gate, got, mustHave)
		}
		if strings.Contains(got, "the user denied") {
			t.Errorf("gate %q blames the user: %q", gate, got)
		}
	}
	if got := loginDenialGuidance("denied"); !strings.Contains(got, "the user denied tool session_login") {
		t.Errorf("explicit denial lost its wording: %q", got)
	}
}
