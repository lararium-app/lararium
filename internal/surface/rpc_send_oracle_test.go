package surface

import (
	"net/http"
	"testing"
)

// sessions.send must answer a malformed, hidden and unknown id the same way
// whatever the other params are: an empty text or a missing subscription
// must not tell a caller which ids exist (SURFACE-SPEC §4b).
func TestRPCSessionsSend_UnknownHiddenAndMalformedIDsAnswerIdentically(t *testing.T) {
	f := newAudienceFixture(t)
	hidden := f.createREST(t, f.tokA, true).ID
	visible := f.createREST(t, f.tokA, false).ID
	const unknown = "s_00000000000000000000000ZZZ"
	client := dialAuthenticatedWS(t, f.ts.URL, f.tokB)

	var rid int64
	for _, id := range []string{hidden, unknown, "s_short", ""} {
		for _, text := range []string{"hello", "   "} {
			rid++
			resp := client.callRPC(t, rid, "sessions.send", map[string]any{"id": id, "text": text})
			assertRPCErrorReason(t, resp, "not_found")
		}
	}

	// A session the caller can see keeps its own parameter and subscription
	// replies: they say nothing about ids the caller cannot see.
	rid++
	assertRPCErrorReason(t, client.callRPC(t, rid, "sessions.send", map[string]any{"id": visible, "text": "hello"}), "not_subscribed")
	rid++
	assertRPCErrorReason(t, client.callRPC(t, rid, "sessions.send", map[string]any{"id": visible, "text": "  "}), "empty_message_text")

	if n := f.modelCalls(); n != 0 {
		t.Errorf("model was called %d times, want 0", n)
	}
}

// The first turn on a fresh hearth creates main, so main is not gated on
// existing: the unsubscribed reply, not not_found, is what it gets.
func TestRPCSessionsSend_MainIsNotGatedOnExisting(t *testing.T) {
	f := newAudienceFixture(t)
	client := dialAuthenticatedWS(t, f.ts.URL, f.tokA)
	assertRPCErrorReason(t, client.callRPC(t, 1, "sessions.send", map[string]any{"id": "main", "text": "hello"}), "not_subscribed")
}

// Deleting a session withdraws every live subscription on it, so a stale
// subscription cannot satisfy the send gate for an id that is gone.
func TestSessionDelete_DropsLiveSubscriptions(t *testing.T) {
	for _, via := range []string{"REST", "RPC"} {
		t.Run(via, func(t *testing.T) {
			f := newAudienceFixture(t)
			id := f.createREST(t, f.tokA, false).ID
			a := dialAuthenticatedWS(t, f.ts.URL, f.tokA)
			b := dialAuthenticatedWS(t, f.ts.URL, f.tokB)
			for i, c := range []*testWSClient{a, b} {
				if resp := c.callRPC(t, int64(i+1), "sessions.subscribe", map[string]any{"id": id}); resp.Error != nil {
					t.Fatalf("subscribe: %+v", resp.Error)
				}
			}

			f.srv.rpcSubsMu.RLock()
			n := len(f.srv.rpcSubs[id])
			f.srv.rpcSubsMu.RUnlock()
			if n != 2 {
				t.Fatalf("subscriptions before delete = %d, want 2", n)
			}

			if via == "REST" {
				if code, out := f.do(t, f.tokA, http.MethodDelete, "/v1/sessions/"+id, ""); code != http.StatusNoContent {
					t.Fatalf("DELETE = %d %s", code, out)
				}
			} else if resp := a.callRPC(t, 10, "sessions.delete", map[string]any{"id": id}); resp.Error != nil {
				t.Fatalf("sessions.delete: %+v", resp.Error)
			}

			f.srv.rpcSubsMu.RLock()
			_, left := f.srv.rpcSubs[id]
			f.srv.rpcSubsMu.RUnlock()
			if left {
				t.Error("subscriptions on a deleted session were not dropped")
			}
			assertRPCErrorReason(t, b.callRPC(t, 11, "sessions.send", map[string]any{"id": id, "text": "hello"}), "not_found")
		})
	}
}
