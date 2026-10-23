package surface

import (
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
