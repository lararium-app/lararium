package surface

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type rpcTestCustosRegistry struct {
	mu          sync.Mutex
	cards       []CustosCard
	subs        map[int]func(*CustosCard, *CustosGone)
	nextSubID   int
	cancelCount atomic.Int32
	attachCalls atomic.Int32
	onSubscribe func(push func(*CustosCard, *CustosGone))
	resolveFn   func(id, verdict string) (string, error)
}

func newRPCTestCustosRegistry() *rpcTestCustosRegistry {
	return &rpcTestCustosRegistry{
		subs: make(map[int]func(*CustosCard, *CustosGone)),
	}
}

func (r *rpcTestCustosRegistry) Snapshot() []CustosCard {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]CustosCard, len(r.cards))
	copy(out, r.cards)
	return out
}

func (r *rpcTestCustosRegistry) Subscribe(f func(card *CustosCard, gone *CustosGone)) func() {
	r.mu.Lock()
	r.nextSubID++
	id := r.nextSubID
	r.subs[id] = f
	hook := r.onSubscribe
	r.mu.Unlock()

	if hook != nil {
		hook(f)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.subs, id)
			r.mu.Unlock()
			r.cancelCount.Add(1)
		})
	}
}

func (r *rpcTestCustosRegistry) Emit(card *CustosCard, gone *CustosGone) {
	r.mu.Lock()
	cbs := make([]func(*CustosCard, *CustosGone), 0, len(r.subs))
	for _, cb := range r.subs {
		cbs = append(cbs, cb)
	}
	r.mu.Unlock()
	for _, cb := range cbs {
		cb(card, gone)
	}
}

func (r *rpcTestCustosRegistry) ActiveSubCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.subs)
}

func (r *rpcTestCustosRegistry) Resolve(id, verdict string) (string, error) {
	if r.resolveFn != nil {
		return r.resolveFn(id, verdict)
	}
	return "", ErrNoSuchCard
}

func (r *rpcTestCustosRegistry) Attach() ([]CustosCard, func(), <-chan CustosEvent) {
	r.attachCalls.Add(1)
	ch := make(chan CustosEvent)
	close(ch)
	return nil, func() {}, ch
}

func (c *testWSClient) readNotification(t *testing.T) *RPCNotification {
	t.Helper()
	op, payload := c.readServerFrame(t)
	if op != wsOpText {
		t.Fatalf("notification opcode = %d, want text", op)
	}
	notif, err := DecodeRPCNotification(payload)
	if err != nil {
		t.Fatalf("DecodeRPCNotification: %v (raw=%s)", err, string(payload))
	}
	return notif
}

func TestRPCCustosCardsSubscribe_SnapshotMergeAndLiveDelivery(t *testing.T) {
	srv, tokStore, ts := newTestRPCServer(t, 2*time.Second)
	reg := newRPCTestCustosRegistry()
	hub := NewApprovalHub(30 * time.Second)
	srv.SetCustosRegistry(reg)
	srv.SetApprovalHub(hub)

	// Register one pending turn card in the hub and two cards in the registry
	// (including one duplicate id matching the hub card to verify dedupe).
	turnID, _ := hub.Register("s_MAIN", "web_fetch", "GET example.com", func(string) {})
	reg.cards = []CustosCard{
		{ID: "a_zzzzzzzzzzzzzzzz", Cell: "c1", Cred: "cred1", Tool: "t1", Dest: "api.example.com"},
		{ID: turnID, Cell: "c_dup", Cred: "cred_dup", Tool: "dup"},
		{ID: "a_0000000000000001", Cell: "c2", Cred: "cred2", Tool: "t2", Dest: "api2.example.com"},
	}

	tok, err := tokStore.Create("custos-sub-merge")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	resp := client.callRPC(t, 1, "custos.cards.subscribe", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("subscribe error: %+v", resp.Error)
	}
	if reg.attachCalls.Load() != 0 {
		t.Fatalf("Attach() was called %d times; must use Subscribe + Snapshot", reg.attachCalls.Load())
	}

	var snap struct {
		Cards []CustosCard `json:"cards"`
	}
	if err := json.Unmarshal(resp.Result, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if len(snap.Cards) != 3 {
		t.Fatalf("snapshot len = %d, want 3 (deduped): %+v", len(snap.Cards), snap.Cards)
	}
	for i := 1; i < len(snap.Cards); i++ {
		if snap.Cards[i-1].ID >= snap.Cards[i].ID {
			t.Errorf("snapshot not strictly sorted by ID: %+v", snap.Cards)
		}
	}

	// Live registry card notification (unwrapped CustosCard).
	reg.Emit(&CustosCard{ID: "a_1111111111111111", Cred: "gmail", Tool: "send", Dest: "mail.example.com"}, nil)
	n1 := client.readNotification(t)
	if n1.Method != "custos.card" {
		t.Fatalf("notif1 method = %q, want custos.card", n1.Method)
	}
	var gotCard CustosCard
	if err := json.Unmarshal(n1.Params, &gotCard); err != nil {
		t.Fatalf("unmarshal CustosCard: %v", err)
	}
	if gotCard.ID != "a_1111111111111111" || gotCard.Cred != "gmail" || gotCard.Tool != "send" {
		t.Errorf("gotCard = %+v", gotCard)
	}

	// Live registry gone notification (unwrapped CustosGone).
	reg.Emit(nil, &CustosGone{ID: "a_1111111111111111", State: "approved", Reason: "ok"})
	n2 := client.readNotification(t)
	if n2.Method != "custos.gone" {
		t.Fatalf("notif2 method = %q, want custos.gone", n2.Method)
	}
	var gotGone CustosGone
	if err := json.Unmarshal(n2.Params, &gotGone); err != nil {
		t.Fatalf("unmarshal CustosGone: %v", err)
	}
	if gotGone.ID != "a_1111111111111111" || gotGone.State != "approved" || gotGone.Reason != "ok" {
		t.Errorf("gotGone = %+v", gotGone)
	}

	// Live hub turn card creation and settlement.
	turnID2, _ := hub.Register("s_MAIN", "mcp__demo__ping", "ping args", func(string) {})
	n3 := client.readNotification(t)
	if n3.Method != "custos.card" {
		t.Fatalf("notif3 method = %q, want custos.card", n3.Method)
	}
	var hubCard CustosCard
	if err := json.Unmarshal(n3.Params, &hubCard); err != nil {
		t.Fatalf("unmarshal hub CustosCard: %v", err)
	}
	if hubCard.ID != turnID2 || hubCard.Tool != "mcp__demo__ping" {
		t.Errorf("hubCard = %+v", hubCard)
	}

	hub.Resolve("s_MAIN", turnID2, false)
	n4 := client.readNotification(t)
	if n4.Method != "custos.gone" {
		t.Fatalf("notif4 method = %q, want custos.gone", n4.Method)
	}
	var hubGone CustosGone
	if err := json.Unmarshal(n4.Params, &hubGone); err != nil {
		t.Fatalf("unmarshal hub CustosGone: %v", err)
	}
	if hubGone.ID != turnID2 || hubGone.State != "denied" {
		t.Errorf("hubGone = %+v", hubGone)
	}
}

func TestRPCCustosCardsSubscribe_OrderingResultBeforeSyncGone(t *testing.T) {
	srv, tokStore, ts := newTestRPCServer(t, 2*time.Second)
	reg := newRPCTestCustosRegistry()
	const cardID = "a_order11111111111"
	reg.cards = []CustosCard{{ID: cardID, Tool: "t1"}}
	// Synchronously push a gone event inside Subscribe before it returns.
	reg.onSubscribe = func(push func(*CustosCard, *CustosGone)) {
		push(nil, &CustosGone{ID: cardID, State: "approved", Reason: "ok"})
	}
	srv.SetCustosRegistry(reg)

	tok, err := tokStore.Create("custos-order")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	req, err := EncodeRPCRequest(7, "custos.cards.subscribe", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	client.writeMaskedText(t, req)

	// Frame 1 MUST be the subscribe response, NEVER the custos.gone notification.
	op1, raw1 := client.readServerFrame(t)
	if op1 != wsOpText {
		t.Fatalf("frame1 opcode = %d", op1)
	}
	resp, err := DecodeRPCResponse(raw1)
	if err != nil {
		t.Fatalf("frame1 was not subscribe response (raw=%s): %v", string(raw1), err)
	}
	if resp.ID != 7 || resp.Error != nil {
		t.Fatalf("unexpected frame1 response: %+v", resp)
	}

	// Frame 2 MUST be the custos.gone notification.
	n := client.readNotification(t)
	if n.Method != "custos.gone" {
		t.Fatalf("frame2 method = %q, want custos.gone", n.Method)
	}
	var gone CustosGone
	if err := json.Unmarshal(n.Params, &gone); err != nil {
		t.Fatal(err)
	}
	if gone.ID != cardID || gone.State != "approved" {
		t.Errorf("gone = %+v", gone)
	}
}

func TestRPCCustosCardsSubscribe_Cap64DeliversWithoutResync(t *testing.T) {
	srv, tokStore, ts := newTestRPCServer(t, 2*time.Second)
	reg := newRPCTestCustosRegistry()
	srv.SetCustosRegistry(reg)

	reg.onSubscribe = func(push func(*CustosCard, *CustosGone)) {
		for i := range custosRPCBufferCap {
			push(&CustosCard{ID: fmt.Sprintf("a_%016d", i), Tool: "fit"}, nil)
		}
	}

	tok, err := tokStore.Create("custos-cap64")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	resp := client.callRPC(t, 9, "custos.cards.subscribe", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("subscribe error: %+v", resp.Error)
	}
	for i := range custosRPCBufferCap {
		n := client.readNotification(t)
		if n.Method != "custos.card" {
			t.Fatalf("event %d method = %q, want custos.card", i, n.Method)
		}
	}
	if reg.cancelCount.Load() != 0 {
		t.Errorf("reg cancelCount = %d, want 0", reg.cancelCount.Load())
	}
}

func TestRPCCustosCardsSubscribe_OverflowSendsOneResyncAndKeepsSocketOpen(t *testing.T) {
	srv, tokStore, ts := newTestRPCServer(t, 2*time.Second)
	reg := newRPCTestCustosRegistry()
	hub := NewApprovalHub(30 * time.Second)
	srv.SetCustosRegistry(reg)
	srv.SetApprovalHub(hub)

	// Push 65 events synchronously during Subscribe (before pump starts) to hit exact cap+1 overflow.
	reg.onSubscribe = func(push func(*CustosCard, *CustosGone)) {
		for i := range custosRPCBufferCap + 1 {
			push(&CustosCard{ID: fmt.Sprintf("a_%016d", i), Tool: "burst"}, nil)
		}
	}

	tok, err := tokStore.Create("custos-overflow")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	// Frame 1: subscribe result.
	resp := client.callRPC(t, 10, "custos.cards.subscribe", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("subscribe error: %+v", resp.Error)
	}

	// Frame 2: exactly one custos.resync notification.
	notif := client.readNotification(t)
	if notif.Method != "custos.resync" {
		t.Fatalf("notif method = %q, want custos.resync", notif.Method)
	}

	// Both upstreams must be cancelled after overflow.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reg.cancelCount.Load() == 1 && len(hub.observerSnapshot()) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if reg.cancelCount.Load() != 1 {
		t.Errorf("reg cancelCount = %d, want 1", reg.cancelCount.Load())
	}
	if got := len(hub.observerSnapshot()); got != 0 {
		t.Errorf("hub active observers = %d, want 0", got)
	}

	// Subsequent emissions must produce NO custody notifications, and the socket stays open.
	reg.Emit(&CustosCard{ID: "a_after_overflow01", Tool: "nope"}, nil)
	hub.Register("s_MAIN", "after_overflow", "args", func(string) {})

	// Another RPC call on the same socket proves no extra custody frames were sent and socket is alive.
	listResp := client.callRPC(t, 11, "policy.rules.list", map[string]any{})
	assertRPCErrorReason(t, listResp, "locked")
}

func TestRPCCustosCardsSubscribe_ResubscribeReplacesWithoutDuplicate(t *testing.T) {
	srv, tokStore, ts := newTestRPCServer(t, 2*time.Second)
	reg := newRPCTestCustosRegistry()
	hub := NewApprovalHub(30 * time.Second)
	srv.SetCustosRegistry(reg)
	srv.SetApprovalHub(hub)

	tok, err := tokStore.Create("custos-resub")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	if resp := client.callRPC(t, 1, "custos.cards.subscribe", map[string]any{}); resp.Error != nil {
		t.Fatalf("first subscribe error: %+v", resp.Error)
	}
	if reg.ActiveSubCount() != 1 || len(hub.observerSnapshot()) != 1 {
		t.Fatalf("active subs = (reg=%d, hub=%d), want (1, 1)", reg.ActiveSubCount(), len(hub.observerSnapshot()))
	}

	// Second subscribe on the same connection replaces the first.
	if resp := client.callRPC(t, 2, "custos.cards.subscribe", map[string]any{}); resp.Error != nil {
		t.Fatalf("second subscribe error: %+v", resp.Error)
	}
	if reg.cancelCount.Load() != 1 {
		t.Errorf("reg cancelCount = %d, want 1", reg.cancelCount.Load())
	}
	if reg.ActiveSubCount() != 1 || len(hub.observerSnapshot()) != 1 {
		t.Fatalf("after resubscribe active subs = (reg=%d, hub=%d), want (1, 1)", reg.ActiveSubCount(), len(hub.observerSnapshot()))
	}

	// Emit one card and immediately send another RPC; exactly one custos.card should arrive before the RPC reply.
	reg.Emit(&CustosCard{ID: "a_single1111111111", Tool: "only_once"}, nil)
	n := client.readNotification(t)
	if n.Method != "custos.card" {
		t.Fatalf("got method %q, want custos.card", n.Method)
	}
	// Verify no second duplicate notification precedes the next response.
	reply := client.callRPC(t, 3, "policy.rules.list", map[string]any{})
	assertRPCErrorReason(t, reply, "locked")
}

func TestRPCCustosCardsSubscribe_SocketCloseCancelsAndTearsDownCleanly(t *testing.T) {
	store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Store:         store,
		Sessions:      &fakeSessionSource{},
		Hub:           &fakeHub{},
		Cfg:           ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
		wsAuthTimeout: 2 * time.Second,
	}
	reg := newRPCTestCustosRegistry()
	hub := NewApprovalHub(30 * time.Second)
	srv.SetCustosRegistry(reg)
	srv.SetApprovalHub(hub)

	handler := NewServer(srv)
	wsDone := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ws" {
			defer close(wsDone)
		}
		handler.ServeHTTP(w, r)
	}))
	tok, err := store.Create("custos-teardown")
	if err != nil {
		t.Fatal(err)
	}
	client := dialAuthenticatedWS(t, ts.URL, tok)

	if resp := client.callRPC(t, 1, "custos.cards.subscribe", map[string]any{}); resp.Error != nil {
		t.Fatalf("subscribe error: %+v", resp.Error)
	}
	if reg.ActiveSubCount() != 1 || len(hub.observerSnapshot()) != 1 {
		t.Fatalf("expected active subs on both registry and hub")
	}

	// Close the client socket while the custody subscription is live.
	_ = client.conn.Close()

	// Assert wsHandler itself actually returns (proving no reqWg.Wait deadlock on teardown).
	select {
	case <-wsDone:
	case <-time.After(3 * time.Second):
		t.Fatal("wsHandler did not return after client socket close (custos.cards.subscribe teardown deadlock)")
	}

	// Assert server teardown completes promptly as well.
	closed := make(chan struct{})
	go func() {
		ts.Close()
		close(closed)
	}()

	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("httptest.Server.Close() hung after client socket close")
	}

	if reg.cancelCount.Load() != 1 {
		t.Errorf("registry cancelCount = %d, want 1", reg.cancelCount.Load())
	}
	if got := len(hub.observerSnapshot()); got != 0 {
		t.Errorf("hub observer count = %d, want 0", got)
	}
	srv.rpcCustosMu.Lock()
	remaining := len(srv.rpcCustosSubs)
	srv.rpcCustosMu.Unlock()
	if remaining != 0 {
		t.Errorf("remaining rpcCustosSubs = %d, want 0", remaining)
	}
}

func TestRPCCustosCardsResolve(t *testing.T) {
	const validID = "a_0123456789abcdef"

	t.Run("registry happy paths and all error mappings", func(t *testing.T) {
		srv, tokStore, ts := newTestRPCServer(t, 2*time.Second)
		reg := newRPCTestCustosRegistry()
		srv.SetCustosRegistry(reg)
		tok, err := tokStore.Create("custos-resolve")
		if err != nil {
			t.Fatal(err)
		}
		client := dialAuthenticatedWS(t, ts.URL, tok)

		// Validation gates.
		assertRPCErrorReason(t, client.callRPC(t, 1, "custos.cards.resolve", map[string]any{
			"id": "bad-id", "verdict": "once",
		}), "not_found")
		assertRPCErrorReason(t, client.callRPC(t, 2, "custos.cards.resolve", map[string]any{
			"id": validID, "verdict": "maybe",
		}), "invalid_verdict")

		// Happy path once -> approved, deny -> denied.
		reg.resolveFn = func(_, verdict string) (string, error) {
			return registryAnswerState(verdict), nil
		}
		resp := client.callRPC(t, 3, "custos.cards.resolve", map[string]any{
			"id": validID, "verdict": "once",
		})
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		var out struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(resp.Result, &out); err != nil || out.State != "approved" {
			t.Fatalf("result = %s, want state=approved", string(resp.Result))
		}

		respDeny := client.callRPC(t, 4, "custos.cards.resolve", map[string]any{
			"id": validID, "verdict": "deny",
		})
		if respDeny.Error != nil {
			t.Fatalf("unexpected error: %+v", respDeny.Error)
		}
		if err := json.Unmarshal(respDeny.Result, &out); err != nil || out.State != "denied" {
			t.Fatalf("result = %s, want state=denied", string(respDeny.Result))
		}

		cases := []struct {
			err        error
			wantReason string
		}{
			{ErrCardAnswered, "already_answered"},
			{ErrStaleVerdict, "stale_verdict"},
			{ErrLocked, "locked"},
			{ErrIPAskOnly, "ip_ask_only"},
			{ErrBadSource, "bad_source"},
			{ErrNoSuchCard, "no_such_card"},
			{errors.New("door transport exploded"), "internal_error"},
		}
		for i, tc := range cases {
			reg.resolveFn = func(_, _ string) (string, error) { return "", tc.err }
			assertRPCErrorReason(t, client.callRPC(t, int64(10+i), "custos.cards.resolve", map[string]any{
				"id": validID, "verdict": "once",
			}), tc.wantReason)
		}
	})

	t.Run("hub fallback, hub 410 -> already_answered, and always hook", func(t *testing.T) {
		srv, tokStore, ts := newTestRPCServer(t, 2*time.Second)
		reg := newRPCTestCustosRegistry()
		reg.resolveFn = func(_, _ string) (string, error) { return "", ErrNoSuchCard }
		hub := NewApprovalHub(30 * time.Second)
		srv.SetCustosRegistry(reg)
		srv.SetApprovalHub(hub)

		var mintedTools []string
		var mintErr error
		srv.SetAlwaysRuleHook(func(toolName string) error {
			mintedTools = append(mintedTools, toolName)
			return mintErr
		})

		tok, err := tokStore.Create("custos-resolve-hub")
		if err != nil {
			t.Fatal(err)
		}
		client := dialAuthenticatedWS(t, ts.URL, tok)

		// Card 1: always via hub fallback mints always-rule and settles approved.
		id1, dec1 := hub.Register("s_MAIN", "mcp__cal__create", "create event", func(string) {})
		resp1 := client.callRPC(t, 30, "custos.cards.resolve", map[string]any{
			"id": id1, "verdict": "always",
		})
		if resp1.Error != nil {
			t.Fatalf("unexpected error: %+v", resp1.Error)
		}
		var out struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(resp1.Result, &out); err != nil || out.State != "approved" {
			t.Fatalf("result = %s, want approved", string(resp1.Result))
		}
		if !<-dec1 {
			t.Errorf("hub decision = false, want true")
		}
		if len(mintedTools) != 1 || mintedTools[0] != "mcp__cal__create" {
			t.Errorf("mintedTools = %v, want [mcp__cal__create]", mintedTools)
		}

		// Resolving the same settled hub card again maps hub 410 to already_answered.
		assertRPCErrorReason(t, client.callRPC(t, 31, "custos.cards.resolve", map[string]any{
			"id": id1, "verdict": "once",
		}), "already_answered")

		// Card 2: unknown registry error still falls through when hub owns the id,
		// and a mint failure degrades to once without flipping the ack.
		reg.resolveFn = func(_, _ string) (string, error) { return "", errors.New("door down") }
		mintErr = errors.New("vault locked")
		id2, dec2 := hub.Register("s_MAIN", "mcp__mail__send", "send mail", func(string) {})
		resp2 := client.callRPC(t, 32, "custos.cards.resolve", map[string]any{
			"id": id2, "verdict": "always",
		})
		if resp2.Error != nil {
			t.Fatalf("mint failure must not flip ack, got error: %+v", resp2.Error)
		}
		if err := json.Unmarshal(resp2.Result, &out); err != nil || out.State != "approved" {
			t.Fatalf("result = %s, want approved", string(resp2.Result))
		}
		if !<-dec2 {
			t.Errorf("hub decision 2 = false, want true")
		}
		if len(mintedTools) != 2 || mintedTools[1] != "mcp__mail__send" {
			t.Errorf("mintedTools = %v, want 2nd entry mcp__mail__send", mintedTools)
		}
	})
}
