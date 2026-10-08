package surface

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/router"
)

// FakeCustosRegistry is an in-memory test implementation of CustosRegistry
// with manual publish control and configurable resolve outcomes.
type FakeCustosRegistry struct {
	mu          sync.Mutex
	cards       map[string]CustosCard
	subscribers map[int]func(*CustosCard, *CustosGone)
	nextSubID   int
	errMap      map[string]error
	resolveHook func(id, verdict string) (string, error)
	onSubscribe func()
}

// NewFakeCustosRegistry creates a new initialized FakeCustosRegistry.
func NewFakeCustosRegistry() *FakeCustosRegistry {
	return &FakeCustosRegistry{
		cards:       make(map[string]CustosCard),
		subscribers: make(map[int]func(*CustosCard, *CustosGone)),
		errMap:      make(map[string]error),
	}
}

// SetOnSubscribe sets a callback that fires whenever a subscriber attaches.
func (r *FakeCustosRegistry) SetOnSubscribe(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onSubscribe = fn
}

// Snapshot returns the current pending custody cards sorted by ID for determinism.
func (r *FakeCustosRegistry) Snapshot() []CustosCard {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *FakeCustosRegistry) snapshotLocked() []CustosCard {
	res := make([]CustosCard, 0, len(r.cards))
	for _, c := range r.cards {
		res = append(res, c)
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].ID < res[j].ID
	})
	return res
}

// Subscribe registers a subscriber callback. The returned cancel func unregisters it.
func (r *FakeCustosRegistry) Subscribe(f func(card *CustosCard, gone *CustosGone)) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.subscribeLocked(f)
}

func (r *FakeCustosRegistry) subscribeLocked(f func(card *CustosCard, gone *CustosGone)) func() {
	id := r.nextSubID
	r.nextSubID++
	r.subscribers[id] = f
	if r.onSubscribe != nil {
		go r.onSubscribe()
	}
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.subscribers, id)
	}
}

// Attach returns snapshot and activates live subscription under a single lock
// (mirroring the hearthd HELLO-replay pattern per CUSTOS-SPEC §6.4b).
func (r *FakeCustosRegistry) Attach() ([]CustosCard, func(), <-chan CustosEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cards := r.snapshotLocked()
	ch := make(chan CustosEvent, 64)
	cancel := r.subscribeLocked(func(c *CustosCard, g *CustosGone) {
		select {
		case ch <- CustosEvent{Card: c, Gone: g}:
		default:
		}
	})
	return cards, cancel, ch
}

// Resolve handles a verdict for the card.
func (r *FakeCustosRegistry) Resolve(id, verdict string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.resolveHook != nil {
		return r.resolveHook(id, verdict)
	}
	if err, ok := r.errMap[id]; ok {
		return "", err
	}
	card, exists := r.cards[id]
	if !exists {
		return "", ErrNoSuchCard
	}

	delete(r.cards, id)
	var state string
	if verdict == "deny" {
		state = "denied"
	} else {
		state = "approved"
	}
	gone := CustosGone{ID: card.ID, State: state, Reason: "web"}
	for _, sub := range r.subscribers {
		sub(nil, &gone)
	}
	return state, nil
}

// PublishCard adds card to pending and notifies all subscribers.
func (r *FakeCustosRegistry) PublishCard(card CustosCard) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cards[card.ID] = card
	c := card
	for _, sub := range r.subscribers {
		sub(&c, nil)
	}
}

// PublishGone removes card from pending and notifies all subscribers.
func (r *FakeCustosRegistry) PublishGone(gone CustosGone) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cards, gone.ID)
	g := gone
	for _, sub := range r.subscribers {
		sub(nil, &g)
	}
}

// SetResolveErr sets an error to be returned when Resolving id.
func (r *FakeCustosRegistry) SetResolveErr(id string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errMap[id] = err
}

// SetResolveHook sets a custom resolver hook.
func (r *FakeCustosRegistry) SetResolveHook(hook func(id, verdict string) (string, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolveHook = hook
}

// Helper to set up a test server with valid token.
func setupV16Server(t *testing.T, reg CustosRegistry) (*Server, string, http.Handler) {
	t.Helper()
	store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := store.Create("v16-test")
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
	return srv, tok, NewServer(srv)
}

// V16: GET /v1/custos/cards -> 200 {"cards":[]} with empty or unset registry.
func TestV16_GET_Cards_Empty(t *testing.T) {
	t.Run("unset registry", func(t *testing.T) {
		_, tok, handler := setupV16Server(t, nil)
		req := httptest.NewRequest(http.MethodGet, "/v1/custos/cards", nil)
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var resp struct {
			Cards []CustosCard `json:"cards"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if resp.Cards == nil || len(resp.Cards) != 0 {
			t.Errorf("cards = %v, want empty non-nil slice", resp.Cards)
		}
		if strings.Contains(w.Body.String(), `"cards":null`) {
			t.Errorf("response must not contain null cards: %s", w.Body.String())
		}
	})

	t.Run("empty registry", func(t *testing.T) {
		reg := NewFakeCustosRegistry()
		_, tok, handler := setupV16Server(t, reg)
		req := httptest.NewRequest(http.MethodGet, "/v1/custos/cards", nil)
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		var resp struct {
			Cards []CustosCard `json:"cards"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if resp.Cards == nil || len(resp.Cards) != 0 {
			t.Errorf("cards = %v, want empty non-nil slice", resp.Cards)
		}
	})
}

// V16: GET /v1/custos/cards -> 200 {"cards":[...]} (§6.4a wire shape).
func TestV16_GET_Cards_PendingList(t *testing.T) {
	reg := NewFakeCustosRegistry()
	c1 := CustosCard{
		ID:         "a_card12345678901",
		Cell:       "cell-alpha",
		Cred:       "cred-openai",
		Tool:       "bash_tool",
		Dest:       "api.openai.com",
		Review:     "run ls",
		AgeS:       12,
		ExpiresInS: 288,
	}
	c2 := CustosCard{
		ID:         "a_card12345678902",
		Cell:       "cell-beta",
		Cred:       "cred-anthropic",
		Tool:       "web_search",
		Dest:       "search.api",
		Review:     "query: hello",
		AgeS:       5,
		ExpiresInS: 295,
	}
	reg.PublishCard(c1)
	reg.PublishCard(c2)

	_, tok, handler := setupV16Server(t, reg)
	req := httptest.NewRequest(http.MethodGet, "/v1/custos/cards", nil)
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Cards []CustosCard `json:"cards"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(resp.Cards) != 2 {
		t.Fatalf("len(cards) = %d, want 2", len(resp.Cards))
	}
	if resp.Cards[0].ID != c1.ID || resp.Cards[0].Tool != c1.Tool || resp.Cards[0].Dest != c1.Dest {
		t.Errorf("card 0 mismatch: %+v", resp.Cards[0])
	}
	if resp.Cards[1].ID != c2.ID || resp.Cards[1].Cred != c2.Cred {
		t.Errorf("card 1 mismatch: %+v", resp.Cards[1])
	}
}

// V16: POST /v1/custos/cards/{id}/resolve happy paths (once, always, deny).
func TestV16_Resolve_HappyPath(t *testing.T) {
	reg := NewFakeCustosRegistry()
	c1 := CustosCard{ID: "a_card00000000001", Cred: "k1"}
	c2 := CustosCard{ID: "a_card00000000002", Cred: "k2"}
	c3 := CustosCard{ID: "a_card00000000003", Cred: "k3"}
	reg.PublishCard(c1)
	reg.PublishCard(c2)
	reg.PublishCard(c3)

	_, tok, handler := setupV16Server(t, reg)

	// 1) Allow once -> 200 {"state":"approved"}
	{
		body := `{"verdict":"once"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/custos/cards/"+c1.ID+"/resolve", strings.NewReader(body))
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["state"] != "approved" {
			t.Errorf("state = %q, want approved", resp["state"])
		}
	}

	// 2) Always -> 200 {"state":"approved"}
	{
		body := `{"verdict":"always"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/custos/cards/"+c2.ID+"/resolve", strings.NewReader(body))
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["state"] != "approved" {
			t.Errorf("state = %q, want approved", resp["state"])
		}
	}

	// 3) Deny -> 200 {"state":"denied"}
	{
		body := `{"verdict":"deny"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/custos/cards/"+c3.ID+"/resolve", strings.NewReader(body))
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["state"] != "denied" {
			t.Errorf("state = %q, want denied", resp["state"])
		}
	}
}

// V16: POST /v1/custos/cards/{id}/resolve error status matrix:
// 404 no_such_card, 409 already_answered|stale_verdict, 423 locked, 400 ip_ask_only.
func TestV16_Resolve_StatusMatrix(t *testing.T) {
	reg := NewFakeCustosRegistry()
	cardID := "a_card00000000099"
	reg.PublishCard(CustosCard{ID: cardID})

	_, tok, handler := setupV16Server(t, reg)

	tests := []struct {
		name       string
		id         string
		err        error
		verdict    string
		wantStatus int
		wantError  string
	}{
		{
			name:       "no such card (missing id)",
			id:         "a_card00000000088",
			verdict:    "once",
			wantStatus: http.StatusNotFound,
			wantError:  "no_such_card",
		},
		{
			name:       "already answered",
			id:         cardID,
			err:        ErrCardAnswered,
			verdict:    "once",
			wantStatus: http.StatusConflict,
			wantError:  "already_answered",
		},
		{
			name:       "stale verdict",
			id:         cardID,
			err:        ErrStaleVerdict,
			verdict:    "once",
			wantStatus: http.StatusConflict,
			wantError:  "stale_verdict",
		},
		{
			name:       "locked",
			id:         cardID,
			err:        ErrLocked,
			verdict:    "once",
			wantStatus: http.StatusLocked,
			wantError:  "locked",
		},
		{
			name:       "ip ask only",
			id:         cardID,
			err:        ErrIPAskOnly,
			verdict:    "always",
			wantStatus: http.StatusBadRequest,
			wantError:  "ip_ask_only",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err != nil {
				reg.SetResolveErr(tc.id, tc.err)
			}
			body := fmt.Sprintf(`{"verdict":%q}`, tc.verdict)
			req := httptest.NewRequest(http.MethodPost, "/v1/custos/cards/"+tc.id+"/resolve", strings.NewReader(body))
			req.Host = "127.0.0.1:7717"
			req.Header.Set("Authorization", "Bearer "+tok)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			var resp map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &resp)
			if resp["error"] != tc.wantError {
				t.Errorf("error = %q, want %q", resp["error"], tc.wantError)
			}
		})
	}

	// Unset registry resolve -> 404 no_such_card (spec ambiguity resolution).
	t.Run("unset registry resolve", func(t *testing.T) {
		_, nilTok, nilHandler := setupV16Server(t, nil)
		body := `{"verdict":"once"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/custos/cards/"+cardID+"/resolve", strings.NewReader(body))
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+nilTok)
		w := httptest.NewRecorder()
		nilHandler.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
		var resp map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["error"] != "no_such_card" {
			t.Errorf("error = %q, want no_such_card", resp["error"])
		}
	})

	// Invalid verdict / invalid body -> 400.
	t.Run("invalid verdict", func(t *testing.T) {
		body := `{"verdict":"maybe"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/custos/cards/"+cardID+"/resolve", strings.NewReader(body))
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("invalid body", func(t *testing.T) {
		body := `not-json`
		req := httptest.NewRequest(http.MethodPost, "/v1/custos/cards/"+cardID+"/resolve", strings.NewReader(body))
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})
}

// V16: ID gate rejects malformed card ids with 404 (S4 gate).
func TestV16_ID_Gate(t *testing.T) {
	reg := NewFakeCustosRegistry()
	_, tok, handler := setupV16Server(t, reg)

	malformedIDs := []string{
		"short",
		"a_shrt",                       // < 10 chars after prefix
		"a_" + strings.Repeat("x", 33), // > 32 chars after prefix
		"s_0123456789ABCDEF0123456789", // session id shape
		"../traversal",
		"a_1234567890/sub",
		"a_1234567890@bad",
	}

	for _, badID := range malformedIDs {
		t.Run(badID, func(t *testing.T) {
			body := `{"verdict":"once"}`
			req := httptest.NewRequest(http.MethodPost, "/v1/custos/cards/"+badID+"/resolve", strings.NewReader(body))
			req.Host = "127.0.0.1:7717"
			req.Header.Set("Authorization", "Bearer "+tok)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusNotFound {
				t.Fatalf("ID %q status = %d, want 404", badID, w.Code)
			}
		})
	}
}

// V16: GET /v1/custos/cards/events delivers custos_card and custos_gone shapes.
func TestV16_GlobalSSE_DataShapes(t *testing.T) {
	reg := NewFakeCustosRegistry()
	subscribed := make(chan struct{}, 1)
	reg.SetOnSubscribe(func() {
		select {
		case subscribed <- struct{}{}:
		default:
		}
	})

	_, tok, handler := setupV16Server(t, reg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/v1/custos/cards/events", nil).WithContext(ctx)
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+tok)

	pr, pw := io.Pipe()
	defer pr.Close()

	rec := &pipeRecorder{header: make(http.Header), pw: pw}
	go func() {
		handler.ServeHTTP(rec, req)
		pw.Close()
	}()

	// Wait for handler to subscribe before publishing
	select {
	case <-subscribed:
	case <-time.After(90 * time.Millisecond):
		t.Fatal("timed out waiting for subscription")
	}

	scanner := bufio.NewScanner(pr)

	// Publish card
	card := CustosCard{
		ID:         "a_card11111111111",
		Cell:       "cell-1",
		Cred:       "cred-1",
		Tool:       "tool-1",
		Dest:       "dest-1",
		Review:     "review-1",
		AgeS:       1,
		ExpiresInS: 100,
	}
	reg.PublishCard(card)

	// Read event
	ev1, data1 := readNextSSEEvent(t, scanner)
	if ev1 != "custos_card" {
		t.Fatalf("event = %q, want custos_card", ev1)
	}
	var gotCard CustosCard
	if err := json.Unmarshal([]byte(data1), &gotCard); err != nil {
		t.Fatalf("unmarshal card error: %v", err)
	}
	if gotCard.ID != card.ID || gotCard.Cred != card.Cred || gotCard.Dest != card.Dest {
		t.Errorf("gotCard mismatch: %+v", gotCard)
	}

	// Publish gone
	gone := CustosGone{
		ID:     card.ID,
		State:  "approved",
		Reason: "web",
	}
	reg.PublishGone(gone)

	ev2, data2 := readNextSSEEvent(t, scanner)
	if ev2 != "custos_gone" {
		t.Fatalf("event = %q, want custos_gone", ev2)
	}
	var gotGone CustosGone
	if err := json.Unmarshal([]byte(data2), &gotGone); err != nil {
		t.Fatalf("unmarshal gone error: %v", err)
	}
	if gotGone.ID != gone.ID || gotGone.State != gone.State || gotGone.Reason != gone.Reason {
		t.Errorf("gotGone mismatch: %+v", gotGone)
	}
}

// V16: Global SSE stream emits periodic : ping comment (no sleep >100ms).
func TestV16_GlobalSSE_PingLiveness(t *testing.T) {
	origInterval := custosHeartbeatInterval
	custosHeartbeatInterval = 20 * time.Millisecond
	defer func() { custosHeartbeatInterval = origInterval }()

	reg := NewFakeCustosRegistry()
	subscribed := make(chan struct{}, 1)
	reg.SetOnSubscribe(func() {
		select {
		case subscribed <- struct{}{}:
		default:
		}
	})

	_, tok, handler := setupV16Server(t, reg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/v1/custos/cards/events", nil).WithContext(ctx)
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+tok)

	pr, pw := io.Pipe()
	defer pr.Close()

	rec := &pipeRecorder{header: make(http.Header), pw: pw}
	go func() {
		handler.ServeHTTP(rec, req)
		pw.Close()
	}()

	select {
	case <-subscribed:
	case <-time.After(90 * time.Millisecond):
		t.Fatal("timed out waiting for subscription")
	}

	scanner := bufio.NewScanner(pr)
	pingReceived := make(chan struct{})

	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, ": ping") {
				close(pingReceived)
				return
			}
		}
	}()

	select {
	case <-pingReceived:
		// Succeeded promptly without sleeping >100ms
	case <-time.After(90 * time.Millisecond):
		t.Fatal("timed out waiting for : ping liveness comment")
	}
}

// V16: Disconnect-auto-denial exemption:
// Close custos stream while a card is pending -> card remains pending (never auto-settled).
func TestV16_DisconnectExemption(t *testing.T) {
	reg := NewFakeCustosRegistry()
	card := CustosCard{ID: "a_cardpending000001", Cred: "test-cred"}
	reg.PublishCard(card)

	subscribed := make(chan struct{}, 1)
	reg.SetOnSubscribe(func() {
		select {
		case subscribed <- struct{}{}:
		default:
		}
	})

	_, tok, handler := setupV16Server(t, reg)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/custos/cards/events", nil).WithContext(ctx)
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+tok)

	pr, pw := io.Pipe()
	rec := &pipeRecorder{header: make(http.Header), pw: pw}

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-subscribed:
	case <-time.After(90 * time.Millisecond):
		t.Fatal("timed out waiting for subscription")
	}

	// Disconnect client
	cancel()
	<-done
	pw.Close()
	pr.Close()

	// Assert card is STILL pending in registry
	snap := reg.Snapshot()
	if len(snap) != 1 || snap[0].ID != card.ID {
		t.Fatalf("card was settled on disconnect: %+v", snap)
	}

	// Reconnect and query GET /v1/custos/cards -> still listed
	getReq := httptest.NewRequest(http.MethodGet, "/v1/custos/cards", nil)
	getReq.Host = "127.0.0.1:7717"
	getReq.Header.Set("Authorization", "Bearer "+tok)
	getW := httptest.NewRecorder()
	handler.ServeHTTP(getW, getReq)

	if getW.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", getW.Code)
	}
	var resp struct {
		Cards []CustosCard `json:"cards"`
	}
	_ = json.Unmarshal(getW.Body.Bytes(), &resp)
	if len(resp.Cards) != 1 || resp.Cards[0].ID != card.ID {
		t.Fatalf("GET after reconnect failed to list card: %+v", resp.Cards)
	}
}

// V16: Global stream survives turn boundaries and never carries turn events.
func TestV16_GlobalSSE_SurvivesTurnBoundaries(t *testing.T) {
	sp := &stubProvider{
		name: "test",
		caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
		replies: func(n int, msgs []router.Message) (*router.Completion, error) {
			return &router.Completion{Text: "turn reply"}, nil
		},
	}
	hub := setupHub(t, sp)

	reg := NewFakeCustosRegistry()
	subscribed := make(chan struct{}, 1)
	reg.SetOnSubscribe(func() {
		select {
		case subscribed <- struct{}{}:
		default:
		}
	})

	store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := store.Create("v16-test")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	source := NewPenatusSource(dir, hub)

	srv := &Server{
		Store:    store,
		Sessions: source,
		Hub:      hub,
		Cfg:      ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}
	srv.SetCustosRegistry(reg)
	handler := NewServer(srv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Open global custody card stream
	custosReq := httptest.NewRequest(http.MethodGet, "/v1/custos/cards/events", nil).WithContext(ctx)
	custosReq.Host = "127.0.0.1:7717"
	custosReq.Header.Set("Authorization", "Bearer "+tok)

	pr, pw := io.Pipe()
	defer pr.Close()

	rec := &pipeRecorder{header: make(http.Header), pw: pw}
	go func() {
		handler.ServeHTTP(rec, custosReq)
		pw.Close()
	}()

	select {
	case <-subscribed:
	case <-time.After(90 * time.Millisecond):
		t.Fatal("timed out waiting for subscription")
	}

	scanner := bufio.NewScanner(pr)

	// Publish card before turn
	card1 := CustosCard{ID: "a_card00000000101", Cred: "before"}
	reg.PublishCard(card1)

	ev1, _ := readNextSSEEvent(t, scanner)
	if ev1 != "custos_card" {
		t.Fatalf("event = %q, want custos_card", ev1)
	}

	// 2. Run a turn on session "main"
	turnReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/main/messages", strings.NewReader(`{"text":"hello"}`))
	turnReq.Host = "127.0.0.1:7717"
	turnReq.Header.Set("Authorization", "Bearer "+tok)
	turnReq.Header.Set("Content-Type", "application/json")
	turnW := httptest.NewRecorder()
	handler.ServeHTTP(turnW, turnReq)

	if turnW.Code != http.StatusOK {
		t.Fatalf("turn status = %d, want 200", turnW.Code)
	}

	// 3. Publish card after turn boundary
	card2 := CustosCard{ID: "a_card00000000102", Cred: "after"}
	reg.PublishCard(card2)

	ev2, data2 := readNextSSEEvent(t, scanner)
	if ev2 != "custos_card" {
		t.Fatalf("event = %q, want custos_card (global stream died on turn boundary?)", ev2)
	}
	var c2 CustosCard
	_ = json.Unmarshal([]byte(data2), &c2)
	if c2.ID != card2.ID {
		t.Errorf("card ID = %q, want %q", c2.ID, card2.ID)
	}
}

// V16: Slow client buffer overflow (cap 64) closes stream.
func TestV16_SlowClient_OverflowEviction(t *testing.T) {
	reg := NewFakeCustosRegistry()
	subscribed := make(chan struct{}, 1)
	reg.SetOnSubscribe(func() {
		select {
		case subscribed <- struct{}{}:
		default:
		}
	})

	_, tok, handler := setupV16Server(t, reg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/v1/custos/cards/events", nil).WithContext(ctx)
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+tok)

	// Custom writer that delays Write by 15ms so the publisher can overrun the 64-event buffer
	sw := &slowWriter{header: make(http.Header), delay: 15 * time.Millisecond}
	streamDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(sw, req)
		close(streamDone)
	}()

	select {
	case <-subscribed:
	case <-time.After(90 * time.Millisecond):
		t.Fatal("timed out waiting for subscription")
	}

	// Flood registry with > 64 events while writer is delayed
	for i := 0; i < 75; i++ {
		reg.PublishCard(CustosCard{ID: fmt.Sprintf("a_card00000000%03d", i)})
	}

	select {
	case <-streamDone:
		// Succeeded: slow client disconnected on overflow
	case <-time.After(90 * time.Millisecond):
		t.Fatal("stream did not close on buffer overflow")
	}
}

// V16: No-optimistic resolution HTTP contract:
// Cards are settled only on server 200 ack; failed Resolve keeps card pending.
func TestV16_NoOptimisticResolution_HTTPContract(t *testing.T) {
	reg := NewFakeCustosRegistry()
	card := CustosCard{ID: "a_card00000000999", Cred: "fail-test"}
	reg.PublishCard(card)

	// Configure Resolve to fail with 423 locked
	reg.SetResolveErr(card.ID, ErrLocked)

	_, tok, handler := setupV16Server(t, reg)

	body := `{"verdict":"once"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/custos/cards/"+card.ID+"/resolve", strings.NewReader(body))
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusLocked {
		t.Fatalf("status = %d, want 423", w.Code)
	}

	// Card MUST remain pending
	snap := reg.Snapshot()
	if len(snap) != 1 || snap[0].ID != card.ID {
		t.Fatalf("card did not survive failed resolve: %+v", snap)
	}
}

// Helpers for testing SSE streams.

type pipeRecorder struct {
	header http.Header
	pw     *io.PipeWriter
}

func (r *pipeRecorder) Header() http.Header         { return r.header }
func (r *pipeRecorder) Write(b []byte) (int, error) { return r.pw.Write(b) }
func (r *pipeRecorder) WriteHeader(code int)        {}
func (r *pipeRecorder) Flush()                      {}

type slowWriter struct {
	header http.Header
	delay  time.Duration
}

func (s *slowWriter) Header() http.Header { return s.header }
func (s *slowWriter) Write(b []byte) (int, error) {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	return len(b), nil
}
func (s *slowWriter) WriteHeader(code int) {}
func (s *slowWriter) Flush()               {}

func readNextSSEEvent(t *testing.T, scanner *bufio.Scanner) (string, string) {
	t.Helper()
	var eventType string
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, ":") {
			continue // skip ping comments
		}
		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data.WriteString(strings.TrimPrefix(line, "data: "))
		} else if line == "" && eventType != "" {
			return eventType, data.String()
		}
	}
	t.Fatalf("reached EOF while waiting for SSE event")
	return "", ""
}
