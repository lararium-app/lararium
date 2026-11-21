package surface

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// gatedEventsSource holds every sessions.subscribe call inside its catch-up
// read: Events signals entered, then waits for one token on release. While a
// call is held there, its subscription is already registered but its result
// frame has not been written, which is exactly the window issue #310 is about.
type gatedEventsSource struct {
	*fakeSessionSource

	entered chan struct{}
	release chan struct{}
}

func newGatedEventsSource(inner *fakeSessionSource) *gatedEventsSource {
	return &gatedEventsSource{
		fakeSessionSource: inner,
		entered:           make(chan struct{}, 8),
		release:           make(chan struct{}),
	}
}

func (g *gatedEventsSource) Events(id string, afterSeq int64, limit int) ([]json.RawMessage, bool, error) {
	g.entered <- struct{}{}
	<-g.release
	return g.fakeSessionSource.Events(id, afterSeq, limit)
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// deltaEvent is a live session.event payload carrying a sequence marker n,
// built through the same encoder the turn's RPC sink uses.
func deltaEvent(n int) json.RawMessage {
	return buildRPCSessionEventJSON("delta", fmt.Sprintf(`{"text":"t%d","n":%d}`, n, n))
}

func assertSessionDelta(t *testing.T, notif *RPCNotification, wantN int) {
	t.Helper()
	if notif.Method != "session.event" {
		t.Fatalf("notification method = %q, want session.event (want n=%d)", notif.Method, wantN)
	}
	var params struct {
		SessionID string `json:"session_id"`
		Event     struct {
			Type string `json:"type"`
			N    int    `json:"n"`
		} `json:"event"`
	}
	if err := json.Unmarshal(notif.Params, &params); err != nil {
		t.Fatalf("unmarshal session.event params: %v (raw=%s)", err, notif.Params)
	}
	if params.SessionID != deliverySessionID || params.Event.Type != "delta" || params.Event.N != wantN {
		t.Fatalf("session.event = %s, want session %s delta n=%d", notif.Params, deliverySessionID, wantN)
	}
}

func readSubscribeResult(t *testing.T, client *testWSClient) *RPCResponse {
	t.Helper()
	op, raw := client.readServerFrame(t)
	if op != wsOpText {
		t.Fatalf("frame opcode = %d, want text", op)
	}
	resp, err := DecodeRPCResponse(raw)
	if err != nil {
		t.Fatalf("frame is not a response (a live event overtook the subscribe result?): %v (raw=%s)", err, raw)
	}
	if resp.Error != nil {
		t.Fatalf("subscribe failed: %+v", resp.Error)
	}
	return resp
}

func sendSubscribe(t *testing.T, client *testWSClient, id int64) {
	t.Helper()
	req, err := EncodeRPCRequest(id, "sessions.subscribe", map[string]any{"id": deliverySessionID})
	if err != nil {
		t.Fatal(err)
	}
	client.writeMaskedText(t, req)
}

const deliverySessionID = "s_ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// Issue #310 acceptance: a turn already in flight on S emits while this
// connection is inside sessions.subscribe for S. The result frame must come
// first, the held-back events after it in emit order, and the emitting turn
// must never wait on the subscribe call.
func TestRPCSessionsSubscribe_ResultPrecedesLiveEvents(t *testing.T) {
	srv, store, ts := newTestRPCServer(t, 2*time.Second)
	src := newGatedEventsSource(&fakeSessionSource{
		events:   []json.RawMessage{json.RawMessage(`{"seq":1,"t":"msg","role":"user","text":"hi"}`)},
		inFlight: true,
	})
	srv.Sessions = src
	tok, err := store.Create("delivery-order")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	sendSubscribe(t, client, 1)
	waitSignal(t, src.entered, "the subscribe catch-up read")

	// notifyRPCSessionEvent is the sink the in-flight turn's onEvent calls
	// (rpc_sessions.go, rpcSessionsSend). It runs on its own goroutine, as
	// the turn does, and must return while the subscribe is still open.
	emitted := make(chan struct{})
	go func() {
		defer close(emitted)
		for n := 1; n <= 3; n++ {
			srv.notifyRPCSessionEvent(deliverySessionID, deltaEvent(n))
		}
	}()
	waitSignal(t, emitted, "live emits to return during an open subscribe")
	src.release <- struct{}{}

	resp := readSubscribeResult(t, client)
	if resp.ID != 1 {
		t.Fatalf("result id = %d, want 1", resp.ID)
	}
	var out struct {
		Events   []json.RawMessage `json:"events"`
		InFlight bool              `json:"in_flight"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 1 || !out.InFlight {
		t.Fatalf("catch-up = %d events in_flight=%v, want 1 true", len(out.Events), out.InFlight)
	}
	for n := 1; n <= 3; n++ {
		assertSessionDelta(t, client.readNotification(t), n)
	}

	// Once the result is out, delivery is live again.
	srv.notifyRPCSessionEvent(deliverySessionID, deltaEvent(4))
	assertSessionDelta(t, client.readNotification(t), 4)
}

// Events emitted continuously across the close of the hold-back window are
// neither dropped nor reordered: the queue drain and live writes for one
// (connection, session) pair are serialized.
func TestRPCSessionsSubscribe_NoDropOrReorderAcrossWindowClose(t *testing.T) {
	const total = 300
	srv, store, ts := newTestRPCServer(t, 2*time.Second)
	src := newGatedEventsSource(&fakeSessionSource{inFlight: true})
	srv.Sessions = src
	tok, err := store.Create("delivery-stress")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	sendSubscribe(t, client, 1)
	waitSignal(t, src.entered, "the subscribe catch-up read")

	quarter := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := 1; n <= total; n++ {
			srv.notifyRPCSessionEvent(deliverySessionID, deltaEvent(n))
			if n == total/4 {
				close(quarter)
			}
		}
	}()
	waitSignal(t, quarter, "the first quarter of emits")
	src.release <- struct{}{}

	readSubscribeResult(t, client)
	for n := 1; n <= total; n++ {
		assertSessionDelta(t, client.readNotification(t), n)
	}
	waitSignal(t, done, "the emitter to finish")
}

// A re-subscribe on an already-live subscription (a client catching up with
// after_seq) holds events back until its own result, too.
func TestRPCSessionsSubscribe_ResubscribeHoldsBackUntilItsResult(t *testing.T) {
	srv, store, ts := newTestRPCServer(t, 2*time.Second)
	src := newGatedEventsSource(&fakeSessionSource{inFlight: true})
	srv.Sessions = src
	tok, err := store.Create("delivery-resub")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	sendSubscribe(t, client, 1)
	waitSignal(t, src.entered, "the first catch-up read")
	src.release <- struct{}{}
	readSubscribeResult(t, client)
	srv.notifyRPCSessionEvent(deliverySessionID, deltaEvent(1))
	assertSessionDelta(t, client.readNotification(t), 1)

	sendSubscribe(t, client, 2)
	waitSignal(t, src.entered, "the re-subscribe catch-up read")
	srv.notifyRPCSessionEvent(deliverySessionID, deltaEvent(2))
	src.release <- struct{}{}

	if resp := readSubscribeResult(t, client); resp.ID != 2 {
		t.Fatalf("result id = %d, want 2", resp.ID)
	}
	assertSessionDelta(t, client.readNotification(t), 2)
}

// Two subscribe calls open on the same (connection, session) pair at once:
// events stay held until the LAST open call has written its result.
func TestRPCSessionsSubscribe_ConcurrentCallsReleaseAfterLastResult(t *testing.T) {
	srv, store, ts := newTestRPCServer(t, 2*time.Second)
	src := newGatedEventsSource(&fakeSessionSource{inFlight: true})
	srv.Sessions = src
	tok, err := store.Create("delivery-concurrent")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	sendSubscribe(t, client, 1)
	sendSubscribe(t, client, 2)
	waitSignal(t, src.entered, "the first catch-up read")
	waitSignal(t, src.entered, "the second catch-up read")
	srv.notifyRPCSessionEvent(deliverySessionID, deltaEvent(1))

	src.release <- struct{}{}
	first := readSubscribeResult(t, client)
	src.release <- struct{}{}
	second := readSubscribeResult(t, client)
	if first.ID == second.ID || first.ID+second.ID != 3 {
		t.Fatalf("result ids = %d, %d, want 1 and 2", first.ID, second.ID)
	}
	assertSessionDelta(t, client.readNotification(t), 1)
}

// A subscribe for a session that does not exist still leaves no
// subscription behind (unchanged behavior, re-covered for the new state).
func TestRPCSessionsSubscribe_NotFoundLeavesNoSubscription(t *testing.T) {
	srv, store, ts := newTestRPCServer(t, 2*time.Second)
	srv.Sessions = &fakeSessionSource{eventsErr: ErrNoSession}
	tok, err := store.Create("delivery-notfound")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	assertRPCErrorReason(t, client.callRPC(t, 1, "sessions.subscribe", map[string]any{"id": deliverySessionID}), "not_found")
	assertRPCErrorReason(t, client.callRPC(t, 2, "sessions.send", map[string]any{"id": deliverySessionID, "text": "hi"}), "not_subscribed")

	srv.rpcSubsMu.RLock()
	n := len(srv.rpcSubs[deliverySessionID])
	srv.rpcSubsMu.RUnlock()
	if n != 0 {
		t.Fatalf("subscriptions left for %s = %d, want 0", deliverySessionID, n)
	}
}
