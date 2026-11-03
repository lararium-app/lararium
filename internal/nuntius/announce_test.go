package nuntius

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type syncClock struct {
	mu sync.RWMutex
	t  time.Time
}

func (c *syncClock) now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.t
}

func (c *syncClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newAnnounceTestBridge(t *testing.T, cfg Config, tg Transport, clk *syncClock, sleep func(context.Context, time.Duration) error, logger *log.Logger) (*Bridge, *Fake) {
	t.Helper()
	home := t.TempDir()
	var fake *Fake
	if tg == nil {
		fake = &Fake{}
		tg = fake
	} else if f, ok := tg.(*Fake); ok {
		fake = f
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	if clk == nil {
		clk = &syncClock{t: time.Unix(1000, 0)}
	}
	if sleep == nil {
		sleep = func(_ context.Context, d time.Duration) error {
			clk.advance(d)
			return nil
		}
	}

	deps := Deps{
		Cfg:   cfg,
		Home:  home,
		TG:    tg,
		Log:   logger,
		now:   clk.now,
		sleep: sleep,
	}
	b, err := New(deps)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	t.Cleanup(b.Stop)
	return b, fake
}

// TestAnnounce_FanOutAllOwnerChats verifies that fan-out delivers to every
// recorded owner chat exactly once per call.
func TestAnnounce_FanOutAllOwnerChats(t *testing.T) {
	clk := &syncClock{t: time.Unix(1000, 0)}
	b, fake := newAnnounceTestBridge(t, Config{Enabled: true}, nil, clk, nil, nil)

	b.rememberOwnerChat("chat-1")
	b.rememberOwnerChat("chat-2")
	b.rememberOwnerChat("chat-3")

	ctx := context.Background()
	text := "broadcast message"
	if err := b.Announce(ctx, text); err != nil {
		t.Fatalf("Announce failed: %v", err)
	}

	fake.mu.Lock()
	sent := append([]FakeMsg(nil), fake.Sent...)
	fake.mu.Unlock()

	if len(sent) != 3 {
		t.Fatalf("expected 3 sent messages, got %d", len(sent))
	}

	expectedChats := []string{"chat-1", "chat-2", "chat-3"}
	for i, wantChat := range expectedChats {
		if sent[i].ChatID != wantChat {
			t.Errorf("send[%d] chat = %q, want %q", i, sent[i].ChatID, wantChat)
		}
		if sent[i].Text != text {
			t.Errorf("send[%d] text = %q, want %q", i, sent[i].Text, text)
		}
	}
}

// TestAnnounce_BucketAnd429Pause verifies that rate-limit token bucket waits
// and 429 Retry-After pauses are observed via the fake clock and injected sleep.
func TestAnnounce_BucketAnd429Pause(t *testing.T) {
	clk := &syncClock{t: time.Unix(1000, 0)}
	var slept []time.Duration
	var mu sync.Mutex
	sleep := func(_ context.Context, d time.Duration) error {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
		clk.advance(d)
		return nil
	}

	b, _ := newAnnounceTestBridge(t, Config{Enabled: true}, nil, clk, sleep, nil)
	b.rememberOwnerChat("chat-burst")

	ctx := context.Background()

	// 1. First send: consumes burst token (1 token burst capacity)
	if err := b.Announce(ctx, "msg1"); err != nil {
		t.Fatalf("first announce failed: %v", err)
	}
	mu.Lock()
	if len(slept) != 0 {
		t.Fatalf("first send should not sleep, slept: %v", slept)
	}
	mu.Unlock()

	// 2. Second send to same chat immediately: bucket needs refill (1 token/sec)
	if err := b.Announce(ctx, "msg2"); err != nil {
		t.Fatalf("second announce failed: %v", err)
	}
	mu.Lock()
	if len(slept) == 0 || slept[0] < 900*time.Millisecond {
		t.Fatalf("second send should observe token refill wait ~1s, got: %v", slept)
	}
	slept = nil
	mu.Unlock()

	// 3. Test 429 Retry-After pause:
	// Cause the next send to trigger a 429 response with retryAfter = 5s
	fake429Transport := &errorInjectTransport{
		err: &apiError{status: http.StatusTooManyRequests, retryAfter: 5, desc: "flood limit"},
	}
	b.tg = &gated{T: fake429Transport, fc: b.flood}

	err := b.Announce(ctx, "msg-429")
	if err == nil {
		t.Fatal("expected 429 error from announce")
	}

	// Now restore normal transport and attempt next send:
	fakeNormal := &Fake{}
	b.tg = &gated{T: fakeNormal, fc: b.flood}

	mu.Lock()
	slept = nil
	mu.Unlock()

	if err := b.Announce(ctx, "msg-after-429"); err != nil {
		t.Fatalf("announce after 429 pause failed: %v", err)
	}

	mu.Lock()
	var totalPause time.Duration
	for _, d := range slept {
		totalPause += d
	}
	mu.Unlock()

	if totalPause < 4*time.Second {
		t.Fatalf("expected 429 pause of at least 5s, got total sleep: %v (slept: %v)", totalPause, slept)
	}
}

type errorInjectTransport struct {
	err error
}

func (e *errorInjectTransport) GetUpdates(context.Context, int64) ([]Update, error) {
	return nil, nil
}

func (e *errorInjectTransport) SendMessage(context.Context, string, string, *Keyboard) (*Message, error) {
	return nil, e.err
}

func (e *errorInjectTransport) EditMessageText(context.Context, string, int64, string, *Keyboard) error {
	return e.err
}

func (e *errorInjectTransport) SendChatAction(context.Context, string, string) error {
	return e.err
}

func (e *errorInjectTransport) AnswerCallback(context.Context, string, string) error {
	return e.err
}

func (e *errorInjectTransport) DeleteMessageReplyMarkup(context.Context, string, int64) error {
	return e.err
}

// TestAnnounce_OneFailingChatDoesNotBlockOthers verifies that when one chat fails,
// fan-out continues to all remaining chats (record-and-continue).
func TestAnnounce_OneFailingChatDoesNotBlockOthers(t *testing.T) {
	clk := &syncClock{t: time.Unix(1000, 0)}
	fake := &Fake{}

	failingTransport := &selectiveFailTransport{
		base:     fake,
		failChat: "chat-dead",
	}

	b, _ := newAnnounceTestBridge(t, Config{Enabled: true}, failingTransport, clk, nil, nil)
	b.rememberOwnerChat("chat-1")
	b.rememberOwnerChat("chat-dead")
	b.rememberOwnerChat("chat-3")

	ctx := context.Background()
	err := b.Announce(ctx, "resilient fanout")
	if err == nil {
		t.Fatal("expected error from dead chat, got nil")
	}
	if !strings.Contains(err.Error(), "chat-dead") {
		t.Fatalf("expected error to name chat-dead, got: %v", err)
	}

	fake.mu.Lock()
	sent := append([]FakeMsg(nil), fake.Sent...)
	fake.mu.Unlock()

	if len(sent) != 2 {
		t.Fatalf("expected 2 successful sends, got %d", len(sent))
	}
	if sent[0].ChatID != "chat-1" {
		t.Errorf("first send chat = %q, want chat-1", sent[0].ChatID)
	}
	if sent[1].ChatID != "chat-3" {
		t.Errorf("second send chat = %q, want chat-3", sent[1].ChatID)
	}
}

type selectiveFailTransport struct {
	base     Transport
	failChat string
}

func (s *selectiveFailTransport) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	return s.base.GetUpdates(ctx, offset)
}

func (s *selectiveFailTransport) SendMessage(ctx context.Context, chatID, text string, kb *Keyboard) (*Message, error) {
	if chatID == s.failChat {
		return nil, errors.New("chat dead: recipient unreachable")
	}
	return s.base.SendMessage(ctx, chatID, text, kb)
}

func (s *selectiveFailTransport) EditMessageText(ctx context.Context, chatID string, msgID int64, text string, kb *Keyboard) error {
	return s.base.EditMessageText(ctx, chatID, msgID, text, kb)
}

func (s *selectiveFailTransport) SendChatAction(ctx context.Context, chatID, action string) error {
	return s.base.SendChatAction(ctx, chatID, action)
}

func (s *selectiveFailTransport) AnswerCallback(ctx context.Context, cbID, text string) error {
	return s.base.AnswerCallback(ctx, cbID, text)
}

func (s *selectiveFailTransport) DeleteMessageReplyMarkup(ctx context.Context, chatID string, msgID int64) error {
	return s.base.DeleteMessageReplyMarkup(ctx, chatID, msgID)
}

// TestAnnounce_EmptyOwnerSet verifies that an empty owner set is a no-op returning nil.
func TestAnnounce_EmptyOwnerSet(t *testing.T) {
	clk := &syncClock{t: time.Unix(1000, 0)}
	b, fake := newAnnounceTestBridge(t, Config{Enabled: true}, nil, clk, nil, nil)

	ctx := context.Background()
	if err := b.Announce(ctx, "hello"); err != nil {
		t.Fatalf("expected nil for empty owner set, got: %v", err)
	}

	fake.mu.Lock()
	count := len(fake.Sent)
	fake.mu.Unlock()

	if count != 0 {
		t.Fatalf("expected 0 sends, got %d", count)
	}
}

// TestAnnounce_DisabledFallbackInvokedOnce verifies that when nuntius is disabled
// or absent, Announce invokes the registered fallback seam exactly once.
func TestAnnounce_DisabledFallbackInvokedOnce(t *testing.T) {
	ctx := context.Background()

	// Subtest 1: Bridge with Enabled = false uses instance fallback
	t.Run("disabled instance fallback", func(t *testing.T) {
		clk := &syncClock{t: time.Unix(1000, 0)}
		b, fake := newAnnounceTestBridge(t, Config{Enabled: false}, nil, clk, nil, nil)
		b.rememberOwnerChat("chat-1")

		var calls atomic.Int32
		var receivedText string
		b.SetAnnounceFallback(func(_ context.Context, text string) error {
			calls.Add(1)
			receivedText = text
			return nil
		})

		if err := b.Announce(ctx, "fallback alert"); err != nil {
			t.Fatalf("Announce failed: %v", err)
		}

		if got := calls.Load(); got != 1 {
			t.Fatalf("expected fallback invoked exactly once, got %d", got)
		}
		if receivedText != "fallback alert" {
			t.Fatalf("fallback text = %q, want %q", receivedText, "fallback alert")
		}

		fake.mu.Lock()
		count := len(fake.Sent)
		fake.mu.Unlock()
		if count != 0 {
			t.Fatalf("disabled bridge should not send to TG, sent: %d", count)
		}
	})

	// Subtest 2: nil Bridge (*Bridge)(nil) uses global fallback seam
	t.Run("nil bridge global fallback", func(t *testing.T) {
		var globalCalls atomic.Int32
		var globalText string
		SetAnnounceFallback(func(_ context.Context, text string) error {
			globalCalls.Add(1)
			globalText = text
			return nil
		})
		t.Cleanup(func() { SetAnnounceFallback(nil) })

		var nilBridge *Bridge
		if err := nilBridge.Announce(ctx, "nil bridge text"); err != nil {
			t.Fatalf("nilBridge.Announce failed: %v", err)
		}

		if got := globalCalls.Load(); got != 1 {
			t.Fatalf("expected global fallback invoked exactly once, got %d", got)
		}
		if globalText != "nil bridge text" {
			t.Fatalf("global fallback text = %q, want %q", globalText, "nil bridge text")
		}
	})

	// Subtest 3: Nil fallback is safe (no-op success)
	t.Run("nil fallback callback safety", func(t *testing.T) {
		clk := &syncClock{t: time.Unix(1000, 0)}
		b, _ := newAnnounceTestBridge(t, Config{Enabled: false}, nil, clk, nil, nil)
		b.SetAnnounceFallback(nil)
		SetAnnounceFallback(nil)

		if err := b.Announce(ctx, "no-op"); err != nil {
			t.Fatalf("expected nil error with nil fallback, got: %v", err)
		}

		var nilBridge *Bridge
		if err := nilBridge.Announce(ctx, "no-op-nil"); err != nil {
			t.Fatalf("expected nil error on nil bridge with nil fallback, got: %v", err)
		}
	})

	// Subtest 4: Fallback error propagation
	t.Run("fallback error propagates", func(t *testing.T) {
		clk := &syncClock{t: time.Unix(1000, 0)}
		b, _ := newAnnounceTestBridge(t, Config{Enabled: false}, nil, clk, nil, nil)
		expectedErr := errors.New("surface event broadcast failed")
		b.SetAnnounceFallback(func(_ context.Context, _ string) error {
			return expectedErr
		})

		err := b.Announce(ctx, "fail text")
		if !errors.Is(err, expectedErr) {
			t.Fatalf("expected %v, got %v", expectedErr, err)
		}
	})
}

// TestAnnounce_SecretNeedleMatchesReplyPath verifies that live secret needles
// behave identically to the reply path: text is passed to SendMessage, and errors
// or logs never leak the secret needle (mirroring TestH10_RefusedLogCarriesNoBody
// and TestBotAPITransportErrorNeverLeaksToken).
func TestAnnounce_SecretNeedleMatchesReplyPath(t *testing.T) {
	clk := &syncClock{t: time.Unix(1000, 0)}
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)

	b, fake := newAnnounceTestBridge(t, Config{Enabled: true}, nil, clk, nil, logger)
	b.rememberOwnerChat("chat-owner")

	const needle = "sk-live-CANARY-SECRET-NEEDLE-888"
	textWithNeedle := "Notice: live credential is " + needle

	ctx := context.Background()

	// 1. Successful send: text arrives at transport untouched
	if err := b.Announce(ctx, textWithNeedle); err != nil {
		t.Fatalf("Announce failed: %v", err)
	}

	fake.mu.Lock()
	if len(fake.Sent) != 1 || fake.Sent[0].Text != textWithNeedle {
		t.Fatalf("transport did not receive exact text, got: %+v", fake.Sent)
	}
	fake.mu.Unlock()

	// Logger must not leak needle
	if strings.Contains(logBuf.String(), needle) {
		t.Fatalf("secret needle leaked into log on success: %q", logBuf.String())
	}

	// 2. Failure path: transport fails with network error
	logBuf.Reset()
	failTransport := &errorInjectTransport{err: errors.New("connection failed: telegram 502")}
	b.tg = &gated{T: failTransport, fc: b.flood}

	err := b.Announce(ctx, textWithNeedle)
	if err == nil {
		t.Fatal("expected announce error on failing transport")
	}

	// Returned error must not contain secret needle
	if strings.Contains(err.Error(), needle) {
		t.Fatalf("returned error leaked secret needle: %v", err)
	}

	// Logger must not contain secret needle
	if strings.Contains(logBuf.String(), needle) {
		t.Fatalf("secret needle leaked into log on failure: %q", logBuf.String())
	}
}

// TestAnnounce_ContextCanceled verifies that context cancellation stops sends
// and returns ctx.Err().
func TestAnnounce_ContextCanceled(t *testing.T) {
	clk := &syncClock{t: time.Unix(1000, 0)}
	b, fake := newAnnounceTestBridge(t, Config{Enabled: true}, nil, clk, nil, nil)
	b.rememberOwnerChat("chat-1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	err := b.Announce(ctx, "cancelled")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}

	fake.mu.Lock()
	count := len(fake.Sent)
	fake.mu.Unlock()
	if count != 0 {
		t.Fatalf("pre-cancelled context should not send, sent: %d", count)
	}
}

// TestAnnounce_PreservesOwnerOrder verifies that fan-out order preserves
// the order of recorded owner chats.
func TestAnnounce_PreservesOwnerOrder(t *testing.T) {
	clk := &syncClock{t: time.Unix(1000, 0)}
	b, fake := newAnnounceTestBridge(t, Config{Enabled: true}, nil, clk, nil, nil)

	b.rememberOwnerChat("chat-zebra")
	b.rememberOwnerChat("chat-alpha")
	b.rememberOwnerChat("chat-beta")

	if err := b.Announce(context.Background(), "order-check"); err != nil {
		t.Fatalf("Announce failed: %v", err)
	}

	fake.mu.Lock()
	sent := append([]FakeMsg(nil), fake.Sent...)
	fake.mu.Unlock()

	if len(sent) != 3 {
		t.Fatalf("expected 3 sends, got %d", len(sent))
	}
	expected := []string{"chat-zebra", "chat-alpha", "chat-beta"}
	for i, want := range expected {
		if sent[i].ChatID != want {
			t.Errorf("send[%d] chat = %q, want %q", i, sent[i].ChatID, want)
		}
	}
}

// TestAnnounce_Concurrent verifies that concurrent calls to Announce
// execute safely without race conditions.
func TestAnnounce_Concurrent(t *testing.T) {
	clk := &syncClock{t: time.Unix(1000, 0)}
	var mu sync.Mutex
	sleep := func(_ context.Context, d time.Duration) error {
		mu.Lock()
		clk.advance(d)
		mu.Unlock()
		return nil
	}

	b, _ := newAnnounceTestBridge(t, Config{Enabled: true}, nil, clk, sleep, nil)
	b.rememberOwnerChat("chat-concur-1")
	b.rememberOwnerChat("chat-concur-2")

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_ = b.Announce(context.Background(), "concurrent text")
		}(i)
	}
	wg.Wait()
}
