package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

// scheduleStub scripts a model that invokes schedule_wake on the first user
// turn, acknowledges the tool result, and answers subsequent wake turns.
type scheduleStub struct {
	calls     int32
	userTurns []string
	toolArgs  string
}

func (s *scheduleStub) Name() string { return "schedstub" }

func (s *scheduleStub) Capabilities(context.Context) (router.Caps, error) {
	return router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"}, nil
}

func (s *scheduleStub) Complete(_ context.Context, msgs []router.Message, opts router.Options) (*router.Completion, error) {
	callNum := atomic.AddInt32(&s.calls, 1)
	if len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		if last.Role == router.RoleTool {
			return &router.Completion{Text: "scheduled:" + last.Content, Model: "schedstub"}, nil
		}
		if last.Role == router.RoleUser {
			s.userTurns = append(s.userTurns, last.Content)
		}
	}
	if callNum == 1 && len(opts.Tools) > 0 && s.toolArgs != "" {
		return &router.Completion{
			ToolCalls: []router.ToolCall{{
				ID:       "call_sched_1",
				Name:     "schedule_wake",
				ArgsJSON: s.toolArgs,
			}},
			Model: "schedstub",
		}, nil
	}
	return &router.Completion{Text: "wake-reply", Model: "schedstub"}, nil
}

func setupScheduleSession(t *testing.T, sp router.Provider) (*Session, string) {
	t.Helper()
	home := t.TempDir()
	sessDir := filepath.Join(home, "sessions", "main")
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatal(err)
	}
	log, err := penatus.OpenLog(sessDir)
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
	}, home
}

func TestScheduleWakeFireAndRemove(t *testing.T) {
	stub := &scheduleStub{
		toolArgs: `{"delay_seconds":60,"prompt":"ping me in 1 minute"}`,
	}
	s, home := setupScheduleSession(t, stub)
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	clk := NewFakeClock(t0)

	sched, err := NewScheduler(home, s, WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	tools := []Tool{sched.Tool()}
	sched.SetSession(s, tools, nil)

	reply, err := s.RunTurnTools(context.Background(), "please ping me in 1 minute", nil, tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reply, "scheduled:") {
		t.Fatalf("unexpected reply %q", reply)
	}

	var res WakeResult
	if err := json.Unmarshal([]byte(strings.TrimPrefix(reply, "scheduled:")), &res); err != nil {
		t.Fatalf("unmarshal tool result: %v", err)
	}
	if res.ID == "" || res.DueAt != "2026-10-30T12:01:00Z" {
		t.Fatalf("unexpected WakeResult: %+v", res)
	}

	if got := len(sched.Pending()); got != 1 {
		t.Fatalf("pending wakes = %d, want 1", got)
	}

	// Advance clock to 1 second before due: must not fire yet.
	clk.Advance(59 * time.Second)
	if len(stub.userTurns) != 1 {
		t.Fatalf("wake fired early; userTurns = %v", stub.userTurns)
	}

	// Advance clock across due time: wake fires once, prompt enters turn entry point.
	clk.Advance(1 * time.Second)
	if len(stub.userTurns) != 2 {
		t.Fatalf("userTurns = %v, want 2 turns (initial + wake)", stub.userTurns)
	}
	if stub.userTurns[1] != "ping me in 1 minute" {
		t.Fatalf("wake turn prompt = %q, want %q", stub.userTurns[1], "ping me in 1 minute")
	}

	// Record removed from memory and disk.
	if got := len(sched.Pending()); got != 0 {
		t.Fatalf("pending wakes after fire = %d, want 0", got)
	}
	rawWakes, err := os.ReadFile(sched.WakesFile())
	if err != nil {
		t.Fatalf("read wakes file: %v", err)
	}
	if strings.Contains(string(rawWakes), res.ID) {
		t.Fatalf("fired wake %s still present in wakes.json: %s", res.ID, string(rawWakes))
	}

	// Verify both scheduling and firing are recorded in session events.jsonl.
	var sawCreated, sawFired bool
	for _, ev := range s.Log.Live() {
		switch ev.T {
		case "schedule_created":
			sawCreated = true
		case "schedule_fired":
			sawFired = true
		}
	}
	if !sawCreated || !sawFired {
		t.Fatalf("events trail missing audit events (schedule_created=%v, schedule_fired=%v)", sawCreated, sawFired)
	}

	// Advancing again does not re-fire.
	clk.Advance(60 * time.Second)
	if len(stub.userTurns) != 2 {
		t.Fatalf("wake fired more than once; userTurns = %v", stub.userTurns)
	}
}

func TestScheduleWakePersistence(t *testing.T) {
	stub := &scheduleStub{}
	s, home := setupScheduleSession(t, stub)
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	clk := NewFakeClock(t0)

	sched1, err := NewScheduler(home, s, WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	w, err := sched1.Schedule(context.Background(), "main", 120, "check the oven")
	if err != nil {
		t.Fatal(err)
	}
	sched1.Stop()

	// Ensure no leftover temp files exist.
	tmps, err := filepath.Glob(filepath.Join(home, "wakes-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmps) != 0 {
		t.Fatalf("leftover temp files: %v", tmps)
	}

	// Rebuild scheduler state from the file while wake is still in the future.
	sched2, err := NewScheduler(home, s, WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer sched2.Stop()

	pending := sched2.Pending()
	if len(pending) != 1 {
		t.Fatalf("rebuilt scheduler pending = %d, want 1", len(pending))
	}
	if pending[0].ID != w.ID || pending[0].Prompt != "check the oven" || pending[0].DueAt != w.DueAt {
		t.Fatalf("rebuilt wake mismatch: got %+v, want %+v", pending[0], w)
	}

	// Advance clock to fire on the rebuilt scheduler.
	clk.Advance(120 * time.Second)
	if len(stub.userTurns) != 1 || stub.userTurns[0] != "check the oven" {
		t.Fatalf("expected rebuilt scheduler to fire turn once, got userTurns=%v", stub.userTurns)
	}

	// After fire, file no longer contains the wake.
	data, err := os.ReadFile(sched2.WakesFile())
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []Wake
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("unmarshal wakes.json after fire: %v", err)
	}
	if len(onDisk) != 0 {
		t.Fatalf("wakes.json still has entries after fire: %+v", onDisk)
	}
}

func TestScheduleWakeMissedWhileOffline(t *testing.T) {
	stub := &scheduleStub{}
	s, home := setupScheduleSession(t, stub)
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	clk := NewFakeClock(t0)

	sched1, err := NewScheduler(home, s, WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	w, err := sched1.Schedule(context.Background(), "main", 60, "missed reminder")
	if err != nil {
		t.Fatal(err)
	}
	// Daemon goes offline before due time.
	sched1.Stop()

	// Clock advances past due time while daemon is offline.
	clk.Advance(5 * time.Minute)
	if len(stub.userTurns) != 0 {
		t.Fatalf("wake should not fire while scheduler is stopped; userTurns=%v", stub.userTurns)
	}

	// Daemon starts back up: missed wake fires immediately once and writes offline-startup audit line.
	sched2, err := NewScheduler(home, s, WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.userTurns) != 1 || stub.userTurns[0] != "missed reminder" {
		t.Fatalf("missed wake did not fire once at startup; userTurns=%v", stub.userTurns)
	}
	if len(sched2.Pending()) != 0 {
		t.Fatalf("missed wake still pending after startup fire: %+v", sched2.Pending())
	}

	var sawOfflineAudit bool
	for _, ev := range s.Log.Live() {
		if ev.T != "schedule_fired" {
			continue
		}
		var f struct {
			ID      string `json:"id"`
			Missed  bool   `json:"missed"`
			Offline bool   `json:"offline"`
			Reason  string `json:"reason"`
		}
		decodeInto(ev, &f)
		if f.ID == w.ID && f.Missed && f.Offline && f.Reason == "slipped_while_offline" {
			sawOfflineAudit = true
		}
	}
	if !sawOfflineAudit {
		t.Fatalf("missing offline-startup audit line in events.jsonl: %+v", s.Log.Live())
	}
	sched2.Stop()

	// Subsequent restart must not duplicate the fired wake.
	clk.Advance(1 * time.Minute)
	sched3, err := NewScheduler(home, s, WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer sched3.Stop()
	if len(stub.userTurns) != 1 {
		t.Fatalf("missed wake fired again on second restart; userTurns=%v", stub.userTurns)
	}
}

func TestScheduleWakeRejections(t *testing.T) {
	stub := &scheduleStub{}
	s, home := setupScheduleSession(t, stub)
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	clk := NewFakeClock(t0)

	sched, err := NewScheduler(home, s, WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	defer sched.Stop()

	tool := sched.Tool()
	eng := newToolEngine([]Tool{tool}, s.Log.Dir(), nil)
	ctx := withSession(context.Background(), s)

	callTool := func(args string) ToolOutcome {
		tc := router.ToolCall{ID: "c", Name: "schedule_wake", ArgsJSON: args}
		approval, tl, _ := eng.Gate(tc)
		return eng.Execute(ctx, tc, tl, approval)
	}

	// 1. Delay 0
	if out := callTool(`{"delay_seconds":0,"prompt":"zero"}`); out.OK {
		t.Fatalf("expected rejection for delay 0, got OK: %s", out.Text)
	}
	// 2. Delay over a year (> 31536000)
	if out := callTool(`{"delay_seconds":31536001,"prompt":"too far"}`); out.OK {
		t.Fatalf("expected rejection for delay > 31536000, got OK: %s", out.Text)
	}
	// 3. Empty prompt
	if out := callTool(`{"delay_seconds":60,"prompt":"   "}`); out.OK {
		t.Fatalf("expected rejection for empty prompt, got OK: %s", out.Text)
	}
	if len(sched.Pending()) != 0 {
		t.Fatalf("rejected calls must not add pending wakes, got %d", len(sched.Pending()))
	}

	// 4. 50 pending wakes succeed, 51st is rejected (error result, no panic).
	for i := 1; i <= MaxPendingWakes; i++ {
		args := fmt.Sprintf(`{"delay_seconds":%d,"prompt":"wake %d"}`, 60+i, i)
		if out := callTool(args); !out.OK {
			t.Fatalf("wake %d unexpectedly failed: %s", i, out.Text)
		}
	}
	if len(sched.Pending()) != MaxPendingWakes {
		t.Fatalf("pending count = %d, want %d", len(sched.Pending()), MaxPendingWakes)
	}
	if out := callTool(`{"delay_seconds":120,"prompt":"wake 51"}`); out.OK {
		t.Fatalf("expected 51st pending wake to be rejected, got OK: %s", out.Text)
	}
	if len(sched.Pending()) != MaxPendingWakes {
		t.Fatalf("pending count after 51st rejection = %d, want %d", len(sched.Pending()), MaxPendingWakes)
	}
}
