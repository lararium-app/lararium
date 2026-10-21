package surface

// Issue #128: turn-approval cards (web chat + MCP rail) must appear on the
// custody card surface — listed by GET /v1/custos/cards, resolvable via
// POST /v1/custos/cards/{id}/resolve, and streamed on the SSE feed — so
// registry watchers (phone Approvals, CLI) see and settle them.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func setupTurnCardServer(t *testing.T, reg CustosRegistry, hub *ApprovalHub) (string, http.Handler) {
	t.Helper()
	store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	tokVal, err := store.Create("turncards-test")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Store:    store,
		Sessions: &fakeSessionSource{},
		Hub:      &fakeHub{},
		Cfg:      ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}
	if reg != nil {
		srv.SetCustosRegistry(reg)
	}
	if hub != nil {
		srv.SetApprovalHub(hub)
	}
	return tokVal, NewServer(srv)
}

func tcGet(t *testing.T, handler http.Handler, tok, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func tcPost(t *testing.T, handler http.Handler, tok, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func tcListCards(t *testing.T, handler http.Handler, tok string) []CustosCard {
	t.Helper()
	code, body := tcGet(t, handler, tok, "/v1/custos/cards")
	if code != http.StatusOK {
		t.Fatalf("GET cards status = %d, body = %s", code, body)
	}
	var resp struct {
		Cards []CustosCard `json:"cards"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	return resp.Cards
}

// A pending turn approval is listed on the registry endpoint with the
// frozen shape: id, tool, review, age/expiry; cell/cred stay empty.
func TestIssue128_TurnCards_Listed(t *testing.T) {
	hub := NewApprovalHub(30 * time.Second)
	tok, handler := setupTurnCardServer(t, nil, hub)

	id, _ := hub.Register("s_01TURN", "mcp:searxng.search_web", `query="lararium"`, func(string) {})

	cards := tcListCards(t, handler, tok)
	if len(cards) != 1 {
		t.Fatalf("cards = %d, want 1 (%v)", len(cards), cards)
	}
	c := cards[0]
	if c.ID != id {
		t.Errorf("id = %q, want %q", c.ID, id)
	}
	if c.Tool != "mcp:searxng.search_web" {
		t.Errorf("tool = %q", c.Tool)
	}
	if c.Review != `query="lararium"` {
		t.Errorf("review = %q", c.Review)
	}
	if c.Cell != "" || c.Cred != "" || c.Dest != "" {
		t.Errorf("turn card must not carry lane fields: %+v", c)
	}
	if c.ExpiresInS < 20 || c.ExpiresInS > 30 {
		t.Errorf("expires_in_s = %d, want ~30", c.ExpiresInS)
	}
}

// Registry (worker-lane) cards and turn cards coexist in one sorted list.
func TestIssue128_TurnCards_MergedWithRegistry(t *testing.T) {
	reg := NewFakeCustosRegistry()
	reg.PublishCard(CustosCard{ID: "a_worker000000000000000000", Cell: "c_w", Cred: "gmail", Tool: "gmail.send", Dest: "x@y.z"})
	hub := NewApprovalHub(30 * time.Second)
	tok, handler := setupTurnCardServer(t, reg, hub)

	id, _ := hub.Register("s_01TURN", "toolx", "args", func(string) {})
	reg.PublishCard(CustosCard{ID: "a_worker2000000000000000000", Cell: "c_w", Cred: "gmail", Tool: "gmail.send"})

	cards := tcListCards(t, handler, tok)
	if len(cards) != 3 {
		t.Fatalf("cards = %d, want 3 (%v)", len(cards), cards)
	}
	for i := 1; i < len(cards); i++ {
		if cards[i-1].ID > cards[i].ID {
			t.Errorf("cards not sorted by id: %v", cards)
			break
		}
	}
	var found bool
	for _, c := range cards {
		if c.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("turn card %s missing from merged list", id)
	}
}

// Custos worker-lane approvals on the hub (cred/cell set) are NOT
// re-published by the bridge — the door registry serves them (no doubles).
func TestIssue128_WorkerCards_NotDoubled(t *testing.T) {
	reg := NewFakeCustosRegistry()
	hub := NewApprovalHub(30 * time.Second)
	tok, handler := setupTurnCardServer(t, reg, hub)

	hub.RegisterCustos("c_w", "gmail", "to:x@y.z", "send", 30*time.Second, nil)

	cards := tcListCards(t, handler, tok)
	if len(cards) != 0 {
		t.Fatalf("worker-lane approval leaked into bridge: %v", cards)
	}
}

// Resolving through the registry endpoint settles the live turn: the
// decision channel fires, the card disappears, and a second resolve is
// already_answered.
func TestIssue128_TurnCards_ResolveOnce(t *testing.T) {
	hub := NewApprovalHub(30 * time.Second)
	tok, handler := setupTurnCardServer(t, nil, hub)

	id, decision := hub.Register("s_01TURN", "toolx", "args", func(string) {})

	code, body := tcPost(t, handler, tok, "/v1/custos/cards/"+id+"/resolve", `{"verdict":"once"}`)
	if code != http.StatusOK {
		t.Fatalf("resolve status = %d, body = %s", code, body)
	}
	if !strings.Contains(body, `"approved"`) {
		t.Errorf("body = %s, want approved state", body)
	}
	select {
	case allow := <-decision:
		if !allow {
			t.Errorf("decision = false, want true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decision channel never fired")
	}

	if cards := tcListCards(t, handler, tok); len(cards) != 0 {
		t.Errorf("settled card still listed: %v", cards)
	}

	code, body = tcPost(t, handler, tok, "/v1/custos/cards/"+id+"/resolve", `{"verdict":"once"}`)
	if code != http.StatusConflict || !strings.Contains(body, "already_answered") {
		t.Errorf("second resolve = %d %s, want 409 already_answered", code, body)
	}
}

func TestIssue128_TurnCards_ResolveDeny(t *testing.T) {
	hub := NewApprovalHub(30 * time.Second)
	tok, handler := setupTurnCardServer(t, nil, hub)

	id, decision := hub.Register("s_01TURN", "toolx", "args", func(string) {})

	code, body := tcPost(t, handler, tok, "/v1/custos/cards/"+id+"/resolve", `{"verdict":"deny"}`)
	if code != http.StatusOK || !strings.Contains(body, `"denied"`) {
		t.Fatalf("resolve status = %d, body = %s", code, body)
	}
	select {
	case allow := <-decision:
		if allow {
			t.Errorf("decision = true, want false on deny")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decision channel never fired")
	}
}

// Registry ownership is unchanged: a known worker card resolves through
// the registry path and never consults the hub.
func TestIssue128_RegistryPath_Priority(t *testing.T) {
	reg := NewFakeCustosRegistry()
	reg.PublishCard(CustosCard{ID: "a_worker000000000000000000", Cell: "c_w", Cred: "gmail", Tool: "gmail.send"})
	hub := NewApprovalHub(30 * time.Second)
	tok, handler := setupTurnCardServer(t, reg, hub)

	code, body := tcPost(t, handler, tok, "/v1/custos/cards/a_worker000000000000000000/resolve", `{"verdict":"once"}`)
	if code != http.StatusOK {
		t.Fatalf("worker resolve = %d %s, want 200", code, body)
	}
	for _, c := range reg.Snapshot() {
		if c.ID == "a_worker000000000000000000" {
			t.Errorf("worker card still pending after resolve")
		}
	}
}

// Unknown ids stay 404 no_such_card even with the hub bridged.
func TestIssue128_UnknownID_Still404(t *testing.T) {
	hub := NewApprovalHub(30 * time.Second)
	tok, handler := setupTurnCardServer(t, nil, hub)

	code, body := tcPost(t, handler, tok, "/v1/custos/cards/a_000000000000000000000000/resolve", `{"verdict":"once"}`)
	if code != http.StatusNotFound || !strings.Contains(body, "no_such_card") {
		t.Fatalf("unknown resolve = %d %s, want 404 no_such_card", code, body)
	}
}

// Hub-level observer contract: card on Register, gone on settle; worker
// lanes silent; cancel stops delivery.
func TestIssue128_HubObserver_CardAndGone(t *testing.T) {
	hub := NewApprovalHub(30 * time.Second)
	events := make(chan CustosEvent, 8)
	cancel := hub.SubscribeCustos(func(card *CustosCard, gone *CustosGone) {
		events <- CustosEvent{Card: card, Gone: gone}
	})

	id, _ := hub.Register("s_01OBS", "tooly", "argz", func(string) {})
	ev := <-events
	if ev.Card == nil || ev.Card.ID != id || ev.Card.Tool != "tooly" {
		t.Fatalf("pending event = %+v, want card for %s", ev, id)
	}

	hub.Resolve("s_01OBS", id, true)
	ev = <-events
	if ev.Gone == nil || ev.Gone.ID != id || ev.Gone.State != "approved" {
		t.Fatalf("terminal event = %+v, want gone approved", ev)
	}

	// Worker lane: silent.
	hub.RegisterCustos("c_w", "gmail", "to:x@y.z", "send", 30*time.Second, nil)
	cancel()

	// After cancel: no further events.
	id2, _ := hub.Register("s_01OBS", "toolz", "a", func(string) {})
	hub.Resolve("s_01OBS", id2, false)
	select {
	case ev := <-events:
		t.Fatalf("event after cancel: %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
}

// Timeout maps onto the §6.4b gone vocabulary (expired).
func TestIssue128_HubObserver_TimeoutIsExpired(t *testing.T) {
	hub := NewApprovalHub(80 * time.Millisecond)
	events := make(chan CustosEvent, 4)
	hub.SubscribeCustos(func(card *CustosCard, gone *CustosGone) {
		events <- CustosEvent{Card: card, Gone: gone}
	})
	hub.Register("s_01TO", "tool", "a", func(string) {})
	<-events // pending card

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Gone != nil {
				if ev.Gone.State != "expired" {
					t.Fatalf("gone state = %q, want expired", ev.Gone.State)
				}
				return
			}
		case <-deadline:
			t.Fatal("no gone event after timeout")
		}
	}
}

// --- Issue #132: verdict "always" must mint a card-derived rule ---

func setupTurnCardServerWithHook(t *testing.T, hub *ApprovalHub, hook func(string) error) (string, http.Handler) {
	t.Helper()
	store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	tokVal, err := store.Create("turncards-132")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Store:    store,
		Sessions: &fakeSessionSource{},
		Hub:      &fakeHub{},
		Cfg:      ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}
	srv.SetApprovalHub(hub)
	srv.SetAlwaysRuleHook(hook)
	return tokVal, NewServer(srv)
}

func TestIssue132_AlwaysMintsRule(t *testing.T) {
	hub := NewApprovalHub(30 * time.Second)
	var minted []string
	tok, handler := setupTurnCardServerWithHook(t, hub, func(name string) error {
		minted = append(minted, name)
		return nil
	})

	id, decision := hub.Register("s_01TURN", "mcp__github__create_issue", `{"repo":"x"}`, func(string) {})

	code, body := tcPost(t, handler, tok, "/v1/custos/cards/"+id+"/resolve", `{"verdict":"always"}`)
	if code != http.StatusOK || !strings.Contains(body, `"approved"`) {
		t.Fatalf("resolve = %d %s, want 200 approved", code, body)
	}
	select {
	case allow := <-decision:
		if !allow {
			t.Error("decision = false, want true on always")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decision channel never fired")
	}
	if len(minted) != 1 || minted[0] != "mcp__github__create_issue" {
		t.Errorf("minted = %v, want exactly [mcp__github__create_issue]", minted)
	}
}

func TestIssue132_OnceAndDenyDoNotMint(t *testing.T) {
	for _, verdict := range []string{"once", "deny"} {
		hub := NewApprovalHub(30 * time.Second)
		mints := 0
		tok, handler := setupTurnCardServerWithHook(t, hub, func(string) error {
			mints++
			return nil
		})
		id, _ := hub.Register("s_01TURN", "toolx", "args", func(string) {})
		if code, body := tcPost(t, handler, tok, "/v1/custos/cards/"+id+"/resolve", `{"verdict":"`+verdict+`"}`); code != http.StatusOK {
			t.Fatalf("verdict %s: resolve = %d %s", verdict, code, body)
		}
		if mints != 0 {
			t.Errorf("verdict %s minted %d rules, want 0", verdict, mints)
		}
	}
}

// A mint failure must not flip the resolve ack: the approval already
// settled honestly, and a false 500 would invite double-taps. The loud
// degradation is the log line (the hook ran; its error is swallowed).
func TestIssue132_MintFailureStillAcks(t *testing.T) {
	hub := NewApprovalHub(30 * time.Second)
	tok, handler := setupTurnCardServerWithHook(t, hub, func(string) error {
		return errors.New("mint failed (test)")
	})
	id, decision := hub.Register("s_01TURN", "toolx", "args", func(string) {})
	code, body := tcPost(t, handler, tok, "/v1/custos/cards/"+id+"/resolve", `{"verdict":"always"}`)
	if code != http.StatusOK || !strings.Contains(body, `"approved"`) {
		t.Fatalf("resolve = %d %s, want 200 approved despite mint failure", code, body)
	}
	<-decision
}

func TestIssue132_NameOf(t *testing.T) {
	hub := NewApprovalHub(30 * time.Second)
	id, _ := hub.Register("s_01TURN", "tooly", "args", func(string) {})
	if name, ok := hub.NameOf(id); !ok || name != "tooly" {
		t.Errorf("NameOf = %q,%v want tooly,true", name, ok)
	}
	if _, ok := hub.NameOf("a_missing0000000000000000"); ok {
		t.Error("NameOf of unknown id returned ok")
	}
}
