package nuntius

import (
	"context"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/router"
)

type hungRoundTripper struct {
	mu        sync.Mutex
	calls     int
	hungCalls int
}

func (h *hungRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	h.mu.Lock()
	h.calls++
	callNum := h.calls
	h.mu.Unlock()

	if callNum == 1 {
		// First turn: provider hangs until context is cancelled by timeout
		h.mu.Lock()
		h.hungCalls++
		h.mu.Unlock()
		<-req.Context().Done()
		return nil, req.Context().Err()
	}

	// Subsequent turn: responsive provider returning valid response
	respJSON := `{"id":"test-1","choices":[{"message":{"role":"assistant","content":"next turn completed"},"finish_reason":"stop"}]}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(respJSON)),
		Request:    req,
	}, nil
}

type testProvEngine struct {
	provider router.Provider
}

func (e *testProvEngine) RunTurn(ctx context.Context, req TurnReq) TurnResult {
	c, err := e.provider.Complete(ctx, []router.Message{
		{Role: "user", Content: req.Text},
	}, router.Options{Model: "test-model"})
	if err != nil {
		return TurnResult{Err: err}
	}
	if req.OnDelta != nil {
		req.OnDelta(c.Text)
	}
	return TurnResult{Text: c.Text}
}

// M11: provider that never responds → turn is abandoned within the timeout and
// the queue processes the NEXT queued turn (using a fake RoundTripper).
func TestBridgeTurnTimeoutQueueAdvances(t *testing.T) {
	rt := &hungRoundTripper{}
	customClient := &http.Client{Transport: rt}
	prov := router.NewOpenAIWithClient("test-hanging", "http://fake.local/v1", "test-key", "test-model", false, customClient)
	engine := &testProvEngine{provider: prov}

	home := t.TempDir()
	ndir := filepath.Join(home, "nuntius")
	store, err := NewPairStore(ndir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	code, _ := store.Mint(now, time.Hour)
	_, _ = store.Redeem("owner1", code, now)

	sf, err := NewStateFile(ndir)
	if err != nil {
		t.Fatal(err)
	}
	_ = sf.Save(State{ActiveSession: "main", OwnerChat: "123"})

	fake := &Fake{}
	var logBuf strings.Builder
	b, err := New(Deps{
		Cfg:    Config{Enabled: true},
		Home:   home,
		TG:     fake,
		Engine: engine,
		Log:    log.New(&logBuf, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	b.turnTimeout = 50 * time.Millisecond // M11: short timeout for test

	q := b.queueFor("123")
	// Submit Turn 1 (will hang at provider)
	if !q.submit(job{rec: Record{UpdateID: 1, Kind: KindTurn}, chatID: "123", text: "first turn"}) {
		t.Fatal("turn 1 submit failed")
	}

	// Submit Turn 2 into queue (queued behind Turn 1)
	if !q.submit(job{rec: Record{UpdateID: 2, Kind: KindTurn}, chatID: "123", text: "second turn"}) {
		t.Fatal("turn 2 submit failed")
	}

	// Wait for queue to process both turns
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		running := q.running
		held := len(q.held)
		q.mu.Unlock()

		rt.mu.Lock()
		calls := rt.calls
		rt.mu.Unlock()

		if !running && held == 0 && calls >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	rt.mu.Lock()
	calls := rt.calls
	rt.mu.Unlock()
	if calls < 2 {
		t.Fatalf("expected at least 2 provider calls (turn 1 timeout, then turn 2), got %d", calls)
	}

	// Check that turn 1 was aborted with turn timeout
	// and turn 2 completed successfully.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	foundAbort := false
	foundSuccess := false
	for _, m := range fake.Sent {
		if strings.Contains(m.Text, "aborted: turn timeout") {
			foundAbort = true
		}
		if strings.Contains(m.Text, "next turn completed") {
			foundSuccess = true
		}
	}
	for _, e := range fake.Edits {
		if strings.Contains(e.Text, "aborted: turn timeout") {
			foundAbort = true
		}
		if strings.Contains(e.Text, "next turn completed") {
			foundSuccess = true
		}
	}
	if !foundAbort {
		t.Errorf("turn 1 was not aborted with turn timeout; sent=%+v, edits=%+v", fake.Sent, fake.Edits)
	}
	if !foundSuccess {
		t.Errorf("turn 2 was not processed successfully; sent=%+v, edits=%+v", fake.Sent, fake.Edits)
	}
}
