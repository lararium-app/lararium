package surface

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

// stubProvider records requests and replies from a script.
type stubProvider struct {
	name    string
	caps    router.Caps
	replies func(n int, msgs []router.Message) (*router.Completion, error)
	n       int32
	mu      sync.Mutex
	got     []([]router.Message)
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Capabilities(ctx context.Context) (router.Caps, error) {
	return s.caps, nil
}

func (s *stubProvider) Complete(ctx context.Context, msgs []router.Message, opts router.Options) (*router.Completion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	s.got = append(s.got, msgs)
	return s.replies(int(s.n), msgs)
}

func (s *stubProvider) CompleteStream(ctx context.Context, msgs []router.Message, opts router.Options, onDelta func(string) error) (*router.Completion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	s.got = append(s.got, msgs)
	comp, err := s.replies(int(s.n), msgs)
	if err != nil {
		return nil, err
	}
	if comp.Text != "" {
		_ = onDelta(comp.Text)
	}
	return comp, nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func raw(s string) json.RawMessage {
	return mustJSON(s)
}

func setupHub(t *testing.T, sp *stubProvider) (*Hub, string) {
	t.Helper()
	dir := t.TempDir()

	// Create main session
	_, err := penatus.CreateSession(dir, "main", "main")
	if err != nil {
		t.Fatal(err)
	}

	// Open log for main
	mainDir := dir + "/sessions/main"
	log, err := penatus.OpenLog(mainDir)
	if err != nil {
		t.Fatal(err)
	}

	// Write a system prompt
	_ = log.Append(penatus.Event{T: "msg", TS: time.Now().UTC().Format(time.RFC3339), Fields: map[string]json.RawMessage{
		"role": raw("assistant"), "text": raw("system prompt"), "model": raw("test"),
		"usage": mustJSON(map[string]int{"in": 10, "out": 5}),
	}})

	rt := router.NewRouter(router.Profile{Chain: []router.Target{{Provider: sp}}})
	rt.SetProfile("compact", router.Profile{Chain: []router.Target{{Provider: sp}}})

	tools := []loop.Tool{
		{
			Spec: router.ToolSpec{
				Name:             "echo",
				Description:      "Echo tool",
				ParamsJSONSchema: `{"type":"object","properties":{"v":{"type":"string"}}}`,
			},
			Trusted: false, // untrusted to trigger approval
			Run: func(ctx context.Context, args string) (string, error) {
				return "ECHO:" + args, nil
			},
		},
	}

	cfg := ServeConfig{
		ApprovalTimeout: 5 * time.Second,
		TurnTimeout:     10 * time.Second,
	}

	ap := NewApprovalHub(cfg.ApprovalTimeout)
	hub := NewHub(cfg, dir, 1000, 80, tools, rt, ap)

	return hub, dir
}

func parseSSEFrames(body string) []map[string]any {
	var frames []map[string]any
	lines := strings.Split(body, "\n")
	var eventType string
	var data strings.Builder
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data.WriteString(strings.TrimPrefix(line, "data: "))
			data.WriteString("\n")
		} else if line == "" && eventType != "" {
			var m map[string]any
			_ = json.Unmarshal([]byte(data.String()), &m)
			frames = append(frames, map[string]any{
				"event": eventType,
				"data":  m,
			})
			eventType = ""
			data.Reset()
		} else if strings.HasPrefix(line, ": ping") {
			frames = append(frames, map[string]any{
				"event": "ping",
				"data":  nil,
			})
		}
	}
	return frames
}

func TestHub_GateConcurrentRequests(t *testing.T) {
	// This test uses a slow provider to ensure concurrent requests hit the gate
	started := make(chan struct{}, 1)
	released := make(chan struct{}, 1)

	sp := &stubProvider{
		name: "slow",
		caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
		replies: func(n int, msgs []router.Message) (*router.Completion, error) {
			started <- struct{}{}
			<-released
			return &router.Completion{Text: "done", Model: "slow"}, nil
		},
	}

	hub, _ := setupHub(t, sp)

	wg := sync.WaitGroup{}
	wg.Add(2)

	var codes [2]int
	var bodies [2]string

	for i := 0; i < 2; i++ {
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest("POST", "/v1/sessions/main/messages", strings.NewReader(`{"text":"hello"}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			hub.HandleMessage(w, req, "main")
			codes[idx] = w.Code
			bodies[idx] = w.Body.String()
		}(i)
	}

	// Wait for the first request to start (acquire gate and call provider)
	<-started

	// Release the first request
	released <- struct{}{}

	wg.Wait()

	// Exactly one 200, one 409
	successCount := 0
	conflictCount := 0
	for _, c := range codes {
		if c == 200 {
			successCount++
		} else if c == 409 {
			conflictCount++
		}
	}

	if successCount != 1 {
		t.Fatalf("expected 1 success, got %d (codes: %v)", successCount, codes)
	}
	if conflictCount != 1 {
		t.Fatalf("expected 1 conflict, got %d (codes: %v)", conflictCount, codes)
	}

	// Verify the 409 response
	for i, c := range codes {
		if c == 409 {
			var errResp map[string]string
			_ = json.Unmarshal([]byte(bodies[i]), &errResp)
			if errResp["error"] != "turn in flight" {
				t.Errorf("409 error = %q, want 'turn in flight'", errResp["error"])
			}
		}
	}
}

func TestHub_SSEOrderWithApproval(t *testing.T) {
	sp := &stubProvider{
		name: "stub",
		caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
		replies: func(n int, msgs []router.Message) (*router.Completion, error) {
			if n == 1 {
				// First call: return text chunks + tool call
				// The onDelta will be called for each chunk
				return &router.Completion{
					Text:      "hello there",
					ToolCalls: []router.ToolCall{{ID: "call_1", Name: "echo", ArgsJSON: `{"v":"hi there"}`}},
					Model:     "stub",
				}, nil
			}
			// Second call: return final text
			return &router.Completion{Text: "final", Model: "stub"}, nil
		},
	}

	hub, _ := setupHub(t, sp)

	req := httptest.NewRequest("POST", "/v1/sessions/main/messages", strings.NewReader(`{"text":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	hub.HandleMessage(w, req, "main")

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	frames := parseSSEFrames(w.Body.String())

	// Check event order - actual order from frozen loop package:
	// delta (from first completion text), approval_request, tool_start, tool_done, delta (from second completion), turn_done
	expectedEvents := []string{"delta", "approval_request", "tool_start", "tool_done", "delta", "turn_done"}
	var seenEvents []string
	for _, f := range frames {
		if f["event"] != "ping" {
			seenEvents = append(seenEvents, f["event"].(string))
		}
	}

	if len(seenEvents) != len(expectedEvents) {
		t.Fatalf("events = %v, want %v", seenEvents, expectedEvents)
	}
	for i, e := range expectedEvents {
		if seenEvents[i] != e {
			t.Fatalf("event[%d] = %s, want %s", i, seenEvents[i], e)
		}
	}

	// Verify exactly one terminal event
	terminalCount := 0
	for _, f := range frames {
		if f["event"] == "turn_done" || f["event"] == "error" {
			terminalCount++
		}
	}
	if terminalCount != 1 {
		t.Fatalf("terminal events = %d, want 1", terminalCount)
	}
}

func TestHub_DisconnectDeniesPending(t *testing.T) {
	cancelCall := make(chan struct{}, 1)

	sp := &stubProvider{
		name: "stub",
		caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
		replies: func(n int, msgs []router.Message) (*router.Completion, error) {
			// Return tool call immediately
			select {
			case <-cancelCall:
				return nil, context.Canceled
			default:
			}
			return &router.Completion{
				ToolCalls: []router.ToolCall{{ID: "call_1", Name: "echo", ArgsJSON: `{"v":"hi"}`}},
				Model:     "stub",
			}, nil
		},
	}

	hub, _ := setupHub(t, sp)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/sessions/main/messages", strings.NewReader(`{"text":"hello"}`))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		hub.HandleMessage(w, req, "main")
		close(done)
	}()

	// Wait a bit for the turn to start and create an approval
	time.Sleep(50 * time.Millisecond)

	// Cancel context (simulate client disconnect)
	cancel()

	// Signal stub to respect cancellation
	close(cancelCall)

	// Wait for handler to finish
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not finish after cancel")
	}

	// Pending count should be 0
	if hub.ap.PendingCount("main") != 0 {
		t.Fatalf("pending count after disconnect = %d, want 0", hub.ap.PendingCount("main"))
	}

	// Verify gate is released by checking InFlight is false
	if hub.InFlight("main") {
		t.Fatal("InFlight should be false after disconnect")
	}
}

func TestHub_Heartbeat(t *testing.T) {
	sp := &stubProvider{
		name: "stub",
		caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
		replies: func(n int, msgs []router.Message) (*router.Completion, error) {
			// Block indefinitely - this simulates an idle model
			select {}
		},
	}

	hub, _ := setupHub(t, sp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest("POST", "/v1/sessions/main/messages", strings.NewReader(`{"text":"hello"}`))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		hub.HandleMessage(w, req, "main")
		close(done)
	}()

	// Wait for heartbeat
	select {
	case <-done:
		t.Fatal("handler finished unexpectedly")
	case <-time.After(6 * time.Second):
		// Check if we got a ping
		body := w.Body.String()
		if !strings.Contains(body, ": ping") {
			t.Fatal("no heartbeat ping received within 6 seconds")
		}
		cancel()
	}
}

func TestHub_Cancel(t *testing.T) {
	cancelCall := make(chan struct{}, 1)

	sp := &stubProvider{
		name: "stub",
		caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
		replies: func(n int, msgs []router.Message) (*router.Completion, error) {
			// First call: block until cancelled
			select {
			case <-cancelCall:
				return nil, context.Canceled
			case <-time.After(100 * time.Millisecond):
				return &router.Completion{
					ToolCalls: []router.ToolCall{{ID: "call_1", Name: "echo", ArgsJSON: `{"v":"hi"}`}},
					Model:     "stub",
				}, nil
			}
		},
	}

	hub, _ := setupHub(t, sp)

	// First request - will be cancelled
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/sessions/main/messages", strings.NewReader(`{"text":"hello"}`))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	// Start handler in goroutine
	done := make(chan struct{})
	go func() {
		hub.HandleMessage(w, req, "main")
		close(done)
	}()

	// Wait for turn to start (InFlight true)
	time.Sleep(50 * time.Millisecond)
	if !hub.InFlight("main") {
		t.Fatal("InFlight should be true during turn")
	}

	// Cancel the turn
	ok := hub.Cancel("main")
	if !ok {
		t.Fatal("Cancel should return true when turn in flight")
	}

	// Signal the stub to return cancellation
	close(cancelCall)

	// Cancel the request context to unblock the handler
	cancel()

	// Wait for handler to finish
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not finish after cancel")
	}

	// InFlight should be false now (gate released)
	if hub.InFlight("main") {
		t.Fatal("InFlight should be false after cancel")
	}
}

func TestHub_CancelNoInFlight(t *testing.T) {
	sp := &stubProvider{
		name: "stub",
		caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
		replies: func(n int, msgs []router.Message) (*router.Completion, error) {
			return &router.Completion{Text: "done", Model: "stub"}, nil
		},
	}

	hub, _ := setupHub(t, sp)

	// Cancel with no in-flight turn
	cancelled := hub.Cancel("main")
	if cancelled {
		t.Fatal("Cancel should return false when no turn in flight")
	}
}

func TestHub_HandleApproval(t *testing.T) {
	sp := &stubProvider{
		name: "stub",
		caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
		replies: func(n int, msgs []router.Message) (*router.Completion, error) {
			return &router.Completion{Text: "done", Model: "stub"}, nil
		},
	}

	hub, _ := setupHub(t, sp)

	// Register an approval
	id, ch := hub.ap.Register("main", "echo", `{"v":"hi"}`, func(id string) {})

	// Allow it
	req := httptest.NewRequest("POST", "/v1/sessions/main/approvals/"+id, strings.NewReader(`{"decision":"allow"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	hub.HandleApproval(w, req, "main", id)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp map[string]bool
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if !resp["ok"] {
		t.Fatal("response ok should be true")
	}

	// Decision should be received
	select {
	case decision := <-ch:
		if !decision {
			t.Fatal("decision should be true")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("decision not received")
	}

	// Double resolve should return 410
	req2 := httptest.NewRequest("POST", "/v1/sessions/main/approvals/"+id, strings.NewReader(`{"decision":"allow"}`))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	hub.HandleApproval(w2, req2, "main", id)

	if w2.Code != 410 {
		t.Fatalf("second resolve status = %d, want 410", w2.Code)
	}
}

func TestHub_HandleApprovalForeignSession(t *testing.T) {
	sp := &stubProvider{
		name: "stub",
		caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
		replies: func(n int, msgs []router.Message) (*router.Completion, error) {
			return &router.Completion{Text: "done", Model: "stub"}, nil
		},
	}

	hub, _ := setupHub(t, sp)

	// Register an approval in session A
	id, _ := hub.ap.Register("s_AAA", "echo", `{"v":"hi"}`, func(id string) {})

	// Try to resolve from session B
	req := httptest.NewRequest("POST", "/v1/sessions/s_BBB/approvals/"+id, strings.NewReader(`{"decision":"allow"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	hub.HandleApproval(w, req, "s_BBB", id)

	if w.Code != 404 {
		t.Fatalf("foreign session status = %d, want 404", w.Code)
	}
}
