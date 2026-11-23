package surface

import (
	"bytes"
	"encoding/json"
	stdlog "log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

type auditEnvelope struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	Source   string `json:"source"`
}

func decodeAuditEnvelope(t *testing.T, ap *ApprovalHub, sessionID, approval, fallback string) auditEnvelope {
	t.Helper()
	raw, ok := auditApprovalFor(ap, sessionID, approval, fallback)
	if !ok {
		t.Fatalf("auditApprovalFor(%q) returned ok=false", approval)
	}
	var got auditEnvelope
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal audit envelope %q: %v", string(raw), err)
	}
	return got
}

func TestAuditApprovalForHubAuthority(t *testing.T) {
	cases := []struct {
		name         string
		setup        func(t *testing.T) (ap *ApprovalHub, sessionID, approval string)
		want         auditEnvelope
		wantMismatch bool
	}{
		{
			name: "cancel_card_timeout_overrides_approved_prefix",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				ap := NewApprovalHub(time.Hour)
				id, _ := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
				if !ap.CancelCard(id, "timeout", "timer") {
					t.Fatal("CancelCard returned false")
				}
				return ap, "s1", "approved:" + id
			},
			want:         auditEnvelope{Decision: "denied", Reason: "timeout", Source: "timer"},
			wantMismatch: true,
		},
		{
			name: "cancel_all_for_custos_overrides_approved_prefix",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				ap := NewApprovalHub(time.Hour)
				id, _ := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
				ap.CancelAllFor("s1", "custos")
				return ap, "s1", "approved:" + id
			},
			want:         auditEnvelope{Decision: "denied", Reason: "cancelled", Source: "custos"},
			wantMismatch: true,
		},
		{
			name: "settle_custos_locked_overrides_approved_prefix",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				ap := NewApprovalHub(time.Hour)
				id, _ := ap.RegisterCustos("cell1", "gmail", "mail.example:443", "send", time.Hour, nil)
				if n := ap.SettleCustosLocked("custos"); n != 1 {
					t.Fatalf("SettleCustosLocked = %d, want 1", n)
				}
				return ap, "cell1", "approved:" + id
			},
			want:         auditEnvelope{Decision: "denied", Reason: "custos_locked", Source: "custos"},
			wantMismatch: true,
		},
		{
			name: "credential_revoked_overrides_approved_prefix",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				ap := NewApprovalHub(time.Hour)
				id, _ := ap.RegisterCustos("cell1", "gmail", "mail.example:443", "send", time.Hour, nil)
				if n := ap.CancelByCredential("gmail", "cell1"); n != 1 {
					t.Fatalf("CancelByCredential = %d, want 1", n)
				}
				return ap, "cell1", "approved:" + id
			},
			want:         auditEnvelope{Decision: "denied", Reason: "credential_revoked", Source: "custos"},
			wantMismatch: true,
		},
		{
			name: "inverse_race_approved_in_hub_overrides_denied_prefix",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				ap := NewApprovalHub(time.Hour)
				id, _ := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
				if code := ap.ResolveFrom("s1", id, true, "telegram"); code != 200 {
					t.Fatalf("ResolveFrom = %d, want 200", code)
				}
				return ap, "s1", "denied:" + id
			},
			want:         auditEnvelope{Decision: "allowed", Reason: "ok", Source: "telegram"},
			wantMismatch: true,
		},
		{
			name: "explicit_deny_from_web_unchanged",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				ap := NewApprovalHub(time.Hour)
				id, _ := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
				if code := ap.Resolve("s1", id, false); code != 200 {
					t.Fatalf("Resolve = %d, want 200", code)
				}
				return ap, "s1", "denied:" + id
			},
			want:         auditEnvelope{Decision: "denied", Reason: "ok", Source: "web"},
			wantMismatch: false,
		},
		{
			name: "resolve_timeout_with_denied_prefix",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				clk := loop.NewFakeClock(time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC))
				ap := NewApprovalHub(30 * time.Second)
				ap.SetClock(clk)
				id, ch := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
				clk.Advance(30 * time.Second)
				if <-ch {
					t.Fatal("timeout must deny")
				}
				return ap, "s1", "denied:" + id
			},
			want:         auditEnvelope{Decision: "denied", Reason: "timeout", Source: "timer"},
			wantMismatch: false,
		},
		{
			name: "unknown_hub_cause_fails_safe_to_cancelled",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				ap := NewApprovalHub(time.Hour)
				id, _ := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
				ap.DenyAllFor("s1", "fabricated_unknown_cause")
				return ap, "s1", "denied:" + id
			},
			want:         auditEnvelope{Decision: "denied", Reason: "cancelled", Source: "hub"},
			wantMismatch: false,
		},
		{
			name: "unknown_hub_cause_with_approved_prefix_fails_safe",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				ap := NewApprovalHub(time.Hour)
				id, _ := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
				ap.DenyAllFor("s1", "fabricated_unknown_cause")
				return ap, "s1", "approved:" + id
			},
			want:         auditEnvelope{Decision: "denied", Reason: "cancelled", Source: "hub"},
			wantMismatch: true,
		},
		{
			name: "fallback_denied_no_approver",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				return NewApprovalHub(time.Hour), "s1", "denied:no-approver"
			},
			want:         auditEnvelope{Decision: "denied", Reason: "disconnected", Source: "web"},
			wantMismatch: false,
		},
		{
			name: "fallback_approved_unknown_id",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				return NewApprovalHub(time.Hour), "s1", "approved:unknown-id"
			},
			want:         auditEnvelope{Decision: "allowed", Reason: "ok", Source: "web"},
			wantMismatch: false,
		},
		{
			name: "fallback_approved_none",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				return NewApprovalHub(time.Hour), "s1", "approved:none"
			},
			want:         auditEnvelope{Decision: "allowed", Reason: "ok", Source: "web"},
			wantMismatch: false,
		},
		{
			name: "fallback_auto",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				return NewApprovalHub(time.Hour), "s1", "auto"
			},
			want:         auditEnvelope{Decision: "allowed", Reason: "not_required", Source: "web"},
			wantMismatch: false,
		},
		{
			name: "fallback_empty",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				return NewApprovalHub(time.Hour), "s1", ""
			},
			want:         auditEnvelope{Decision: "allowed", Reason: "not_required", Source: "web"},
			wantMismatch: false,
		},
		{
			name: "fallback_nil_hub_approved",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				return nil, "s1", "approved:a_1234567890"
			},
			want:         auditEnvelope{Decision: "allowed", Reason: "ok", Source: "web"},
			wantMismatch: false,
		},
		{
			name: "fallback_nil_hub_denied",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				return nil, "s1", "denied:a_1234567890"
			},
			want:         auditEnvelope{Decision: "denied", Reason: "disconnected", Source: "web"},
			wantMismatch: false,
		},
		{
			name: "pending_card_uses_fallback",
			setup: func(t *testing.T) (*ApprovalHub, string, string) {
				t.Helper()
				ap := NewApprovalHub(time.Hour)
				id, _ := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
				return ap, "s1", "denied:" + id
			},
			want:         auditEnvelope{Decision: "denied", Reason: "disconnected", Source: "web"},
			wantMismatch: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			origOut := stdlog.Writer()
			stdlog.SetOutput(&logBuf)
			defer stdlog.SetOutput(origOut)

			ap, sessionID, approval := tc.setup(t)
			if ap != nil {
				t.Cleanup(ap.Shutdown)
			}
			got := decodeAuditEnvelope(t, ap, sessionID, approval, "web")
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}

			logged := logBuf.String()
			hasMismatch := strings.Contains(logged, "surface: audit authority mismatch ")
			if hasMismatch != tc.wantMismatch {
				t.Fatalf("mismatch warning logged = %v, want %v (log=%q)", hasMismatch, tc.wantMismatch, logged)
			}
		})
	}
}

func TestMapAuditReasonFrozenEnum(t *testing.T) {
	allowed := map[string]bool{
		"ok":            true,
		"timeout":       true,
		"disconnected":  true,
		"shutdown":      true,
		"undeliverable": true,
		"cancelled":     true,
		"custos_locked": true,
		//nolint:goconst // frozen enum member spelled per SURFACE-SPEC §6/A8
		"credential_revoked": true,
		"not_required":       true,
	}
	cases := map[string]string{ //nolint:gosec // G101: frozen audit-reason enum literals, not credentials
		"ok":                 "ok",
		"denied":             "ok",
		"timed_out":          "timeout",
		"timeout":            "timeout",
		"shutdown":           "shutdown",
		"undeliverable":      "undeliverable",
		"disconnected":       "disconnected",
		"cancelled":          "cancelled",
		"flow_gone":          "cancelled",
		"too_many_asks":      "cancelled",
		"custos_locked":      "custos_locked",
		"credential_revoked": "credential_revoked",
		"":                   "cancelled",
		"arbitrary_cause":    "cancelled",
	}
	for in, want := range cases {
		// #320: verdict-shaped reasons ("ok", "denied") map to "ok" only
		// when they agree with the record state; every other member is a
		// terminal cause carried on a denied record.
		state := "denied"
		if in == "ok" {
			state = "approved"
		}
		got := mapAuditReason(state, in)
		if got != want {
			t.Errorf("mapAuditReason(%q, %q) = %q, want %q", state, in, got, want)
		}
		if !allowed[got] {
			t.Errorf("mapAuditReason(%q) = %q, not in frozen enum", in, got)
		}
	}
}

func TestApprovalHubTerminalSnapshot(t *testing.T) {
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)
	ap := NewApprovalHub(time.Hour)
	ap.SetClock(clk)

	// Nil hub and unknown id report found=false.
	var nilHub *ApprovalHub
	if _, _, _, _, found := nilHub.Terminal("s1", "a_none"); found {
		t.Fatal("nil hub Terminal must return found=false")
	}
	if _, _, _, _, found := ap.Terminal("s1", "a_unknown"); found {
		t.Fatal("unknown id Terminal must return found=false")
	}

	// Pending approval reports found=false.
	id1, _ := ap.RegisterOn("s1", "echo", "a", true, func(string) {})
	id2, _ := ap.RegisterOn("s1", "echo", "b", true, func(string) {})
	if _, _, _, _, found := ap.Terminal("s1", id1); found {
		t.Fatal("pending approval Terminal must return found=false")
	}

	// Advance clock and settle first approval.
	clk.Advance(5 * time.Second)
	t1 := clk.Now()
	if code := ap.ResolveFrom("s1", id1, true, "telegram"); code != 200 {
		t.Fatalf("ResolveFrom = %d, want 200", code)
	}
	state, reason, source, settledAt1, found := ap.Terminal("s1", id1)
	if !found {
		t.Fatal("settled approval Terminal must return found=true")
	}
	if state != "approved" || reason != "ok" || source != "telegram" {
		t.Fatalf("got (%q,%q,%q), want (approved,ok,telegram)", state, reason, source)
	}
	if settledAt1.IsZero() || !settledAt1.Equal(t1) {
		t.Fatalf("settledAt1 = %v, want %v", settledAt1, t1)
	}
	// Wrong session must not match.
	if _, _, _, _, foundOther := ap.Terminal("s2", id1); foundOther {
		t.Fatal("Terminal with foreign session must return found=false")
	}

	// Advance clock and settle second approval; settledAt must be strictly later.
	clk.Advance(7 * time.Second)
	t2 := clk.Now()
	if !ap.CancelCard(id2, "flow_gone", "custos") {
		t.Fatal("CancelCard returned false")
	}
	state, reason, source, settledAt2, found := ap.Terminal("s1", id2)
	if !found {
		t.Fatal("cancelled approval Terminal must return found=true")
	}
	if state != "denied" || reason != "flow_gone" || source != "custos" {
		t.Fatalf("got (%q,%q,%q), want (denied,flow_gone,custos)", state, reason, source)
	}
	if settledAt2.IsZero() || !settledAt2.Equal(t2) || !settledAt2.After(settledAt1) {
		t.Fatalf("settledAt2 = %v (settledAt1=%v), want monotonic %v", settledAt2, settledAt1, t2)
	}

	// Immediate no-channel denial on RegisterOn must also stamp settledAt.
	clk.Advance(3 * time.Second)
	t3 := clk.Now()
	id3, _ := ap.RegisterOn("s1", "echo", "c", false, func(string) {})
	state, reason, source, settledAt3, found := ap.Terminal("s1", id3)
	if !found || state != "denied" || reason != "disconnected" || source != "hub" {
		t.Fatalf("no-channel Terminal = (%q,%q,%q,%v,%v)", state, reason, source, settledAt3, found)
	}
	if !settledAt3.Equal(t3) || !settledAt3.After(settledAt2) {
		t.Fatalf("settledAt3 = %v, want %v", settledAt3, t3)
	}
}

func TestApprovalHubTerminalAtomicConcurrentRead(t *testing.T) {
	t0 := time.Date(2026, 11, 23, 11, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)
	ap := NewApprovalHub(time.Hour)
	ap.SetClock(clk)

	id, _ := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
	clk.Advance(10 * time.Second)
	wantTime := clk.Now()

	start := make(chan struct{})
	var wg sync.WaitGroup
	var observed atomic.Int64
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 200 {
				state, reason, source, settledAt, found := ap.Terminal("s1", id)
				if !found {
					continue
				}
				observed.Add(1)
				if state != "approved" || reason != "ok" || source != "telegram" || !settledAt.Equal(wantTime) {
					t.Errorf("torn Terminal snapshot: (%q,%q,%q,%v)", state, reason, source, settledAt)
					return
				}
			}
		}()
	}

	close(start)
	if code := ap.ResolveFrom("s1", id, true, "telegram"); code != 200 {
		t.Fatalf("ResolveFrom = %d, want 200", code)
	}
	wg.Wait()
	if observed.Load() == 0 {
		t.Fatal("no reader ever observed the terminal snapshot; test was vacuous")
	}
}
