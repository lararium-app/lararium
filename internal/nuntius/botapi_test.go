package nuntius

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Bucket / FloodControl (N9) ---

func TestBucketBurstThenRefill(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	var slept []time.Duration
	var mu sync.Mutex
	sleep := func(_ context.Context, d time.Duration) error {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
		clk.advance(d)
		return nil
	}
	b := NewBucket(1, 1, clk.now, sleep)
	ctx := context.Background()

	if err := b.Wait(ctx); err != nil { // burst token
		t.Fatal(err)
	}
	if err := b.Wait(ctx); err != nil { // must park ~1s for a refill
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) == 0 || slept[0] < 900*time.Millisecond {
		t.Fatalf("expected ~1s park, slept %v", slept)
	}
}

func TestBucketPauseBlocks(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	var slept []time.Duration
	sleep := func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		clk.advance(d)
		return nil
	}
	b := NewBucket(25, 25, clk.now, sleep)
	b.Pause(7 * time.Second)
	if err := b.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Even with 25 tokens banked, the pause had to expire first.
	if len(slept) == 0 || slept[0] < 6*time.Second {
		t.Fatalf("pause not honored, slept %v", slept)
	}
}

func TestFloodControlPerChatIndependence(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	parked := map[string]int{}
	var mu sync.Mutex
	chatSleep := func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		parked["chat"]++
		mu.Unlock()
		clk.advance(d)
		return ctx.Err()
	}
	fc := NewFloodControl(clk.now, nil) // global bucket: no real waits
	fc.chatFactory = func() *Bucket { return NewBucket(1, 1, clk.now, chatSleep) }

	ctx := context.Background()
	// Chat A: burst 1 goes free, the second parks on the per-chat
	// bucket (synchronous fake sleep advances the clock ~1s).
	if err := fc.Send(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	if err := fc.Send(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	aParks := parked["chat"]
	mu.Unlock()
	if aParks == 0 {
		t.Fatal("chat A's second send should have parked on the per-chat bucket")
	}
	// Chat B starts fresh: its own bucket grants immediately.
	if err := fc.Send(ctx, "B"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if parked["chat"] != aParks {
		t.Fatalf("chat B parked on a per-chat bucket: %d parks", parked["chat"])
	}
}

func TestFloodControlPauseGlobal(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	var parked time.Duration
	var mu sync.Mutex
	sleep := func(_ context.Context, d time.Duration) error {
		mu.Lock()
		parked += d
		mu.Unlock()
		clk.advance(d)
		return nil
	}
	fc := NewFloodControl(clk.now, sleep)
	fc.PauseGlobal(5 * time.Second)
	// Drain the burst so the next send must touch the global bucket.
	for range 26 {
		if err := fc.Send(context.Background(), "c"); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if parked < 4*time.Second {
		t.Fatalf("global pause not honored: parked %v", parked)
	}
}

// --- BotAPI (real transport against an httptest server) ---

const testToken = "123456:TESTTOKENabcdef"

func newTestAPI(t *testing.T, handler http.HandlerFunc) *BotAPI {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	fc := NewFloodControl(nil, nil)
	api := NewBotAPI(testToken, fc)
	api.base = srv.URL
	return api
}

func okJSON(result string) string {
	return `{"ok":true,"result":` + result + `}`
}

func TestBotAPIGetUpdatesWire(t *testing.T) {
	var gotPath, gotBody string
	api := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		_, _ = w.Write([]byte(okJSON(`[{"update_id":7,"message":{"message_id":1,"from":{"id":"9"},"chat":{"id":"9","type":"private"},"text":"hi"}}]`)))
	})
	updates, err := api.GetUpdates(context.Background(), 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].UpdateID != 7 {
		t.Fatalf("updates = %+v", updates)
	}
	if !strings.HasSuffix(gotPath, "/bot"+testToken+"/getUpdates") {
		t.Fatalf("path = %q", gotPath)
	}
	for _, want := range []string{"timeout=30", "offset=6", `allowed_updates=%5B%22message%22%2C%22callback_query%22%5D`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("body %q missing %q", gotBody, want)
		}
	}
}

func TestBotAPISendAndEdit(t *testing.T) {
	var paths []string
	var bodies []string
	api := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		bodies = append(bodies, string(buf))
		_, _ = w.Write([]byte(okJSON(`{"message_id":55,"chat":{"id":"42","type":"private"}}`)))
	})
	msg, err := api.SendMessage(context.Background(), "42", "hello", ApprovalKeyboard("ap1"))
	if err != nil {
		t.Fatal(err)
	}
	if msg.MessageID != 55 {
		t.Fatalf("message id = %d", msg.MessageID)
	}
	if !strings.Contains(bodies[0], "reply_markup=") || !strings.Contains(bodies[0], "ap%3Aap1%3Aa") {
		t.Fatalf("keyboard missing from sendMessage body: %q", bodies[0])
	}
	if err := api.EditMessageText(context.Background(), "42", 55, "edited", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(paths[1], "/editMessageText") {
		t.Fatalf("path = %q", paths[1])
	}
	if !strings.Contains(bodies[1], "message_id=55") || !strings.Contains(bodies[1], `%7B%22inline_keyboard%22%3A%5B%5D%7D`) {
		t.Fatalf("edit body = %q", bodies[1])
	}
}

func TestBotAPI429PausesBuckets(t *testing.T) {
	calls := 0
	api := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":9}}`))
	})
	_, err := api.SendMessage(context.Background(), "42", "x", nil)
	if !Is429(err) {
		t.Fatalf("err = %v, want 429", err)
	}
	if RetryAfter(err) != 9*time.Second {
		t.Fatalf("retry_after = %v", RetryAfter(err))
	}
	// Both send buckets must be paused ~9s: the next send parks.
	clk := &fakeClock{t: time.Unix(0, 0)}
	var parked time.Duration
	var mu sync.Mutex
	api.fc = NewFloodControl(clk.now, func(_ context.Context, d time.Duration) error {
		mu.Lock()
		parked += d
		mu.Unlock()
		clk.advance(d)
		return nil
	})
	api.fc.PauseGlobal(9 * time.Second)
	api.fc.PauseChat("42", 9*time.Second)
	if err := api.fc.Send(context.Background(), "42"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if parked < 8*time.Second {
		t.Fatalf("429 pause not applied: parked %v", parked)
	}
}

func TestBotAPI404NeverLeaksToken(t *testing.T) {
	api := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html>404 Not Found</html>"))
	})
	_, err := api.SendMessage(context.Background(), "42", "x", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error text leaks the token: %v", err)
	}
}

func TestBotAPIRefusesRedirects(t *testing.T) {
	// A redirect would replay the token-bearing URL to the target
	// host; the client must not follow it (N4).
	var followed bool
	api := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if !followed {
			followed = true
			http.Redirect(w, r, "http://127.0.0.1:9/stolen", http.StatusFound)
			return
		}
		t.Error("redirect was followed")
	})
	_, err := api.SendMessage(context.Background(), "42", "x", nil)
	if err == nil {
		t.Fatal("expected error on 302")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaks token: %v", err)
	}
}

func TestBotAPITransportErrorNeverLeaksToken(t *testing.T) {
	// Point at a dead port: client.Do fails with a *url.Error whose
	// text embeds the full URL — token included (N4).
	fc := NewFloodControl(nil, nil)
	api := NewBotAPI(testToken, fc)
	api.base = "http://127.0.0.1:9" // port 9: connection refused
	_, err := api.SendMessage(context.Background(), "42", "x", nil)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("transport error leaks the token: %v", err)
	}
	if strings.Contains(err.Error(), "/bot"+testToken) {
		t.Fatalf("transport error leaks the token path: %v", err)
	}
}

func TestBotAPIErrorDescriptionSanitized(t *testing.T) {
	long := strings.Repeat("d", 500)
	api := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		body, _ := json.Marshal(map[string]any{"ok": false, "description": long})
		_, _ = w.Write(body)
	})
	_, err := api.SendMessage(context.Background(), "42", "x", nil)
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), strings.Repeat("d", 300)) {
		t.Fatal("description not truncated")
	}
}

func TestBotAPITokenNeverInErrorOnDecodeFailure(t *testing.T) {
	api := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json at all"))
	})
	_, err := api.GetUpdates(context.Background(), 0)
	if err == nil {
		t.Fatal("expected malformed-response error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaks token: %v", err)
	}
}

func TestBotAPIAnswerCallbackNotGated(t *testing.T) {
	// Callbacks must always be answerable, even with the send buckets
	// paused (N9: the pause is for outbound sends).
	clk := &fakeClock{t: time.Unix(0, 0)}
	api := newTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(okJSON("true")))
	})
	api.fc = NewFloodControl(clk.now, func(_ context.Context, d time.Duration) error {
		clk.advance(d)
		return nil
	})
	api.fc.PauseGlobal(60 * time.Second)
	api.fc.PauseChat("42", 60*time.Second)
	done := make(chan error, 1)
	go func() { done <- api.AnswerCallback(context.Background(), "cb1", "Done") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AnswerCallback blocked behind a send pause")
	}
}
