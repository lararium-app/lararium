package surface

// #320 review round 3: audit source confinement on every path, mint/close
// race, panicking trust hook, mid-mint retention, deterministic prune
// tie-break, and settledAt stamping across every terminal path.

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

func TestAuditSourceConfinedOnEveryPath(t *testing.T) {
	cases := []struct {
		name, approval, fallback string
		wantDecision, wantReason string
		wantSource               string
	}{
		{"auto_empty_channel", "auto", "", "allowed", "not_required", "hub"},
		{"empty_approval_unknown_channel", "", "api", "allowed", "not_required", "hub"},
		{"auto_headless_cron_passes", "auto", "cron", "allowed", "not_required", "cron"},
		{"auto_headless_heartbeat_passes", "auto", "heartbeat", "allowed", "not_required", "heartbeat"},
		{"prefix_without_hub_record", "approved:a_missing", "bogus", "allowed", "ok", "hub"},
		{"denied_prefix_without_hub_record", "denied:a_missing", "web", "denied", "disconnected", "web"},
		{"malformed_string", "garbage", "api", "denied", "cancelled", "hub"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj, ok := auditApprovalFor(nil, "s1", tc.approval, tc.fallback)
			if !ok {
				t.Fatal("auditApprovalFor returned ok=false")
			}
			var got auditEnvelope
			if err := json.Unmarshal(obj, &got); err != nil {
				t.Fatal(err)
			}
			want := auditEnvelope{Decision: tc.wantDecision, Reason: tc.wantReason, Source: tc.wantSource}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

// Shutdown that races a failing connector mint must settle the claimed
// card on the closed hub instead of resurrecting a pending card + timer.
func TestShutdownRacingFailedMintSettlesDenied(t *testing.T) {
	hub := setupHub(t, raceStub())
	t.Cleanup(hub.ap.Shutdown)
	id, ch := hub.ap.RegisterOn("main", "mcp__tool__shut", `{}`, true, func(string) {})

	entered := make(chan struct{})
	release := make(chan error, 1)
	hub.SetConnectorTrustHook(func(string) error {
		close(entered)
		return <-release
	})

	code := make(chan int, 1)
	go func() {
		c, _ := hub.ap.ResolveScopeVia("main", id, true, "cli", "phone", "connector")
		code <- c
	}()
	<-entered

	hub.ap.Shutdown()
	release <- errors.New("simulated mint failure during shutdown")

	if c := <-code; c != 500 {
		t.Fatalf("failed mint status = %d, want 500", c)
	}
	select {
	case allow := <-ch:
		if allow {
			t.Fatal("waiter released with allow after failed mint on a closed hub")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was never released; card was resurrected as pending")
	}
	if n := hub.ap.PendingCount("main"); n != 0 {
		t.Fatalf("PendingCount = %d after shutdown-raced mint failure, want 0", n)
	}
	state, reason, source, _, found := hub.ap.Terminal("main", id)
	if !found || state != "denied" || reason != "shutdown" || source != "shutdown" {
		t.Fatalf("Terminal = (%q,%q,%q,%v), want denied/shutdown/shutdown", state, reason, source, found)
	}
}

// A panicking trust hook must roll the claim back like any failed mint
// instead of leaving the card minting (invisible, unprunable) forever.
func TestPanickingTrustHookRollsBack(t *testing.T) {
	hub := setupHub(t, raceStub())
	t.Cleanup(hub.ap.Shutdown)
	id, _ := hub.ap.RegisterOn("main", "mcp__tool__panic", `{}`, true, func(string) {})

	hub.SetConnectorTrustHook(func(string) error { panic("boom") })
	code, err := hub.ap.ResolveScopeVia("main", id, true, "cli", "phone", "connector")
	if code != 500 || err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("panicking hook = (%d, %v), want 500 with a panicked error", code, err)
	}
	if n := hub.ap.PendingCount("main"); n != 1 {
		t.Fatalf("PendingCount = %d after panic rollback, want 1", n)
	}
	if _, _, _, _, found := hub.ap.Terminal("main", id); found {
		t.Fatal("Terminal exposed a record after panic rollback (card must be pending)")
	}

	hub.SetConnectorTrustHook(func(string) error { return nil })
	if code, err := hub.ap.ResolveScopeVia("main", id, true, "cli", "phone", "connector"); code != 200 || err != nil {
		t.Fatalf("retry after panic = (%d, %v), want 200", code, err)
	}
}

// The commit of a slow mint stamps settledAt at commit time, not at the
// pre-hook claim time.
func TestMintCommitRestampsSettledAt(t *testing.T) {
	clk := loop.NewFakeClock(time.Date(2026, 11, 23, 15, 0, 0, 0, time.UTC))
	hub := setupHub(t, raceStub())
	t.Cleanup(hub.ap.Shutdown)
	hub.ap.SetClock(clk)
	id, _ := hub.ap.RegisterOn("main", "mcp__tool__slow", `{}`, true, func(string) {})

	hub.SetConnectorTrustHook(func(string) error {
		clk.Advance(9 * time.Second)
		return nil
	})
	if code, err := hub.ap.ResolveScopeVia("main", id, true, "cli", "phone", "connector"); code != 200 || err != nil {
		t.Fatalf("resolve = (%d, %v), want 200", code, err)
	}
	_, _, _, settledAt, found := hub.ap.Terminal("main", id)
	if !found || !settledAt.Equal(clk.Now()) {
		t.Fatalf("settledAt = %v (found=%v), want commit time %v", settledAt, found, clk.Now())
	}
}

// A card that is mid-mint looks terminal only because of its claim; the
// retention cap must never evict it.
func TestPruneSkipsMintingCard(t *testing.T) {
	clk := loop.NewFakeClock(time.Date(2026, 11, 23, 16, 0, 0, 0, time.UTC))
	hub := setupHub(t, raceStub())
	t.Cleanup(hub.ap.Shutdown)
	hub.ap.SetClock(clk)

	mintID, _ := hub.ap.RegisterOn("main", "mcp__tool__mint", `{}`, true, func(string) {})
	entered := make(chan struct{})
	release := make(chan error, 1)
	hub.SetConnectorTrustHook(func(string) error {
		close(entered)
		return <-release
	})
	code := make(chan int, 1)
	go func() {
		c, _ := hub.ap.ResolveScopeVia("main", mintID, true, "cli", "phone", "connector")
		code <- c
	}()
	<-entered

	var fillIDs []string
	for range maxTerminalEntriesPerSession + 2 {
		clk.Advance(time.Second)
		id, _ := hub.ap.RegisterOn("main", "echo", "args", true, func(string) {})
		if c := hub.ap.ResolveFrom("main", id, true, "web"); c != 200 {
			t.Fatalf("fill resolve = %d, want 200", c)
		}
		fillIDs = append(fillIDs, id)
	}
	clk.Advance(time.Second)
	hub.ap.RegisterOn("main", "echo", "args", true, func(string) {}) // trips the final prune

	if _, _, _, _, found := hub.ap.Terminal("main", fillIDs[0]); found {
		t.Fatal("earliest fill survived; prune did not run, test is vacuous")
	}

	release <- nil
	if c := <-code; c != 200 {
		t.Fatalf("mint commit = %d, want 200", c)
	}
	state, _, _, _, found := hub.ap.Terminal("main", mintID)
	if !found || state != "approved" {
		t.Fatalf("mid-mint card was evicted by prune (found=%v state=%q)", found, state)
	}
}

// Batch settlements stamp one instant on many cards; eviction among ties
// must be deterministic regardless of map iteration order.
func TestPruneTieBreakDeterministic(t *testing.T) {
	const extra = 3
	for round := range 20 {
		clk := loop.NewFakeClock(time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC))
		ap := NewApprovalHub(time.Hour)
		ap.SetClock(clk)

		var ids []string
		for range maxTerminalEntriesPerSession + extra {
			id, _ := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
			ids = append(ids, id)
		}
		ap.DenyAllFor("s1", "cancelled") // one instant for every card
		ap.RegisterOn("s1", "echo", "args", true, func(string) {})

		sorted := append([]string(nil), ids...)
		sort.Strings(sorted)
		for i, id := range sorted {
			_, _, _, _, found := ap.Terminal("s1", id)
			if wantEvicted := i < extra; found == wantEvicted {
				t.Fatalf("round %d: id %s (rank %d) found=%v, want evicted=%v", round, id, i, found, wantEvicted)
			}
		}
		ap.Shutdown()
	}
}

// settledAt is stamped under the hub lock on every terminal path.
func TestSettledAtStampedOnTerminalPaths(t *testing.T) {
	paths := []struct {
		name         string
		telegramOnly bool // card has no live web channel, so MarkUndeliverable settles it
		run          func(clk *loop.FakeClock, ap *ApprovalHub, id string)
	}{
		// The 1h timer armed at registration fires at t0+1h; the test has
		// already advanced 7s, so advance the remainder exactly.
		{"timeout", false, func(clk *loop.FakeClock, _ *ApprovalHub, _ string) { clk.Advance(time.Hour - 7*time.Second) }},
		{"shutdown", false, func(_ *loop.FakeClock, ap *ApprovalHub, _ string) { ap.Shutdown() }},
		{"disconnect", false, func(_ *loop.FakeClock, ap *ApprovalHub, _ string) { ap.DisconnectFor("s1") }},
		{"undeliverable", true, func(_ *loop.FakeClock, ap *ApprovalHub, id string) { ap.MarkUndeliverable(id) }},
		{"cancelled_for", false, func(_ *loop.FakeClock, ap *ApprovalHub, _ string) { ap.MarkCancelledFor("s1", "hub") }},
		{"deny_all_for", false, func(_ *loop.FakeClock, ap *ApprovalHub, _ string) { ap.DenyAllFor("s1", "cancelled") }},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			clk := loop.NewFakeClock(time.Date(2026, 11, 23, 18, 0, 0, 0, time.UTC))
			ap := NewApprovalHub(time.Hour)
			ap.SetClock(clk)
			t.Cleanup(ap.Shutdown)
			if p.telegramOnly {
				ap.SetBridgeLive(func() bool { return true })
			}
			id, _ := ap.RegisterOn("s1", "echo", "args", !p.telegramOnly, func(string) {})
			clk.Advance(7 * time.Second)
			p.run(clk, ap, id)
			want := clk.Now()
			state, _, _, settledAt, found := ap.Terminal("s1", id)
			if !found || state == "pending" {
				t.Fatalf("Terminal = (%q, found=%v), want a terminal record", state, found)
			}
			if !settledAt.Equal(want) {
				t.Fatalf("settledAt = %v, want %v", settledAt, want)
			}
		})
	}
}
