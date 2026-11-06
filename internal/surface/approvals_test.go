package surface

import (
	"sync"
	"testing"
	"time"
)

func TestApprovalHub_RegisterResolveAllow(t *testing.T) {
	h := NewApprovalHub(5 * time.Minute)

	id, ch := h.Register("s_123", "echo", `{"v":"hi"}`, func(id string) {})
	if id == "" {
		t.Fatal("empty approval id")
	}

	select {
	case <-ch:
		t.Fatal("channel should not be ready before resolve")
	default:
	}

	code := h.Resolve("s_123", id, true)
	if code != 200 {
		t.Fatalf("Resolve = %d, want 200", code)
	}

	select {
	case decision := <-ch:
		if !decision {
			t.Fatal("decision should be true")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("decision channel not ready")
	}

	// Second resolve should return 410
	code = h.Resolve("s_123", id, true)
	if code != 410 {
		t.Fatalf("second Resolve = %d, want 410", code)
	}
}

func TestApprovalHub_DoubleResolve(t *testing.T) {
	h := NewApprovalHub(5 * time.Minute)

	id, ch := h.Register("s_123", "echo", `{"v":"hi"}`, func(id string) {})

	code := h.Resolve("s_123", id, true)
	if code != 200 {
		t.Fatalf("first Resolve = %d, want 200", code)
	}
	<-ch

	code = h.Resolve("s_123", id, false)
	if code != 410 {
		t.Fatalf("second Resolve = %d, want 410", code)
	}
}

func TestApprovalHub_ForeignSession(t *testing.T) {
	h := NewApprovalHub(5 * time.Minute)

	id, _ := h.Register("s_123", "echo", `{"v":"hi"}`, func(id string) {})

	code := h.Resolve("s_456", id, true)
	if code != 404 {
		t.Fatalf("foreign session Resolve = %d, want 404", code)
	}
}

func TestApprovalHub_Timeout(t *testing.T) {
	h := NewApprovalHub(50 * time.Millisecond)

	id, ch := h.Register("s_123", "echo", `{"v":"hi"}`, func(id string) {})

	select {
	case decision := <-ch:
		if decision {
			t.Fatal("timeout decision should be false")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout did not fire")
	}

	reason := h.Reason("s_123", id)
	if reason != "timed_out" {
		t.Fatalf("reason = %q, want timed_out", reason)
	}
}

func TestApprovalHub_DenyAllFor(t *testing.T) {
	h := NewApprovalHub(5 * time.Minute)

	id1, ch1 := h.Register("s_123", "echo", `{"v":"1"}`, func(id string) {})
	_, ch2 := h.Register("s_123", "echo", `{"v":"2"}`, func(id string) {})
	_, ch3 := h.Register("s_456", "echo", `{"v":"3"}`, func(id string) {})

	h.DenyAllFor("s_123", "disconnected")

	select {
	case decision := <-ch1:
		if decision {
			t.Fatal("denied decision should be false")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ch1 not resolved")
	}

	select {
	case decision := <-ch2:
		if decision {
			t.Fatal("denied decision should be false")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ch2 not resolved")
	}

	// Other session should not be affected
	select {
	case <-ch3:
		t.Fatal("other session should not be denied")
	case <-time.After(100 * time.Millisecond):
	}

	if h.PendingCount("s_123") != 0 {
		t.Fatalf("pending count = %d, want 0", h.PendingCount("s_123"))
	}
	if h.PendingCount("s_456") != 1 {
		t.Fatalf("pending count s_456 = %d, want 1", h.PendingCount("s_456"))
	}

	reason := h.Reason("s_123", id1)
	if reason != "disconnected" {
		t.Fatalf("reason = %q, want disconnected", reason)
	}
}

func TestApprovalHub_PendingCount(t *testing.T) {
	h := NewApprovalHub(5 * time.Minute)

	if h.PendingCount("s_123") != 0 {
		t.Fatalf("initial count = %d, want 0", h.PendingCount("s_123"))
	}

	id1, _ := h.Register("s_123", "echo", `{"v":"1"}`, func(id string) {})
	if h.PendingCount("s_123") != 1 {
		t.Fatalf("count after 1 = %d, want 1", h.PendingCount("s_123"))
	}

	_, _ = h.Register("s_123", "echo", `{"v":"2"}`, func(id string) {})
	if h.PendingCount("s_123") != 2 {
		t.Fatalf("count after 2 = %d, want 2", h.PendingCount("s_123"))
	}

	h.Resolve("s_123", id1, true)
	if h.PendingCount("s_123") != 1 {
		t.Fatalf("count after resolve = %d, want 1", h.PendingCount("s_123"))
	}

	h.DenyAllFor("s_123", "test")
	if h.PendingCount("s_123") != 0 {
		t.Fatalf("count after deny all = %d, want 0", h.PendingCount("s_123"))
	}
}

func TestSessionOf(t *testing.T) {
	h := NewApprovalHub(5 * time.Minute)

	id, _ := h.Register("s_123", "echo", `{"v":"1"}`, func(string) {})
	if id == "" {
		t.Fatal("empty approval id")
	}

	sess, ok := h.SessionOf(id)
	if !ok || sess != "s_123" {
		t.Fatalf("SessionOf(%q) = (%q, %v), want (%q, true)", id, sess, ok, "s_123")
	}

	_, ok = h.SessionOf("a_unknown")
	if ok {
		t.Fatal("SessionOf for unknown approval id should return false")
	}
}

// M19: approvals hub unbounded growth — 1000 settle cycles -> map sizes stay bounded;
// pending survive pruning.
func TestApprovalHub_TerminalPruning(t *testing.T) {
	h := NewApprovalHub(5 * time.Minute)
	const sessionID = "s_session_pruning_test"

	// 1. Create a pending approval that will NEVER be settled.
	pendingID, _ := h.Register(sessionID, "pending_tool", `{}`, func(string) {})

	// 2. Run 1000 settle cycles: insert an approval and settle it.
	for i := range 1000 {
		id, ch := h.Register(sessionID, "tool", `{}`, func(string) {})
		code := h.Resolve(sessionID, id, true)
		if code != 200 {
			t.Fatalf("cycle %d: resolve failed with %d", i, code)
		}
		<-ch
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	sessionMap := h.bySession[sessionID]
	if sessionMap == nil {
		t.Fatal("session map is nil")
	}

	// Map size stays bounded (at most 50 terminal entries plus pending).
	if len(sessionMap) > maxTerminalEntriesPerSession+2 {
		t.Fatalf("session map size = %d, want <= %d", len(sessionMap), maxTerminalEntriesPerSession+2)
	}

	// Pending entry must survive pruning!
	pendingAp, ok := sessionMap[pendingID]
	if !ok {
		t.Fatalf("pending approval %s was pruned", pendingID)
	}
	if pendingAp.state != "pending" {
		t.Fatalf("pending approval state = %q, want pending", pendingAp.state)
	}
}

// L-6: approvals settle attribution — double-click race: recorded via is the first click's.
func TestApprovalHub_SettleAttributionFirstWins(t *testing.T) {
	h := NewApprovalHub(5 * time.Minute)
	const sessionID = "s_session_attribution_test"

	id, ch := h.Register(sessionID, "deploy", `{"target":"prod"}`, func(string) {})

	// First click arrives via "web:button-1"
	code1 := h.ResolveFromVia(sessionID, id, true, "web", "web:button-1")
	if code1 != 200 {
		t.Fatalf("first click code = %d, want 200", code1)
	}

	// Second (losing/stale) click arrives via "telegram:card-button-2"
	code2 := h.ResolveFromVia(sessionID, id, true, "telegram", "telegram:card-button-2")
	if code2 != 410 {
		t.Fatalf("second click code = %d, want 410", code2)
	}

	<-ch

	// Audit trail check: recorded via MUST be the first click's!
	gotVia := h.Via(sessionID, id)
	if gotVia != "web:button-1" {
		t.Fatalf("recorded via = %q, want %q (corrupted by second click)", gotVia, "web:button-1")
	}
}

// L-6: concurrent double-click race — recorded via is whichever click settled first.
func TestApprovalHub_SettleAttributionConcurrentRace(t *testing.T) {
	h := NewApprovalHub(5 * time.Minute)
	const sessionID = "s_session_race_test"

	id, ch := h.Register(sessionID, "deploy", `{"target":"prod"}`, func(string) {})

	var wg sync.WaitGroup
	var code1, code2 int
	wg.Add(2)
	go func() {
		defer wg.Done()
		code1 = h.ResolveFromVia(sessionID, id, true, "web", "first-click")
	}()
	go func() {
		defer wg.Done()
		code2 = h.ResolveFromVia(sessionID, id, true, "web", "second-click")
	}()
	wg.Wait()
	<-ch

	var winnerVia string
	switch {
	case code1 == 200 && code2 == 410:
		winnerVia = "first-click"
	case code2 == 200 && code1 == 410:
		winnerVia = "second-click"
	default:
		t.Fatalf("unexpected codes: code1=%d, code2=%d", code1, code2)
	}

	gotVia := h.Via(sessionID, id)
	if gotVia != winnerVia {
		t.Fatalf("recorded via = %q, want winner %q", gotVia, winnerVia)
	}
}

func TestHub_RegisterRefusedAfterShutdown(t *testing.T) {
	h := NewApprovalHub(5 * time.Minute)

	// Mint before shutdown -> normal card
	idPre, chPre := h.Register("s_pre", "echo", `{"v":"1"}`, func(string) {})
	if idPre == "" {
		t.Fatal("expected non-empty id before shutdown")
	}
	select {
	case <-chPre:
		t.Fatal("decision channel should not be ready before resolve/shutdown")
	default:
	}
	if len(h.Pending()) != 1 {
		t.Fatalf("pending before shutdown = %d, want 1", len(h.Pending()))
	}
	if len(h.PendingTurnCards()) != 1 {
		t.Fatalf("pending turn cards before shutdown = %d, want 1", len(h.PendingTurnCards()))
	}
	if h.PendingCount("s_pre") != 1 {
		t.Fatalf("pending count before shutdown = %d, want 1", h.PendingCount("s_pre"))
	}

	// Call hub Shutdown
	h.Shutdown()

	// Pre-shutdown card settles as denied
	select {
	case d := <-chPre:
		if d {
			t.Fatal("pre-shutdown card should settle as false (denied)")
		}
	case <-time.After(time.Second):
		t.Fatal("pre-shutdown decision channel should settle immediately on shutdown")
	}
	if r := h.Reason("s_pre", idPre); r != "shutdown" {
		t.Fatalf("pre-shutdown card reason = %q, want shutdown", r)
	}

	// Every mint method -> immediate deny (decision channel delivers false with no blocking)
	// 1. Register
	id1, ch1 := h.Register("s_post1", "tool1", "args1", func(string) {
		t.Fatal("onEvent fired after shutdown for Register")
	})
	if id1 == "" {
		t.Fatal("expected non-empty id from Register after shutdown")
	}
	select {
	case d := <-ch1:
		if d {
			t.Fatal("Register decision should be false")
		}
	default:
		t.Fatal("Register decision channel blocked")
	}

	// 2. RegisterOn
	id2, ch2 := h.RegisterOn("s_post2", "tool2", "args2", true, func(string) {
		t.Fatal("onEvent fired after shutdown for RegisterOn")
	})
	if id2 == "" {
		t.Fatal("expected non-empty id from RegisterOn after shutdown")
	}
	select {
	case d := <-ch2:
		if d {
			t.Fatal("RegisterOn decision should be false")
		}
	default:
		t.Fatal("RegisterOn decision channel blocked")
	}

	// 3. RegisterCustos
	id3, ch3 := h.RegisterCustos("cell3", "cred3", "dest3", "args3", 0, func(string) {
		t.Fatal("onEvent fired after shutdown for RegisterCustos")
	})
	if id3 == "" {
		t.Fatal("expected non-empty id from RegisterCustos after shutdown")
	}
	select {
	case d := <-ch3:
		if d {
			t.Fatal("RegisterCustos decision should be false")
		}
	default:
		t.Fatal("RegisterCustos decision channel blocked")
	}

	// 4. RegisterCustosNamed
	id4, ch4 := h.RegisterCustosNamed("cell4", "cred4", "name4", "dest4", "args4", 0, func(string) {
		t.Fatal("onEvent fired after shutdown for RegisterCustosNamed")
	})
	if id4 == "" {
		t.Fatal("expected non-empty id from RegisterCustosNamed after shutdown")
	}
	select {
	case d := <-ch4:
		if d {
			t.Fatal("RegisterCustosNamed decision should be false")
		}
	default:
		t.Fatal("RegisterCustosNamed decision channel blocked")
	}

	// Assert no card listed in any GET view, zero goroutines still waiting
	if pending := h.Pending(); len(pending) != 0 {
		t.Fatalf("pending cards = %d, want 0", len(pending))
	}
	if cards := h.PendingTurnCards(); len(cards) != 0 {
		t.Fatalf("pending turn cards = %d, want 0", len(cards))
	}
	if h.PendingCount("s_pre") != 0 {
		t.Fatalf("pending count s_pre = %d, want 0", h.PendingCount("s_pre"))
	}
	if h.PendingCount("s_post1") != 0 {
		t.Fatalf("pending count s_post1 = %d, want 0", h.PendingCount("s_post1"))
	}
	if h.PendingCount("s_post2") != 0 {
		t.Fatalf("pending count s_post2 = %d, want 0", h.PendingCount("s_post2"))
	}
	if h.PendingCount("cell3") != 0 {
		t.Fatalf("pending count cell3 = %d, want 0", h.PendingCount("cell3"))
	}
	if h.PendingCount("cell4") != 0 {
		t.Fatalf("pending count cell4 = %d, want 0", h.PendingCount("cell4"))
	}
}
