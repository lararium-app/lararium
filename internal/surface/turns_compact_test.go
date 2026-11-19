package surface

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	stdlog "log"
	"strings"
	"sync"
	"testing"

	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

// compactHub builds a hub whose main profile uses mainSP and whose
// "compact" profile uses compactSP. The main session is pre-seeded with
// enough history that Compact has something to summarize.
func compactHub(t *testing.T, mainSP, compactSP *stubProvider) *Hub {
	t.Helper()
	hub := setupHub(t, mainSP)
	hub.router.SetProfile("compact", router.Profile{Chain: []router.Target{{Provider: compactSP}}})

	log, err := penatus.OpenLog(hub.SessionDir("main"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if err := log.Append(penatus.Event{T: "msg", Fields: map[string]json.RawMessage{
			"role": raw(t, role),
			"text": raw(t, strings.Repeat("seed history ", 20)),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	return hub
}

func countCompacts(t *testing.T, hub *Hub) int {
	t.Helper()
	log, err := penatus.OpenLog(hub.SessionDir("main"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range log.Live() {
		if ev.T == "compact" {
			n++
		}
	}
	return n
}

// captureStdLog redirects the std logger for the test's duration.
func captureStdLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := stdlog.Writer()
	stdlog.SetOutput(&lockedWriter{mu: &mu, b: &buf})
	t.Cleanup(func() { stdlog.SetOutput(prev) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func okProvider(ctxLen int, text string) *stubProvider {
	return &stubProvider{
		name: "stub",
		caps: router.Caps{ContextLength: ctxLen, Source: "stub"},
		replies: func(int, []router.Message) (*router.Completion, error) {
			return &router.Completion{Text: text, Model: "stub"}, nil
		},
	}
}

func TestTurn_CompactsAfterCleanTurnOverTrigger(t *testing.T) {
	hub := compactHub(t, okProvider(10, "reply"), okProvider(10, "SUMMARY"))
	if err := hub.RunHeadlessTurn(context.Background(), "main", "hello", "cron", 0, nil); err != nil {
		t.Fatalf("turn returned error: %v", err)
	}
	if n := countCompacts(t, hub); n != 1 {
		t.Fatalf("compact events = %d, want 1", n)
	}
}

func TestTurn_FailedTurnDoesNotCompact(t *testing.T) {
	mainSP := okProvider(10, "")
	mainSP.replies = func(int, []router.Message) (*router.Completion, error) {
		return nil, errors.New("connection refused")
	}
	hub := compactHub(t, mainSP, okProvider(10, "SUMMARY"))
	// Interactive channel: only scheduled channels compact pre-turn.
	if err := hub.RunHeadlessTurn(context.Background(), "main", "hello", "telegram", 0, nil); err == nil {
		t.Fatal("expected turn error")
	}
	if n := countCompacts(t, hub); n != 0 {
		t.Fatalf("compact events = %d, want 0 after failed turn", n)
	}
}

func TestTurn_CompactionErrorDoesNotFailTurn(t *testing.T) {
	compactSP := okProvider(10, "")
	compactSP.replies = func(int, []router.Message) (*router.Completion, error) {
		return nil, errors.New("compact backend down")
	}
	logs := captureStdLog(t)
	hub := compactHub(t, okProvider(10, "reply"), compactSP)
	if err := hub.RunHeadlessTurn(context.Background(), "main", "hello", "cron", 0, nil); err != nil {
		t.Fatalf("turn must succeed despite compaction error, got: %v", err)
	}
	if n := countCompacts(t, hub); n != 0 {
		t.Fatalf("compact events = %d, want 0", n)
	}
	if out := logs(); !strings.Contains(out, "compaction on main failed") || !strings.Contains(out, "compact backend down") {
		t.Fatalf("compaction error not logged; log = %q", out)
	}
}

func TestTurn_UnderTriggerNoCompact(t *testing.T) {
	hub := compactHub(t, okProvider(1000000, "reply"), okProvider(1000000, "SUMMARY"))
	if err := hub.RunHeadlessTurn(context.Background(), "main", "hello", "cron", 0, nil); err != nil {
		t.Fatalf("turn returned error: %v", err)
	}
	if n := countCompacts(t, hub); n != 0 {
		t.Fatalf("compact events = %d, want 0 under trigger", n)
	}
}

// Regression (#292a): a scheduled session whose transcript is already over
// the trigger is compacted BEFORE the turn, so a turn that would then fail
// cannot leave the prompt growing.
func TestTurn_ScheduledSessionCompactsBeforeTurn(t *testing.T) {
	mainSP := okProvider(10, "")
	mainSP.replies = func(int, []router.Message) (*router.Completion, error) {
		return nil, errors.New("connection refused")
	}
	hub := compactHub(t, mainSP, okProvider(10, "SUMMARY"))
	if err := hub.RunHeadlessTurn(context.Background(), "main", "hello", "cron", 0, nil); err == nil {
		t.Fatal("expected turn error")
	}
	if n := countCompacts(t, hub); n != 1 {
		t.Fatalf("compact events = %d, want 1 (pre-turn) for cron channel", n)
	}
}

// Regression (#292b): a provider context-overflow forces a compaction and
// the turn is retried exactly once.
func TestTurn_ContextOverflowCompactsAndRetriesOnce(t *testing.T) {
	overflow := errors.New("HTTP 400: request (65959 tokens) exceeds the available context size (32768 tokens)")
	mainSP := okProvider(1000000, "")
	mainSP.replies = func(n int, _ []router.Message) (*router.Completion, error) {
		if n == 1 {
			return nil, overflow
		}
		return &router.Completion{Text: "recovered", Model: "stub"}, nil
	}
	hub := compactHub(t, mainSP, okProvider(1000000, "SUMMARY"))
	if err := hub.RunHeadlessTurn(context.Background(), "main", "hello", "telegram", 0, nil); err != nil {
		t.Fatalf("turn should recover, got %v", err)
	}
	if n := countCompacts(t, hub); n != 1 {
		t.Fatalf("compact events = %d, want 1", n)
	}
	if mainSP.n != 2 {
		t.Fatalf("main provider calls = %d, want 2 (one retry)", mainSP.n)
	}

	// Persistent overflow: only ONE retry, then the turn fails.
	alwaysSP := okProvider(1000000, "")
	alwaysSP.replies = func(int, []router.Message) (*router.Completion, error) { return nil, overflow }
	hub2 := compactHub(t, alwaysSP, okProvider(1000000, "SUMMARY"))
	if err := hub2.RunHeadlessTurn(context.Background(), "main", "hello", "telegram", 0, nil); err == nil {
		t.Fatal("expected failure on persistent overflow")
	}
	if alwaysSP.n != 2 {
		t.Fatalf("calls = %d, want exactly 2 (initial + one retry)", alwaysSP.n)
	}
}
