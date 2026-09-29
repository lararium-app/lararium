package loop

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

// stubProvider records requests, replies from a script.
type stubProvider struct {
	name    string
	caps    router.Caps
	replies func(n int, msgs []router.Message) string
	n       int32
	got     []([]router.Message)
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Capabilities(context.Context) (router.Caps, error) {
	return s.caps, nil
}
func (s *stubProvider) Complete(_ context.Context, msgs []router.Message, _ router.Options) (*router.Completion, error) {
	i := atomic.AddInt32(&s.n, 1)
	s.got = append(s.got, msgs)
	text := s.replies(int(i), msgs)
	return &router.Completion{Text: text, Model: s.name}, nil
}

func setup(t *testing.T, sp *stubProvider) *Session {
	t.Helper()
	dir := t.TempDir()
	log, err := penatus.OpenLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.NewRouter(router.Profile{Chain: []router.Target{{Provider: sp}}})
	rt.SetProfile("compact", router.Profile{Chain: []router.Target{{Provider: sp}}})
	return &Session{
		Log:            log,
		Sys:            "SYS",
		Router:         rt,
		CompactProfile: "compact",
		TriggerPct:     80,
	}
}

func TestTurnsRoundTripThroughLog(t *testing.T) {
	sp := &stubProvider{name: "stub", replies: func(n int, _ []router.Message) string {
		return "reply" + string(rune('0'+n))
	}}
	s := setup(t, sp)
	ctx := context.Background()

	if _, err := s.RunTurn(ctx, "hello", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunTurn(ctx, "again", nil); err != nil {
		t.Fatal(err)
	}

	// Second call must have seen: system, user1, assistant1, user2.
	seen := sp.got[1]
	if len(seen) != 4 {
		t.Fatalf("want 4 msgs, got %d: %+v", len(seen), seen)
	}
	if seen[0].Role != router.RoleSystem || seen[0].Content != "SYS" {
		t.Error("system prompt missing/misordered")
	}
	if seen[1].Content != "hello" || seen[2].Content != "reply1" || seen[3].Content != "again" {
		t.Errorf("bad transcript: %+v", seen)
	}
}

func TestCompactEventRewiresAssembly(t *testing.T) {
	sp := &stubProvider{name: "stub", replies: func(n int, msgs []router.Message) string {
		if msgs[0].Content == compactInstruction {
			return "SUMMARY HERE"
		}
		return "r" + string(rune('0'+n))
	}}
	s := setup(t, sp)
	ctx := context.Background()

	for i := 0; i < 8; i++ {
		if _, err := s.RunTurn(ctx, "msg", nil); err != nil {
			t.Fatal(err)
		}
	}
	before := len(s.Log.Live())
	if err := s.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	// Compact event present.
	compacts := 0
	for _, ev := range s.Log.Live() {
		if ev.T == "compact" {
			compacts++
		}
	}
	if compacts != 1 {
		t.Fatalf("want 1 compact event, log %d→%d events", before, len(s.Log.Live()))
	}

	// Assembly: sys + summary + 4 kept verbatim = 6.
	msgs := s.messages()
	if msgs[1].Content != "[earlier conversation summary]\nSUMMARY HERE" {
		t.Fatalf("summary not injected at compact position: %q", msgs[1].Content)
	}
	if len(msgs) != 6 {
		t.Fatalf("want 6 msgs, got %d: %+v", len(msgs), msgs)
	}
	if !strings.Contains(msgs[len(msgs)-1].Content, "r") {
		t.Error("kept msgs missing")
	}
}

func TestCompactTooSmallNoop(t *testing.T) {
	sp := &stubProvider{name: "stub", replies: func(int, []router.Message) string { return "x" }}
	s := setup(t, sp)
	if _, err := s.RunTurn(context.Background(), "only one", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, ev := range s.Log.Live() {
		if ev.T == "compact" {
			t.Fatal("should not compact a tiny transcript")
		}
	}
}

func TestTombstonedMsgsExcludedFromAssembly(t *testing.T) {
	sp := &stubProvider{name: "stub", replies: func(int, []router.Message) string { return "ok" }}
	s := setup(t, sp)
	ctx := context.Background()
	if _, err := s.RunTurn(ctx, "keep", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunTurn(ctx, "secret", nil); err != nil {
		t.Fatal(err)
	}
	// Tombstone seq 3 (the "secret" user msg).
	if err := appendTombstone(s, 3); err != nil {
		t.Fatal(err)
	}
	msgs := s.messages()
	for _, m := range msgs {
		if m.Content == "secret" {
			t.Fatal("tombstoned message leaked into assembly")
		}
	}
}

func appendTombstone(s *Session, seq int64) error {
	return s.Log.Append(penatus.Event{
		T:      "tombstone",
		TS:     "2026-09-28T00:00:00Z",
		Fields: map[string]json.RawMessage{"seqs": mustJSON([]int64{seq}), "reason": raw("test")},
	})
}
