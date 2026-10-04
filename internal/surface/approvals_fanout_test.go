package surface

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// recordingFanout captures hub transition events (NUNTIUS-SPEC §7.1).
type recordingFanout struct {
	mu       sync.Mutex
	pending  [][3]string // id, session, name
	terminal [][5]string // id, session, state, reason, source
}

func (f *recordingFanout) ApprovalPending(id, sessionID, name, argsSummary string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, [3]string{id, sessionID, name})
}

func (f *recordingFanout) ApprovalTerminal(id, sessionID, state, reason, source string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminal = append(f.terminal, [5]string{id, sessionID, state, reason, source})
}

func (f *recordingFanout) terminals() [][5]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][5]string(nil), f.terminal...)
}

func (f *recordingFanout) pendingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}

func TestRegisterNoLiveChannelDeniesImmediately(t *testing.T) {
	// V16 (web-only half): zero SSE listeners, no bridge → the
	// presence rule denies at creation (S6/V6b preserved).
	ap := NewApprovalHub(time.Minute)
	fan := &recordingFanout{}
	ap.SetFanout(fan)

	id, decision := ap.RegisterOn("s1", "echo", "args", false, func(string) {
		t.Fatal("onEvent must not fire when no channel is live")
	})
	if got := <-decision; got {
		t.Fatal("decision must be false")
	}
	if r := ap.Reason("s1", id); r != "disconnected" {
		t.Fatalf("reason = %q", r)
	}
	if s := ap.Source("s1", id); s != "hub" {
		t.Fatalf("source = %q, want hub", s)
	}
	term := fan.terminals()
	if len(term) != 1 || term[0][4] != "hub" {
		t.Fatalf("terminals = %v", term)
	}
}

func TestRegisterBridgeOnlyBindsBridgeTimeout(t *testing.T) {
	// V16: Telegram-only approval binds nuntius.approval_timeout, not
	// serve.approval_timeout.
	ap := NewApprovalHub(time.Hour)
	ap.SetBridgeTimeout(80 * time.Millisecond)
	ap.SetBridgeLive(func() bool { return true })
	fan := &recordingFanout{}
	ap.SetFanout(fan)

	fired := make(chan string, 1)
	id, decision := ap.RegisterOn("s1", "echo", "args", false, func(aid string) { fired <- aid })
	if <-fired != id {
		t.Fatal("onEvent must fire for a bridge-live approval")
	}
	select {
	case got := <-decision:
		if got {
			t.Fatal("timeout decision must be false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge-only approval did not time out on the bridge timeout")
	}
	if r := ap.Reason("s1", id); r != "timed_out" {
		t.Fatalf("reason = %q", r)
	}
	if s := ap.Source("s1", id); s != "timer" {
		t.Fatalf("source = %q, want timer (A2)", s)
	}
	if fan.pendingCount() != 1 {
		t.Fatalf("pending fan-out count = %d, want 1", fan.pendingCount())
	}
}

func TestRegisterWebWinsTimeoutTie(t *testing.T) {
	// A6: both channels live → serve.approval_timeout wins ties.
	ap := NewApprovalHub(80 * time.Millisecond)
	ap.SetBridgeTimeout(time.Hour)
	ap.SetBridgeLive(func() bool { return true })

	_, decision := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
	select {
	case got := <-decision:
		if got {
			t.Fatal("timeout decision must be false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dual-channel approval did not use the serve timeout")
	}
}

func TestDisconnectForKeepsApprovalWithLiveCard(t *testing.T) {
	// A1 refinement: the web listener drops, but a live bridge with a
	// live card keeps the approval pending.
	ap := NewApprovalHub(time.Hour)
	ap.SetBridgeLive(func() bool { return true })
	id, decision := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
	ap.DisconnectFor("s1")
	select {
	case <-decision:
		t.Fatal("approval with a live Telegram card must survive web disconnect")
	case <-time.After(150 * time.Millisecond):
	}
	// A7: with the web listener already gone, the card dying leaves no
	// live approver — MarkUndeliverable resolves denied:undeliverable.
	ap.MarkUndeliverable(id)
	select {
	case got := <-decision:
		if got {
			t.Fatal("decision must be false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("undeliverable with no live approver did not resolve")
	}
	if r := ap.Reason("s1", id); r != "undeliverable" {
		t.Fatalf("reason = %q, want undeliverable", r)
	}
}

func TestDisconnectForAfterCardDeadDeniesDisconnected(t *testing.T) {
	// §7.6(c) ordering: card dies while web is live (card_dead flag,
	// approval rides), THEN the web listener drops — no live approver
	// remains for this approval, so it resolves denied:disconnected
	// immediately, never the full timer.
	ap := NewApprovalHub(time.Hour)
	ap.SetBridgeLive(func() bool { return true })
	id, decision := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
	ap.MarkUndeliverable(id)
	if ap.PendingCount("s1") != 1 {
		t.Fatal("card_dead with live web must stay pending")
	}
	ap.DisconnectFor("s1")
	select {
	case got := <-decision:
		if got {
			t.Fatal("decision must be false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("card_dead approval did not resolve on web disconnect")
	}
	if r := ap.Reason("s1", id); r != "disconnected" {
		t.Fatalf("reason = %q, want disconnected", r)
	}
}

func TestMarkUndeliverableSoleChannelDenies(t *testing.T) {
	// V8(a): Telegram sole channel → immediate denied:undeliverable,
	// source hub.
	ap := NewApprovalHub(time.Hour)
	ap.SetBridgeLive(func() bool { return true })
	fan := &recordingFanout{}
	ap.SetFanout(fan)

	id, decision := ap.RegisterOn("s1", "echo", "args", false, func(string) {})
	ap.MarkUndeliverable(id)
	select {
	case got := <-decision:
		if got {
			t.Fatal("decision must be false")
		}
	case <-time.After(time.Second):
		t.Fatal("sole-channel undeliverable must resolve immediately")
	}
	if r, s := ap.Reason("s1", id), ap.Source("s1", id); r != "undeliverable" || s != "hub" {
		t.Fatalf("reason/source = %q/%q, want undeliverable/hub (A4/A2)", r, s)
	}
	term := fan.terminals()
	if len(term) != 1 || term[0][3] != "undeliverable" || term[0][4] != "hub" {
		t.Fatalf("terminals = %v", term)
	}
}

func TestMarkUndeliverableWithWebRidesTimer(t *testing.T) {
	// V8(b): an SSE listener is also live → NOT denied; card_dead set,
	// the web resolution still wins.
	ap := NewApprovalHub(time.Hour)
	ap.SetBridgeLive(func() bool { return true })
	id, _ := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
	ap.MarkUndeliverable(id)
	if ap.PendingCount("s1") != 1 {
		t.Fatal("dual-channel undeliverable must stay pending")
	}
	if code := ap.Resolve("s1", id, true); code != 200 {
		t.Fatalf("web resolve after card_dead: code = %d", code)
	}
}

func TestResolveFromAttributionAndFirstWins(t *testing.T) {
	// §7.2 race: first click wins; attribution follows the deciding
	// surface (A2).
	ap := NewApprovalHub(time.Hour)
	fan := &recordingFanout{}
	ap.SetFanout(fan)

	id, decision := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
	if code := ap.ResolveFrom("s1", id, true, "telegram"); code != 200 {
		t.Fatalf("first resolve code = %d", code)
	}
	if !<-decision {
		t.Fatal("decision must be true")
	}
	if code := ap.Resolve("s1", id, false); code != 410 {
		t.Fatalf("losing resolve code = %d, want 410", code)
	}
	if r, s := ap.Reason("s1", id), ap.Source("s1", id); r != "ok" || s != "telegram" {
		t.Fatalf("reason/source = %q/%q, want ok/telegram", r, s)
	}
	if term := fan.terminals(); len(term) != 1 {
		t.Fatalf("terminal fan-out count = %d, want exactly 1", len(term))
	}
}

func TestCancelAllForResolvesPending(t *testing.T) {
	// A5: cancel denies the turn's pending approvals with cause
	// "cancelled" before the turn's own terminal event.
	ap := NewApprovalHub(time.Hour)
	fan := &recordingFanout{}
	ap.SetFanout(fan)

	id, decision := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
	ap.CancelAllFor("s1", "web")
	if got := <-decision; got {
		t.Fatal("decision must be false")
	}
	if r := ap.Reason("s1", id); r != "cancelled" {
		t.Fatalf("reason = %q, want cancelled", r)
	}
	term := fan.terminals()
	if len(term) != 1 || term[0][3] != "cancelled" {
		t.Fatalf("terminals = %v", term)
	}
}

func TestDenyAllAllShutdown(t *testing.T) {
	// V12 hub side: SIGTERM resolves every pending approval
	// denied:shutdown with source "shutdown" (A2).
	ap := NewApprovalHub(time.Hour)
	id1, d1 := ap.RegisterOn("s1", "echo", "a", true, func(string) {})
	id2, d2 := ap.RegisterOn("s2", "echo", "b", true, func(string) {})
	ap.DenyAllAll("shutdown", "shutdown")
	if <-d1 || <-d2 {
		t.Fatal("shutdown decisions must be false")
	}
	for _, pair := range [][2]string{{"s1", id1}, {"s2", id2}} {
		if r, s := ap.Reason(pair[0], pair[1]), ap.Source(pair[0], pair[1]); r != "shutdown" || s != "shutdown" {
			t.Fatalf("%s: reason/source = %q/%q", pair[0], r, s)
		}
	}
}

func TestAuditApprovalForMapping(t *testing.T) {
	// SURFACE-SPEC §6 audit object from the hub's terminal record.
	ap := NewApprovalHub(time.Hour)
	id, _ := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
	ap.ResolveFrom("s1", id, true, "telegram")

	decode := func(t *testing.T, approval string) (string, string, string) {
		t.Helper()
		obj, ok := auditApprovalFor(ap, "s1", approval, "web")
		if !ok {
			t.Fatal("audit object must be present")
		}
		var got struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
			Source   string `json:"source"`
		}
		if err := json.Unmarshal(obj, &got); err != nil {
			t.Fatal(err)
		}
		return got.Decision, got.Reason, got.Source
	}

	if d, r, s := decode(t, "auto"); d != "allowed" || r != "not_required" || s != "web" {
		t.Fatalf("auto: %q/%q/%q", d, r, s)
	}
	if d, r, s := decode(t, "approved:"+id); d != "allowed" || r != "ok" || s != "telegram" {
		t.Fatalf("approved: %q/%q/%q", d, r, s)
	}
	if d, r, s := decode(t, "denied:no-approver"); d != "denied" || r != "disconnected" || s != "web" {
		t.Fatalf("no-approver: %q/%q/%q", d, r, s)
	}
}

func TestDisconnectForLiveTelegramResolves(t *testing.T) {
	// A1: web disconnect must not deny approvals covered by a live Telegram bridge.
	ap := NewApprovalHub(time.Hour)
	ap.SetBridgeLive(func() bool { return true })
	fan := &recordingFanout{}
	ap.SetFanout(fan)

	id, decision := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
	if fan.pendingCount() != 1 {
		t.Fatalf("pending fan-out count = %d, want 1", fan.pendingCount())
	}

	// Web listener drops; bridge is still live.
	ap.DisconnectFor("s1")

	// Approval must remain pending, NOT settled.
	if count := ap.PendingCount("s1"); count != 1 {
		t.Fatalf("pending count after disconnect = %d, want 1", count)
	}
	if r := ap.Reason("s1", id); r != "" {
		t.Fatalf("reason after disconnect = %q, want empty (unsettled)", r)
	}
	select {
	case <-decision:
		t.Fatal("decision channel yielded before Telegram resolve")
	default:
	}

	// Resolve from Telegram: returns 200, decision channel yields true.
	if code := ap.ResolveFrom("s1", id, true, "telegram"); code != 200 {
		t.Fatalf("ResolveFrom code = %d, want 200", code)
	}
	select {
	case got := <-decision:
		if !got {
			t.Fatal("decision channel yielded false, want true")
		}
	case <-time.After(time.Second):
		t.Fatal("decision channel did not yield after Telegram resolve")
	}

	if count := ap.PendingCount("s1"); count != 0 {
		t.Fatalf("pending count after resolve = %d, want 0", count)
	}
	if r, s := ap.Reason("s1", id), ap.Source("s1", id); r != "ok" || s != "telegram" {
		t.Fatalf("reason/source = %q/%q, want ok/telegram", r, s)
	}
}

func TestDisconnectForBridgeDeadDeniesDisconnected(t *testing.T) {
	// A1 symmetry: if the bridge is not live, DisconnectFor settles denied:disconnected
	// and the decision channel yields false immediately.
	ap := NewApprovalHub(time.Hour)
	ap.SetBridgeLive(func() bool { return false })
	fan := &recordingFanout{}
	ap.SetFanout(fan)

	id, decision := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
	ap.DisconnectFor("s1")

	if count := ap.PendingCount("s1"); count != 0 {
		t.Fatalf("pending count = %d, want 0", count)
	}
	select {
	case got := <-decision:
		if got {
			t.Fatal("decision channel yielded true, want false")
		}
	case <-time.After(time.Second):
		t.Fatal("decision channel did not yield after disconnect with dead bridge")
	}
	if r, s := ap.Reason("s1", id), ap.Source("s1", id); r != "disconnected" || s != "web" {
		t.Fatalf("reason/source = %q/%q, want disconnected/web", r, s)
	}
}
