package surface

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

// audienceFixture is one hearth with two paired devices (A and B) over the
// real token store, Penatus source, and turn hub.
type audienceFixture struct {
	srv        *Server
	hub        *Hub
	ts         *httptest.Server
	store      *TokenStore
	tokA, tokB string
	idA, idB   string
}

func newAudienceFixture(t *testing.T) *audienceFixture {
	t.Helper()
	sp := &stubProvider{
		name: "stub",
		caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
		replies: func(_ int, _ []router.Message) (*router.Completion, error) {
			return &router.Completion{Text: "done", Model: "stub"}, nil
		},
	}
	hub := setupHub(t, sp)
	store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Store:         store,
		Sessions:      NewPenatusSource(hub.hearthHome, hub),
		Hub:           hub,
		wsAuthTimeout: 2 * time.Second,
	}
	ts := httptest.NewUnstartedServer(NewServer(srv))
	ts.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, http.LocalAddrContextKey, c.LocalAddr())
	}
	ts.Start()
	t.Cleanup(ts.Close)

	f := &audienceFixture{srv: srv, hub: hub, ts: ts, store: store}
	if f.tokA, err = store.Create("device-a"); err != nil {
		t.Fatal(err)
	}
	if f.tokB, err = store.Create("device-b"); err != nil {
		t.Fatal(err)
	}
	var ok bool
	if f.idA, ok = store.VerifyIdentity(f.tokA); !ok {
		t.Fatal("VerifyIdentity(A) failed")
	}
	if f.idB, ok = store.VerifyIdentity(f.tokB); !ok {
		t.Fatal("VerifyIdentity(B) failed")
	}
	return f
}

// do sends one REST request as the holder of tok and returns status + body.
func (f *audienceFixture) do(t *testing.T, tok, method, path, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func (f *audienceFixture) createREST(t *testing.T, tok string, private bool) SessionInfo {
	t.Helper()
	body := `{"title":"t"}`
	if private {
		body = `{"title":"t","private":true}`
	}
	code, out := f.do(t, tok, http.MethodPost, "/v1/sessions", body)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", code, out)
	}
	var info SessionInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("decode create: %v (%s)", err, out)
	}
	return info
}

func (f *audienceFixture) listREST(t *testing.T, tok string) map[string]map[string]any {
	t.Helper()
	code, out := f.do(t, tok, http.MethodGet, "/v1/sessions", "")
	if code != http.StatusOK {
		t.Fatalf("list = %d %s", code, out)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode list: %v (%s)", err, out)
	}
	byID := map[string]map[string]any{}
	for _, r := range rows {
		id, ok := r["id"].(string)
		if !ok {
			t.Fatalf("list row without string id: %v", r)
		}
		byID[id] = r
	}
	return byID
}

// subscribedCaller reports whether any server-side connection of caller
// holds a live sessions.subscribe registration on session id.
func subscribedCaller(s *Server, id, caller string) bool {
	s.rpcSubsMu.RLock()
	defer s.rpcSubsMu.RUnlock()
	for conn := range s.rpcSubs[id] {
		if conn.caller == caller {
			return true
		}
	}
	return false
}

func rpcListIDs(t *testing.T, c *testWSClient, reqID int64) map[string]bool {
	t.Helper()
	resp := c.callRPC(t, reqID, "sessions.list", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("sessions.list error: %+v", resp.Error)
	}
	var res struct {
		Sessions []SessionInfo `json:"sessions"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, s := range res.Sessions {
		ids[s.ID] = true
	}
	return ids
}

func TestVerifyIdentity_DerivedStableDistinct(t *testing.T) {
	f := newAudienceFixture(t)
	if !strings.HasPrefix(f.idA, "t_") || len(f.idA) != 2+tokenIDHexLen {
		t.Fatalf("identity %q: want t_ + %d hex chars", f.idA, tokenIDHexLen)
	}
	if f.idA == f.idB {
		t.Fatalf("two tokens share identity %q", f.idA)
	}
	if again, ok := f.store.VerifyIdentity(f.tokA); !ok || again != f.idA {
		t.Fatalf("identity not stable: %q then %q", f.idA, again)
	}
	if strings.Contains(f.tokA, f.idA[2:]) {
		t.Fatal("identity must derive from the stored hash, not the token text")
	}
	if id, ok := f.store.VerifyIdentity("lar1_notarealtoken"); ok || id != "" {
		t.Fatalf("unknown token verified: %q %v", id, ok)
	}
	if n, err := f.store.RevokeLabel("device-a"); err != nil || n != 1 {
		t.Fatalf("revoke: %d %v", n, err)
	}
	if _, ok := f.store.VerifyIdentity(f.tokA); ok {
		t.Fatal("revoked token still yields an identity")
	}
	if f.store.Verify(f.tokA) {
		t.Fatal("Verify disagrees with VerifyIdentity after revoke")
	}
}

func TestAudience_CreateRecordsCreatorAndAudience(t *testing.T) {
	f := newAudienceFixture(t)
	priv := f.createREST(t, f.tokA, true)
	shared := f.createREST(t, f.tokA, false)
	if !priv.Private || shared.Private {
		t.Fatalf("private flags: private=%v shared=%v", priv.Private, shared.Private)
	}

	read := func(id string) penatus.SessionHeader {
		data, err := os.ReadFile(filepath.Join(f.hub.hearthHome, "sessions", id, "session.json"))
		if err != nil {
			t.Fatal(err)
		}
		var h penatus.SessionHeader
		if err := json.Unmarshal(data, &h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	h := read(priv.ID)
	if h.Creator == nil || *h.Creator != f.idA || len(h.Audience) != 1 || h.Audience[0] != f.idA {
		t.Fatalf("private header creator=%v audience=%v, want %s/[%s]", h.Creator, h.Audience, f.idA, f.idA)
	}
	if h.Parent == nil || *h.Parent != "main" || h.Title == nil || *h.Title != "t" {
		t.Fatalf("private header lost parent/title: %+v", h)
	}
	h = read(shared.ID)
	if h.Creator == nil || *h.Creator != f.idA || len(h.Audience) != 0 {
		t.Fatalf("shared header creator=%v audience=%v, want creator %s and no audience", h.Creator, h.Audience, f.idA)
	}

	// The audience itself never leaves the server.
	_, out := f.do(t, f.tokA, http.MethodGet, "/v1/sessions", "")
	if strings.Contains(out, f.idA) || strings.Contains(out, "audience") || strings.Contains(out, "creator") {
		t.Fatalf("list leaks audience record: %s", out)
	}
}

func TestAudience_RESTMatrix(t *testing.T) {
	f := newAudienceFixture(t)
	priv := f.createREST(t, f.tokA, true)
	shared := f.createREST(t, f.tokB, false)

	// List: A sees its private session flagged; B sees no row for it.
	rowsA, rowsB := f.listREST(t, f.tokA), f.listREST(t, f.tokB)
	if r, ok := rowsA[priv.ID]; !ok || r["private"] != true {
		t.Fatalf("A's list row for private session = %v", r)
	}
	if _, ok := rowsB[priv.ID]; ok {
		t.Fatal("B lists A's private session")
	}
	for _, rows := range []map[string]map[string]any{rowsA, rowsB} {
		if _, ok := rows["main"]; !ok {
			t.Fatal("main missing from a device's list")
		}
		if r, ok := rows[shared.ID]; !ok || r["private"] != nil {
			t.Fatalf("shared session row = %v (want present, no private key)", r)
		}
	}

	// Every direct path answers B exactly as it answers an unknown id.
	unknown := "s_" + strings.Repeat("0", 26)
	paths := []struct{ method, suffix, body string }{
		{http.MethodGet, "/events", ""},
		{http.MethodPost, "/cancel", ""},
		{http.MethodPost, "/approvals/a_0000000000", `{"decision":"allow"}`},
		{http.MethodPatch, "", `{"model_pin":null}`},
		{http.MethodDelete, "", ""},
	}
	for _, p := range paths {
		code, body := f.do(t, f.tokB, p.method, "/v1/sessions/"+priv.ID+p.suffix, p.body)
		ucode, ubody := f.do(t, f.tokB, p.method, "/v1/sessions/"+unknown+p.suffix, p.body)
		if code != http.StatusNotFound || code != ucode || body != ubody {
			t.Errorf("%s …%s hidden=%d %s unknown=%d %s, want identical 404s", p.method, p.suffix, code, body, ucode, ubody)
		}
	}
	// messages: an unknown id does not 404 today, so pin the hidden reply.
	if code, body := f.do(t, f.tokB, http.MethodPost, "/v1/sessions/"+priv.ID+"/messages", `{"text":"hi"}`); code != http.StatusNotFound || body != `{"error":"not found"}` {
		t.Errorf("messages hidden = %d %s, want 404 not found", code, body)
	}
	if f.hub.InFlight(priv.ID) {
		t.Fatal("hidden POST messages started a turn")
	}

	// The audience still has full access, and the hidden DELETE above did
	// not remove anything.
	if code, body := f.do(t, f.tokA, http.MethodGet, "/v1/sessions/"+priv.ID+"/events", ""); code != http.StatusOK {
		t.Fatalf("A events = %d %s", code, body)
	}
	if code, body := f.do(t, f.tokB, http.MethodGet, "/v1/sessions/"+shared.ID+"/events", ""); code != http.StatusOK {
		t.Fatalf("B events on shared = %d %s", code, body)
	}
	if code, body := f.do(t, f.tokA, http.MethodDelete, "/v1/sessions/"+priv.ID, ""); code != http.StatusNoContent {
		t.Fatalf("A delete own private = %d %s", code, body)
	}
}

func TestAudience_LegacySessionsSharedWithAll(t *testing.T) {
	f := newAudienceFixture(t)
	// A session written by an older build: no creator, no audience.
	legacy, err := f.srv.Sessions.Create("old", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{f.tokA, f.tokB} {
		if _, ok := f.listREST(t, tok)[legacy.ID]; !ok {
			t.Fatal("legacy session hidden from a device")
		}
		if code, _ := f.do(t, tok, http.MethodGet, "/v1/sessions/"+legacy.ID+"/events", ""); code != http.StatusOK {
			t.Fatalf("legacy events = %d", code)
		}
	}
}

func TestAudience_RPCMatrix(t *testing.T) {
	f := newAudienceFixture(t)
	a := dialAuthenticatedWS(t, f.ts.URL, f.tokA)
	b := dialAuthenticatedWS(t, f.ts.URL, f.tokB)

	resp := a.callRPC(t, 1, "sessions.create", map[string]any{"title": "p", "private": true})
	if resp.Error != nil {
		t.Fatalf("create: %+v", resp.Error)
	}
	var priv SessionInfo
	if err := json.Unmarshal(resp.Result, &priv); err != nil || !priv.Private {
		t.Fatalf("create result %s (private=%v err=%v)", resp.Result, priv.Private, err)
	}

	if !rpcListIDs(t, a, 2)[priv.ID] {
		t.Fatal("A's sessions.list misses its private session")
	}
	if rpcListIDs(t, b, 3)[priv.ID] {
		t.Fatal("B's sessions.list shows A's private session")
	}

	calls := []struct {
		method string
		params map[string]any
		reason string
	}{
		{"sessions.subscribe", map[string]any{"id": priv.ID}, "not_found"},
		{"sessions.send", map[string]any{"id": priv.ID, "text": "hi"}, "not_found"},
		{"sessions.cancel", map[string]any{"id": priv.ID}, "no_turn_in_flight"},
		{"sessions.delete", map[string]any{"id": priv.ID}, "no_such_session"},
		{"sessions.configure", map[string]any{"id": priv.ID, "model_pin": nil}, "no_such_session"},
		{"sessions.approvals.resolve", map[string]any{"session_id": priv.ID, "approval_id": "a_0000000000", "decision": "allow"}, "not_found"},
	}
	for i, c := range calls {
		assertRPCErrorReason(t, b.callRPC(t, int64(10+i), c.method, c.params), c.reason)
	}
	if subscribedCaller(f.srv, priv.ID, f.idB) {
		t.Fatal("hidden subscribe left a registration behind")
	}

	if r := a.callRPC(t, 20, "sessions.subscribe", map[string]any{"id": priv.ID}); r.Error != nil {
		t.Fatalf("A subscribe own private: %+v", r.Error)
	}
}

func TestAudience_RPCConfigureRules(t *testing.T) {
	f := newAudienceFixture(t)
	a := dialAuthenticatedWS(t, f.ts.URL, f.tokA)
	b := dialAuthenticatedWS(t, f.ts.URL, f.tokB)

	resp := a.callRPC(t, 1, "sessions.create", map[string]any{"title": "s"})
	if resp.Error != nil {
		t.Fatalf("create: %+v", resp.Error)
	}
	var s SessionInfo
	_ = json.Unmarshal(resp.Result, &s)

	// Only the creator may change the audience; the main session is fixed.
	assertRPCErrorReason(t, b.callRPC(t, 2, "sessions.configure", map[string]any{"id": s.ID, "private": true}), "not_creator")
	assertRPCErrorReason(t, a.callRPC(t, 3, "sessions.configure", map[string]any{"id": "main", "private": true}), "session_shared")
	assertRPCErrorReason(t, a.callRPC(t, 4, "sessions.configure", map[string]any{"id": s.ID, "private": "yes"}), "invalid_request")
	assertRPCErrorReason(t, a.callRPC(t, 5, "sessions.configure", map[string]any{"id": s.ID, "private": nil}), "invalid_request")
	// A failed audience change wrote nothing: the revision is unchanged.
	assertRPCErrorReason(t, a.callRPC(t, 6, "sessions.configure", map[string]any{"id": s.ID, "private": true, "expected_revision": 7}), "revision_conflict")

	// Scope down under CAS, then widen again; B follows on the same socket.
	resp = a.callRPC(t, 7, "sessions.configure", map[string]any{"id": s.ID, "private": true, "expected_revision": 0})
	if resp.Error != nil {
		t.Fatalf("scope down: %+v", resp.Error)
	}
	var res struct {
		Applied map[string]string `json:"applied"`
		Session SessionInfo       `json:"session"`
	}
	_ = json.Unmarshal(resp.Result, &res)
	if res.Applied["private"] != "ok" || !res.Session.Private || res.Session.Revision != 1 {
		t.Fatalf("scope-down result %s", resp.Result)
	}
	if rpcListIDs(t, b, 8)[s.ID] {
		t.Fatal("B still lists the session after scope-down")
	}
	resp = a.callRPC(t, 9, "sessions.configure", map[string]any{"id": s.ID, "private": false, "expected_revision": 1})
	if resp.Error != nil {
		t.Fatalf("widen: %+v", resp.Error)
	}
	if !rpcListIDs(t, b, 10)[s.ID] {
		t.Fatal("B does not list the session after widening (no reconnect)")
	}
	// The creator rule survives widening: B still cannot scope it.
	assertRPCErrorReason(t, b.callRPC(t, 11, "sessions.configure", map[string]any{"id": s.ID, "private": true}), "not_creator")
}

func TestAudience_ScopeDownStopsLiveDelivery(t *testing.T) {
	f := newAudienceFixture(t)
	a := dialAuthenticatedWS(t, f.ts.URL, f.tokA)
	b := dialAuthenticatedWS(t, f.ts.URL, f.tokB)

	resp := a.callRPC(t, 1, "sessions.create", map[string]any{"title": "s"})
	var s SessionInfo
	_ = json.Unmarshal(resp.Result, &s)
	for i, c := range []*testWSClient{a, b} {
		if r := c.callRPC(t, int64(2+i), "sessions.subscribe", map[string]any{"id": s.ID}); r.Error != nil {
			t.Fatalf("subscribe: %+v", r.Error)
		}
	}

	if r := a.callRPC(t, 4, "sessions.configure", map[string]any{"id": s.ID, "private": true}); r.Error != nil {
		t.Fatalf("scope down: %+v", r.Error)
	}
	if subscribedCaller(f.srv, s.ID, f.idB) {
		t.Fatal("B's subscription survived scope-down")
	}
	if !subscribedCaller(f.srv, s.ID, f.idA) {
		t.Fatal("A's subscription was dropped by its own scope-down")
	}

	if r := a.callRPC(t, 5, "sessions.send", map[string]any{"id": s.ID, "text": "hello"}); r.Error != nil {
		t.Fatalf("send: %+v", r.Error)
	}
	for {
		n := a.readNotification(t)
		if bytes.Contains(n.Params, []byte(`"turn_done"`)) {
			break
		}
	}
	// Every notify for the turn has returned once the session leaves the
	// running map, so any frame queued for B is already on its socket.
	deadline := time.Now().Add(5 * time.Second)
	for f.hub.InFlight(s.ID) {
		if time.Now().After(deadline) {
			t.Fatal("turn never left the running map")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// B's next frame must be this reply, not a session.event.
	_ = rpcListIDs(t, b, 6)
}

func TestAudience_CorruptHeaderFailsClosed(t *testing.T) {
	f := newAudienceFixture(t)
	priv := f.createREST(t, f.tokA, true)
	path := filepath.Join(f.hub.hearthHome, "sessions", priv.ID, "session.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{f.tokA, f.tokB} {
		if code, body := f.do(t, tok, http.MethodGet, "/v1/sessions/"+priv.ID+"/events", ""); code != http.StatusNotFound {
			t.Fatalf("events on corrupt header = %d %s, want 404", code, body)
		}
	}
}

func TestAudience_PrivateNeedsIdentity(t *testing.T) {
	hub := setupHub(t, &stubProvider{name: "stub"})
	src := NewPenatusSource(hub.hearthHome, hub)
	before, _ := os.ReadDir(filepath.Join(hub.hearthHome, "sessions"))
	if _, err := src.CreateWith(SessionCreateRequest{Title: "x", Private: true}); !errors.Is(err, errPrivateNeedsIdentity) {
		t.Fatalf("anonymous private create err = %v", err)
	}
	after, _ := os.ReadDir(filepath.Join(hub.hearthHome, "sessions"))
	if len(after) != len(before) {
		t.Fatal("rejected private create still wrote a session")
	}
	if audienceAllows([]string{"t_0123456789"}, "") {
		t.Fatal("anonymous caller admitted to a scoped audience")
	}
	if !audienceAllows(nil, "") {
		t.Fatal("anonymous caller refused a shared session")
	}
}

// gatedAudienceSource parks the next armed SessionAudience call until
// released, so a test can act inside a sessions.subscribe hold-back window.
type gatedAudienceSource struct {
	*PenatusSource

	armed   chan struct{}
	entered chan struct{}
	release chan struct{}
}

func (g *gatedAudienceSource) SessionAudience(id string) ([]string, error) {
	select {
	case <-g.armed:
		close(g.entered)
		<-g.release
	default:
	}
	return g.PenatusSource.SessionAudience(id)
}

func TestAudience_HiddenSubscribeDiscardsHeldEvents(t *testing.T) {
	f := newAudienceFixture(t)
	ps, ok := f.srv.Sessions.(*PenatusSource)
	if !ok {
		t.Fatal("fixture sessions source is not a PenatusSource")
	}
	gated := &gatedAudienceSource{
		PenatusSource: ps,
		armed:         make(chan struct{}, 1),
		entered:       make(chan struct{}),
		release:       make(chan struct{}),
	}
	f.srv.Sessions = gated
	priv, err := gated.CreateWith(SessionCreateRequest{Title: "p", Creator: f.idA, Private: true})
	if err != nil {
		t.Fatal(err)
	}
	b := dialAuthenticatedWS(t, f.ts.URL, f.tokB)

	gated.armed <- struct{}{}
	req, err := EncodeRPCRequest(1, "sessions.subscribe", map[string]any{"id": priv.ID})
	if err != nil {
		t.Fatal(err)
	}
	b.writeMaskedText(t, req)
	select {
	case <-gated.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("subscribe never reached the audience check")
	}
	// B is registered with its window open: an event from another device's
	// turn on the private session is held for B right now.
	f.srv.notifyRPCSessionEvent(priv.ID, json.RawMessage(`{"t":"delta","text":"private text"}`))
	close(gated.release)

	op, payload := b.readServerFrame(t)
	if op != wsOpText {
		t.Fatalf("opcode %d", op)
	}
	resp, err := DecodeRPCResponse(payload)
	if err != nil || resp.ID != 1 {
		t.Fatalf("first frame is not the subscribe reply: %s", payload)
	}
	assertRPCErrorReason(t, resp, "not_found")
	// The held event must have been discarded: B's next frame is this reply.
	_ = rpcListIDs(t, b, 2)
}

func TestAudience_ClosedSubscriptionDiscardsQueue(t *testing.T) {
	// A nil conn panics on any write, so reaching Notify fails the test.
	sub := &rpcSessionSub{pending: 1}
	sub.deliver(nil, "s_x", json.RawMessage(`{"t":"delta"}`))
	sub.close()
	sub.deliver(nil, "s_x", json.RawMessage(`{"t":"delta"}`))
	sub.endSubscribe(nil, "s_x")
	if len(sub.queued) != 0 {
		t.Fatalf("closed subscription kept %d queued events", len(sub.queued))
	}
}

func TestAudience_DropRPCSubsOutside(t *testing.T) {
	s := &Server{}
	ca, cb := &RPCConn{caller: "t_aaaaaaaaaa"}, &RPCConn{caller: "t_bbbbbbbbbb"}
	subA, subB := &rpcSessionSub{}, &rpcSessionSub{}
	s.rpcSubs = map[string]map[*RPCConn]*rpcSessionSub{"s_x": {ca: subA, cb: subB}}
	s.dropRPCSubsOutside("s_x", []string{"t_aaaaaaaaaa"})
	if !s.isRPCSessionSubscribed("s_x", ca) || s.isRPCSessionSubscribed("s_x", cb) {
		t.Fatal("drop kept the wrong registrations")
	}
	if subA.closed || !subB.closed {
		t.Fatalf("closed flags A=%v B=%v", subA.closed, subB.closed)
	}
	s.dropRPCSubsOutside("s_x", nil) // shared: no-op
	if !s.isRPCSessionSubscribed("s_x", ca) {
		t.Fatal("empty audience dropped a registration")
	}
}
