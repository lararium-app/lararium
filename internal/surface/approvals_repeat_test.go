package surface

import (
	"testing"
	"time"
)

// Issue #348 (CUSTOS-SPEC §6.4d): an exact in-turn repeat of an approved
// call is allowed silently — one card, one verdict, one audit row plus an
// auto_allowed terminal for the second call.
func TestApprovalHub_TurnRepeatAfterAllowIsSilent(t *testing.T) {
	h := NewApprovalHub(5 * time.Minute)
	fan := &recordingFanout{}
	h.SetFanout(fan)

	var minted []string
	onEvent := func(id string) { minted = append(minted, id) }

	id1, ch1 := h.RegisterTurn("s1", "t_a", "echo", `{"v":"hi"}`, true, onEvent)
	if code := h.Resolve("s1", id1, true); code != 200 {
		t.Fatalf("Resolve = %d, want 200", code)
	}
	if !<-ch1 {
		t.Fatal("first call: decision = deny, want allow")
	}

	id2, ch2 := h.RegisterTurn("s1", "t_a", "echo", `{"v":"hi"}`, true, onEvent)
	if len(minted) != 1 {
		t.Fatalf("repeat minted a second card (%d approval_requests)", len(minted))
	}
	if id2 == id1 || id2 == "" {
		t.Fatalf("repeat correlation id = %q, want fresh id distinct from %q", id2, id1)
	}
	select {
	case allow := <-ch2:
		if !allow {
			t.Fatal("repeat after allow = deny, want allow")
		}
	default:
		t.Fatal("repeat decision not pre-resolved")
	}
	if got := fan.pendingCount(); got != 1 {
		t.Fatalf("pending fanouts = %d, want 1", got)
	}
	if !hasTerminal(fan, id2, "approved", RepeatAutoAllowed) {
		t.Fatalf("no %s audit row for %q in %v", RepeatAutoAllowed, id2, fan.terminals())
	}
	// The shadow id is audit-only: resolving it finds no card.
	if code := h.Resolve("s1", id2, false); code != 404 {
		t.Fatalf("Resolve(shadow) = %d, want 404", code)
	}
}

// Issue #348: a repeat of a DENIED call is denied immediately with
// reason repeat_denied — no second card to fatigue the approver.
func TestApprovalHub_TurnRepeatAfterDenyIsAutoDenied(t *testing.T) {
	h := NewApprovalHub(5 * time.Minute)
	fan := &recordingFanout{}
	h.SetFanout(fan)

	var events int
	onEvent := func(string) { events++ }

	id1, ch1 := h.RegisterTurn("s1", "t_a", "send_email", `{"to":"x"}`, true, onEvent)
	if code := h.Resolve("s1", id1, false); code != 200 {
		t.Fatalf("Resolve = %d, want 200", code)
	}
	if <-ch1 {
		t.Fatal("first call: decision = allow, want deny")
	}

	_, ch2 := h.RegisterTurn("s1", "t_a", "send_email", `{"to":"x"}`, true, onEvent)
	if events != 1 {
		t.Fatalf("denied repeat minted a second card (%d approval_requests)", events)
	}
	select {
	case allow := <-ch2:
		if allow {
			t.Fatal("repeat after deny = allow, want deny (fail-closed)")
		}
	default:
		t.Fatal("repeat decision not pre-resolved")
	}
	if got := fan.pendingCount(); got != 1 {
		t.Fatalf("pending fanouts = %d, want 1", got)
	}
	shadowID := ""
	for _, row := range fan.terminals() {
		if row[3] == RepeatDenied {
			shadowID = row[0]
		}
	}
	if shadowID == "" {
		t.Fatalf("no %s audit row in %v", RepeatDenied, fan.terminals())
	}
}

// A timed-out card also suppresses the in-turn repeat (fail-closed):
// the approver was absent once; the model looping on timeout must not
// stack 5-minute cards.
func TestApprovalHub_TurnRepeatAfterTimeoutIsAutoDenied(t *testing.T) {
	h := NewApprovalHub(15 * time.Millisecond)
	fan := &recordingFanout{}
	h.SetFanout(fan)

	id1, ch1 := h.RegisterTurn("s1", "t_a", "echo", `{}`, true, func(string) {})
	select {
	case allow := <-ch1:
		if allow {
			t.Fatal("timeout must deny")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("approval never timed out")
	}
	_ = id1

	_, ch2 := h.RegisterTurn("s1", "t_a", "echo", `{}`, true, func(string) {
		t.Fatal("repeat after timeout minted a card")
	})
	select {
	case allow := <-ch2:
		if allow {
			t.Fatal("repeat after timeout = allow, want deny")
		}
	case <-time.After(time.Second):
		t.Fatal("repeat after timeout not pre-resolved")
	}
}

// Issue #348: while the card is still pending, a repeat JOINS it — one
// card, one approver answer, both waiters wake with the same decision.
func TestApprovalHub_TurnRepeatJoinsPendingCard(t *testing.T) {
	h := NewApprovalHub(5 * time.Minute)
	fan := &recordingFanout{}
	h.SetFanout(fan)

	var events int
	onEvent := func(string) { events++ }

	id1, ch1 := h.RegisterTurn("s1", "t_a", "echo", `{}`, true, onEvent)
	id2, ch2 := h.RegisterTurn("s1", "t_a", "echo", `{}`, true, onEvent)
	if events != 1 {
		t.Fatalf("join minted a second card (%d approval_requests)", events)
	}
	if id2 == id1 {
		t.Fatal("joiner must get a fresh correlation id")
	}
	select {
	case <-ch2:
		t.Fatal("joiner woke before the approver answered")
	default:
	}

	if code := h.Resolve("s1", id1, true); code != 200 {
		t.Fatalf("Resolve = %d, want 200", code)
	}
	select {
	case allow := <-ch1:
		if !allow {
			t.Fatal("leader: decision = deny, want allow")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("leader decision not delivered")
	}
	select {
	case allow := <-ch2:
		if !allow {
			t.Fatal("joiner: decision = deny, want allow")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("joiner decision not mirrored from leader")
	}
	if got := fan.pendingCount(); got != 1 {
		t.Fatalf("pending fanouts = %d, want 1", got)
	}
}

// #291 invariant: the NEXT turn re-approves. Suppression is strictly
// in-turn — cache keys embed the turn id.
func TestApprovalHub_TurnRepeatNextTurnRemints(t *testing.T) {
	h := NewApprovalHub(5 * time.Minute)

	var events int
	onEvent := func(string) { events++ }

	id1, ch1 := h.RegisterTurn("s1", "t_a", "echo", `{}`, true, onEvent)
	if code := h.Resolve("s1", id1, true); code != 200 {
		t.Fatalf("Resolve = %d, want 200", code)
	}
	<-ch1

	id2, ch2 := h.RegisterTurn("s1", "t_b", "echo", `{}`, true, onEvent)
	if events != 2 {
		t.Fatalf("next-turn identical call minted %d cards, want 1 new card", events-1)
	}
	if id2 == "" {
		t.Fatal("empty id for next-turn card")
	}
	select {
	case <-ch2:
		t.Fatal("next-turn card must park for a fresh verdict")
	default:
	}
	// Other args are not a repeat.
	if _, ch := h.RegisterTurn("s1", "t_a", "echo", `{"v":"other"}`, true, onEvent); events != 3 {
		t.Fatal("different args must mint their own card")
	} else {
		_ = ch
	}
	// Different session, same turn id: no cross-talk.
	if _, ch := h.RegisterTurn("s2", "t_a", "echo", `{}`, true, onEvent); events != 4 {
		t.Fatal("cross-session key collision")
	} else {
		_ = ch
	}
}

// Legacy scope-free callers (empty turn id, and RegisterOn itself) keep
// minting per call: #348 suppression is opt-in via turn scope only.
func TestApprovalHub_TurnRepeatLegacyUnscopedUnchanged(t *testing.T) {
	h := NewApprovalHub(5 * time.Minute)

	var events int
	onEvent := func(string) { events++ }

	id1, ch1 := h.RegisterTurn("s1", "", "echo", `{}`, true, onEvent)
	_, ch2 := h.RegisterTurn("s1", "", "echo", `{}`, true, onEvent)
	if events != 2 {
		t.Fatalf("unscoped repeats minted %d cards, want 2 (legacy behaviour)", events)
	}
	h.Resolve("s1", id1, true)
	<-ch1
	h.Resolve("s1", id1, true) // both parked waiters are distinct calls
	select {
	case <-ch2:
		t.Fatal("legacy second call shares the first call's verdict")
	default:
	}
}

func hasTerminal(f *recordingFanout, id, state, reason string) bool {
	for _, row := range f.terminals() {
		if row[0] == id && row[2] == state && row[3] == reason {
			return true
		}
	}
	return false
}
