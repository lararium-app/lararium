package surface

// #320 review round 2: state-agreeing verdict reasons, the frozen source
// allowlist, fail-closed malformed approval strings, mid-mint audit
// invisibility, and settle-ordered terminal retention.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

func TestMapAuditReasonRequiresStateAgreement(t *testing.T) {
	cases := []struct {
		state, reason, want string
	}{
		{"approved", "ok", "ok"},
		{"denied", "denied", "ok"},
		{"denied", "ok", "cancelled"},       // CancelCard(id, "ok") misuse
		{"approved", "denied", "cancelled"}, // inconsistent pair
		{"approved", "timeout", "timeout"},
		{"denied", "timed_out", "timeout"},
		{"denied", "custos_locked", "custos_locked"},
		{"denied", "credential_revoked", "credential_revoked"},
		{"denied", "shutdown", "shutdown"},
		{"denied", "undeliverable", "undeliverable"},
		{"denied", "disconnected", "disconnected"},
		{"denied", "flow_gone", "cancelled"},
		{"denied", "fabricated_unknown_cause", "cancelled"},
		{"", "ok", "cancelled"},
	}
	for _, tc := range cases {
		if got := mapAuditReason(tc.state, tc.reason); got != tc.want {
			t.Errorf("mapAuditReason(%q,%q) = %q, want %q", tc.state, tc.reason, got, tc.want)
		}
	}
}

func TestMapAuditSourceFrozenEnum(t *testing.T) {
	for _, src := range []string{"repl", "web", "telegram", "ctl", "custos", "timer", "shutdown", "hub", "cron", "goal", "heartbeat"} {
		if got := mapAuditSource(src); got != src {
			t.Errorf("mapAuditSource(%q) = %q, want pass-through", src, got)
		}
	}
	for _, src := range []string{"listener_closed", "server_closed", "surface", "", "Web", "fabricated"} {
		if got := mapAuditSource(src); got != "hub" {
			t.Errorf("mapAuditSource(%q) = %q, want hub", src, got)
		}
	}
}

func TestAuditApprovalForUnknownSourceMappedToHub(t *testing.T) {
	clk := loop.NewFakeClock(time.Date(2026, 11, 23, 13, 0, 0, 0, time.UTC))
	ap := NewApprovalHub(time.Hour)
	ap.SetClock(clk)
	t.Cleanup(ap.Shutdown)
	id, _ := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
	if !ap.CancelCard(id, "timeout", "listener_closed") {
		t.Fatal("CancelCard did not claim the card")
	}
	obj, ok := auditApprovalFor(ap, "s1", "denied:"+id, "web")
	if !ok {
		t.Fatal("auditApprovalFor returned ok=false")
	}
	got := string(obj)
	if !strings.Contains(got, `"reason":"timeout"`) || !strings.Contains(got, `"source":"hub"`) {
		t.Fatalf("envelope = %s, want timeout reason and hub-mapped source", got)
	}
	if strings.Contains(got, "listener_closed") {
		t.Fatalf("raw unenumerated source leaked into envelope: %s", got)
	}
}

func TestAuditApprovalForMalformedStringFailsClosed(t *testing.T) {
	obj, ok := auditApprovalFor(nil, "s1", "not-an-approval-string", "web")
	if !ok {
		t.Fatal("auditApprovalFor returned ok=false")
	}
	got := string(obj)
	if !strings.Contains(got, `"decision":"denied"`) || !strings.Contains(got, `"reason":"cancelled"`) {
		t.Fatalf("envelope = %s, want fail-closed denied/cancelled", got)
	}
}

// A connector-mint claim must be invisible to the audit accessor until the
// mint commits, and invisible again (pending) after a rollback (#320 A8:
// no speculative terminal state in the ledger).
func TestTerminalHidesMidMintClaim(t *testing.T) {
	hub := setupHub(t, raceStub())
	id, _ := hub.ap.RegisterOn("main", "mcp__tool__vis", `{}`, true, func(string) {})

	entered := make(chan struct{})
	release := make(chan error, 1)
	hub.SetConnectorTrustHook(func(string) error {
		close(entered)
		return <-release
	})

	codeA := make(chan int, 1)
	go func() {
		code, _ := hub.ap.ResolveScopeVia("main", id, true, "cli", "phone", "connector")
		codeA <- code
	}()
	<-entered

	if _, _, _, _, found := hub.ap.Terminal("main", id); found {
		t.Fatal("Terminal exposed a mid-mint provisional claim")
	}

	release <- errors.New("simulated mint failure")
	if code := <-codeA; code != 500 {
		t.Fatalf("failed mint status = %d, want 500", code)
	}
	if _, _, _, _, found := hub.ap.Terminal("main", id); found {
		t.Fatal("Terminal exposed state after mint rollback (card is pending again)")
	}

	// A clean re-resolve commits the claim and becomes auditable.
	hub.SetConnectorTrustHook(func(string) error { return nil })
	if code, _ := hub.ap.ResolveScopeVia("main", id, true, "cli", "phone", "connector"); code != 200 {
		t.Fatalf("committed mint status = %d, want 200", code)
	}
	state, reason, _, settledAt, found := hub.ap.Terminal("main", id)
	if !found || state != "approved" || reason != "ok" || settledAt.IsZero() {
		t.Fatalf("committed Terminal = (%q,%q,%v,%v)", state, reason, settledAt, found)
	}
}

// Terminal retention must evict by settle time: a card parked long before
// its verdict is the newest history, not the oldest (#320 M3).
func TestPruneEvictsBySettleTimeNotCreation(t *testing.T) {
	clk := loop.NewFakeClock(time.Date(2026, 11, 23, 14, 0, 0, 0, time.UTC))
	ap := NewApprovalHub(time.Hour)
	ap.SetClock(clk)
	t.Cleanup(ap.Shutdown)

	parkedID, _ := ap.RegisterOn("s1", "echo", "args", true, func(string) {})

	var fillIDs []string
	for range maxTerminalEntriesPerSession {
		id, _ := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
		if code := ap.ResolveFrom("s1", id, true, "web"); code != 200 {
			t.Fatalf("fill resolve = %d, want 200", code)
		}
		fillIDs = append(fillIDs, id)
		clk.Advance(time.Second)
	}

	// Parked card settles LAST despite being created FIRST.
	clk.Advance(10 * time.Minute)
	if !ap.CancelCard(parkedID, "cancelled", "hub") {
		t.Fatal("parked card was not cancellable")
	}

	// One more registration trips the retention cap: the eviction victim
	// must be the earliest-SETTLED fill, never the late-settling parked
	// card.
	triggerID, _ := ap.RegisterOn("s1", "echo", "args", true, func(string) {})

	if _, _, _, _, found := ap.Terminal("s1", parkedID); !found {
		t.Fatal("late-settling parked card was evicted (createdAt-ordered prune?)")
	}
	if _, _, _, _, found := ap.Terminal("s1", fillIDs[0]); found {
		t.Fatal("earliest-settled fill was not evicted; prune kept too much")
	}
	if _, _, _, _, found := ap.Terminal("s1", triggerID); found {
		t.Fatal("trigger card must remain pending")
	}
}
