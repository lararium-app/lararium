package fasti

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

type heartbeatRecordedTurn struct {
	sessionID string
	prompt    string
}

type testHeartbeatRunner struct {
	mu      sync.Mutex
	turns   []heartbeatRecordedTurn
	replyFn func(sessionID, prompt string) string
}

func (r *testHeartbeatRunner) Run(ctx context.Context, sessionID, prompt string) (string, error) {
	r.mu.Lock()
	r.turns = append(r.turns, heartbeatRecordedTurn{sessionID: sessionID, prompt: prompt})
	fn := r.replyFn
	r.mu.Unlock()

	if fn != nil {
		return fn(sessionID, prompt), nil
	}
	return "HEARTBEAT_OK", nil
}

func (r *testHeartbeatRunner) TurnCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.turns)
}

func (r *testHeartbeatRunner) LastTurn() heartbeatRecordedTurn {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.turns) == 0 {
		return heartbeatRecordedTurn{}
	}
	return r.turns[len(r.turns)-1]
}

// TestHeartbeat_CadenceHourlyFiresOnTheHour: hourly cadence fires on the hour local (spec D6, R8).
func TestHeartbeat_CadenceHourlyFiresOnTheHour(t *testing.T) {
	home := t.TempDir()
	hbPath := HeartbeatFilePath(home)
	hbContent := `cadence: hourly

- [ ] Check inbox
- [ ] Review system metrics
`
	if err := os.WriteFile(hbPath, []byte(hbContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t0 := time.Date(2026, 11, 3, 10, 15, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"

	ann := &recordingAnnouncer{}
	runner := &testHeartbeatRunner{
		replyFn: func(sessionID, prompt string) string {
			return "HEARTBEAT_OK"
		},
	}

	eng, err := New(cfg, home, WithClock(clk), WithAnnouncer(ann), WithTextRunner(runner.Run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Advance 45m to 11:00:00 UTC (the hour boundary)
	clk.Advance(45 * time.Minute)
	_ = eng.Sync(context.Background())

	if runner.TurnCount() != 1 {
		t.Fatalf("expected 1 turn on the hour, got %d", runner.TurnCount())
	}

	last := runner.LastTurn()
	if last.sessionID != "heartbeat" {
		t.Errorf("sessionID = %q, want %q", last.sessionID, "heartbeat")
	}
	if !strings.Contains(last.prompt, "- [ ] Check inbox") || !strings.Contains(last.prompt, "reply HEARTBEAT_OK") {
		t.Errorf("prompt unexpected: %s", last.prompt)
	}

	// Verify journal recorded kind:"heartbeat", status:"fired"
	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("Journal.Events: %v", err)
	}
	var foundHeartbeatFired bool
	for _, ev := range events {
		if ev.Type == "fasti_fired" && ev.Kind == "heartbeat" && ev.Status == "fired" {
			foundHeartbeatFired = true
			break
		}
	}
	if !foundHeartbeatFired {
		t.Errorf("expected fasti_fired kind:heartbeat in journal, got: %+v", events)
	}
}

// TestHeartbeat_DailyAtDailyAt: daily cadence fires at configured daily_at local (spec D6, R8).
func TestHeartbeat_DailyAtDailyAt(t *testing.T) {
	home := t.TempDir()
	hbPath := HeartbeatFilePath(home)
	hbContent := `cadence: daily

- [ ] Morning sync check
`
	if err := os.WriteFile(hbPath, []byte(hbContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t0 := time.Date(2026, 11, 3, 7, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"
	cfg.Heartbeat.DailyAt = "08:15"

	ann := &recordingAnnouncer{}
	runner := &testHeartbeatRunner{}

	eng, err := New(cfg, home, WithClock(clk), WithAnnouncer(ann), WithTextRunner(runner.Run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Advance to 08:14 UTC (1m before daily_at) -> should not fire yet
	clk.Advance(74 * time.Minute)
	_ = eng.Sync(context.Background())

	if runner.TurnCount() != 0 {
		t.Fatalf("should not fire before daily_at, got %d turns", runner.TurnCount())
	}

	// Advance 1m to 08:15 UTC -> fires!
	clk.Advance(1 * time.Minute)
	_ = eng.Sync(context.Background())

	if runner.TurnCount() != 1 {
		t.Fatalf("expected 1 turn at daily_at (08:15), got %d", runner.TurnCount())
	}
}

// TestHeartbeat_EmptyBodyOff: empty checklist body disables heartbeat executor (spec D6).
func TestHeartbeat_EmptyBodyOff(t *testing.T) {
	home := t.TempDir()
	hbPath := HeartbeatFilePath(home)
	// Frontmatter only, no checklist items
	hbContent := `cadence: hourly
`
	if err := os.WriteFile(hbPath, []byte(hbContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"

	runner := &testHeartbeatRunner{}

	eng, err := New(cfg, home, WithClock(clk), WithTextRunner(runner.Run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Advance 3 hours
	clk.Advance(3 * time.Hour)
	_ = eng.Sync(context.Background())

	if runner.TurnCount() != 0 {
		t.Fatalf("empty body must remain off, got %d turns", runner.TurnCount())
	}
}

// TestHeartbeat_MissingFileOff: missing HEARTBEAT.md remains off (spec D6).
func TestHeartbeat_MissingFileOff(t *testing.T) {
	home := t.TempDir()
	// No file written

	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"

	runner := &testHeartbeatRunner{}

	eng, err := New(cfg, home, WithClock(clk), WithTextRunner(runner.Run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Advance multiple hours past daily/hourly boundaries
	clk.Advance(24 * time.Hour)
	_ = eng.Sync(context.Background())

	if runner.TurnCount() != 0 {
		t.Fatalf("missing file must remain off, got %d turns", runner.TurnCount())
	}
}

// TestHeartbeat_CadenceChangePickedUpNextWakeWithoutRestart: rewriting file cadence takes effect at next wake without restart (spec D6, R8).
func TestHeartbeat_CadenceChangePickedUpNextWakeWithoutRestart(t *testing.T) {
	home := t.TempDir()
	hbPath := HeartbeatFilePath(home)

	// Start with cadence hourly
	initialContent := `cadence: hourly

- [ ] Dynamic cadence check
`
	if err := os.WriteFile(hbPath, []byte(initialContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"
	cfg.Heartbeat.DailyAt = "15:00"

	runner := &testHeartbeatRunner{}

	eng, err := New(cfg, home, WithClock(clk), WithTextRunner(runner.Run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wake 1 fires at 11:00 UTC (hourly boundary)
	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background())

	if runner.TurnCount() != 1 {
		t.Fatalf("wake 1: expected 1 turn, got %d", runner.TurnCount())
	}

	// Rewrite file to daily at 15:00 WITHOUT restarting engine
	updatedContent := `cadence: daily

- [ ] Dynamic cadence check updated
`
	if err := os.WriteFile(hbPath, []byte(updatedContent), 0o644); err != nil {
		t.Fatalf("Rewrite file: %v", err)
	}

	// Since wake 1 finished, the engine re-read HEARTBEAT.md and armed next wake for 15:00 UTC!
	// Advance to 12:00 UTC (1 hour later): should NOT fire because cadence is now daily at 15:00
	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background())

	if runner.TurnCount() != 1 {
		t.Fatalf("at 12:00: should NOT fire under daily cadence, got %d turns", runner.TurnCount())
	}

	// Advance to 15:00 UTC (3 hours later): daily wake fires!
	clk.Advance(3 * time.Hour)
	_ = eng.Sync(context.Background())

	if runner.TurnCount() != 2 {
		t.Fatalf("at 15:00: expected daily wake to fire, got %d turns", runner.TurnCount())
	}
}

// TestHeartbeat_ChecklistChangePickedUpNextWake: modified checklist prompt used at next wake without restart (spec D6, R8).
func TestHeartbeat_ChecklistChangePickedUpNextWake(t *testing.T) {
	home := t.TempDir()
	hbPath := HeartbeatFilePath(home)

	initialContent := `cadence: hourly

- [ ] Original item A
`
	if err := os.WriteFile(hbPath, []byte(initialContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"

	runner := &testHeartbeatRunner{}

	eng, err := New(cfg, home, WithClock(clk), WithTextRunner(runner.Run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wake 1 at 11:00 UTC
	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background())

	if runner.TurnCount() != 1 {
		t.Fatalf("wake 1: expected 1 turn, got %d", runner.TurnCount())
	}
	if !strings.Contains(runner.LastTurn().prompt, "Original item A") {
		t.Errorf("wake 1 prompt missing 'Original item A': %s", runner.LastTurn().prompt)
	}

	// Modify checklist without engine restart
	modifiedContent := `cadence: hourly

- [ ] Modified item B
`
	if err := os.WriteFile(hbPath, []byte(modifiedContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Wake 2 at 12:00 UTC
	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background())

	if runner.TurnCount() != 2 {
		t.Fatalf("wake 2: expected 2 turns, got %d", runner.TurnCount())
	}
	if !strings.Contains(runner.LastTurn().prompt, "Modified item B") {
		t.Errorf("wake 2 prompt missing 'Modified item B': %s", runner.LastTurn().prompt)
	}
	if strings.Contains(runner.LastTurn().prompt, "Original item A") {
		t.Errorf("wake 2 prompt should not contain 'Original item A': %s", runner.LastTurn().prompt)
	}
}

// TestHeartbeat_HeartbeatOKNotAnnouncedAndNotBudgetCounted: HEARTBEAT_OK produces 0 announces and 0 budget charge (spec D6).
func TestHeartbeat_HeartbeatOKNotAnnouncedAndNotBudgetCounted(t *testing.T) {
	home := t.TempDir()
	hbPath := HeartbeatFilePath(home)
	hbContent := `cadence: hourly

- [ ] Routine checklist item
`
	if err := os.WriteFile(hbPath, []byte(hbContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"

	ann := &recordingAnnouncer{}
	runner := &testHeartbeatRunner{
		replyFn: func(sessionID, prompt string) string {
			return "  HEARTBEAT_OK  \n" // whitespace-trimmed check
		},
	}

	eng, err := New(cfg, home, WithClock(clk), WithAnnouncer(ann), WithTextRunner(runner.Run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Fire wake
	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background())

	if runner.TurnCount() != 1 {
		t.Fatalf("expected 1 turn, got %d", runner.TurnCount())
	}

	// Announcer should receive ZERO messages
	if ann.Count() != 0 {
		t.Errorf("announcer received %d messages for HEARTBEAT_OK, want 0: %v", ann.Count(), ann.AllMessages())
	}

	// Noise budget should NOT be charged
	snap := eng.NoiseSnapshot()
	if snap.AnnounceCount != 0 {
		t.Errorf("NoiseSnapshot.AnnounceCount = %d, want 0", snap.AnnounceCount)
	}

	// Journal should record status: "fired", reason: "normal"
	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("Journal.Events: %v", err)
	}
	var foundHeartbeatFired bool
	for _, ev := range events {
		if ev.Type == "fasti_fired" && ev.Kind == "heartbeat" && ev.Status == "fired" {
			foundHeartbeatFired = true
			break
		}
	}
	if !foundHeartbeatFired {
		t.Errorf("expected fasti_fired kind:heartbeat status:fired in journal")
	}
}

// TestHeartbeat_NormalReplyRoutedThroughPolicy: normal reply announces and charges budget (spec D6, D7).
func TestHeartbeat_NormalReplyRoutedThroughPolicy(t *testing.T) {
	home := t.TempDir()
	hbPath := HeartbeatFilePath(home)
	hbContent := `cadence: hourly

- [ ] Check server load
`
	if err := os.WriteFile(hbPath, []byte(hbContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"

	ann := &recordingAnnouncer{}
	alertMsg := "Alert: server load exceeded 95% threshold"
	runner := &testHeartbeatRunner{
		replyFn: func(sessionID, prompt string) string {
			return alertMsg
		},
	}

	eng, err := New(cfg, home, WithClock(clk), WithAnnouncer(ann), WithTextRunner(runner.Run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Fire wake
	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background())

	if runner.TurnCount() != 1 {
		t.Fatalf("expected 1 turn, got %d", runner.TurnCount())
	}

	// Announcer should receive the announcement
	if ann.Count() != 1 {
		t.Fatalf("announcer received %d messages, want 1", ann.Count())
	}
	if ann.AllMessages()[0] != alertMsg {
		t.Errorf("announced message = %q, want %q", ann.AllMessages()[0], alertMsg)
	}

	// Noise budget should be charged
	snap := eng.NoiseSnapshot()
	if snap.AnnounceCount != 1 {
		t.Errorf("NoiseSnapshot.AnnounceCount = %d, want 1", snap.AnnounceCount)
	}
}

// TestHeartbeat_QuietWindowDefersWake: quiet window defers wake to window end with quiet_collapse (spec D6, D7).
func TestHeartbeat_QuietWindowDefersWake(t *testing.T) {
	home := t.TempDir()
	hbPath := HeartbeatFilePath(home)
	hbContent := `cadence: hourly
quiet: 11:00-13:00

- [ ] Quiet check item
`
	if err := os.WriteFile(hbPath, []byte(hbContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t0 := time.Date(2026, 11, 3, 10, 30, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"

	ann := &recordingAnnouncer{}
	runner := &testHeartbeatRunner{}

	eng, err := New(cfg, home, WithClock(clk), WithAnnouncer(ann), WithTextRunner(runner.Run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Advance to 11:00 UTC (inside quiet window [11:00, 13:00)) -> suppressed
	clk.Advance(30 * time.Minute)
	_ = eng.Sync(context.Background())

	if runner.TurnCount() != 0 {
		t.Fatalf("wake at 11:00 should be suppressed by quiet hours, got %d turns", runner.TurnCount())
	}

	// Advance to 12:00 UTC (still inside quiet window) -> suppressed and collapses
	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background())

	if runner.TurnCount() != 0 {
		t.Fatalf("wake at 12:00 should be suppressed by quiet hours, got %d turns", runner.TurnCount())
	}

	// Journal law: the deferral is recorded (once for the collapsed set —
	// the 11:00 suppression jumps wakes past the window) — never silent.
	{
		events, err := eng.Journal().Events()
		if err != nil {
			t.Fatalf("Journal.Events: %v", err)
		}
		var quietSkips int
		for _, ev := range events {
			if ev.Type == "fasti_fired" && ev.Kind == "heartbeat" && ev.Status == "skipped" && ev.Reason == "quiet_hours" {
				quietSkips++
			}
		}
		if quietSkips != 1 {
			t.Errorf("skipped quiet_hours heartbeat rows = %d, want 1 (deferral recorded, never silent)", quietSkips)
		}
	}

	// Advance to 13:00 UTC (quiet window end) -> collapses to EXACTLY ONE firing
	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background())

	if runner.TurnCount() != 1 {
		t.Fatalf("expected exactly 1 collapsed wake at window end, got %d", runner.TurnCount())
	}

	// Verify journal event has reason:"quiet_collapse"
	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("Journal.Events: %v", err)
	}
	var foundQuietCollapse bool
	for _, ev := range events {
		if ev.Type == "fasti_fired" && ev.Kind == "heartbeat" && ev.Reason == "quiet_collapse" {
			foundQuietCollapse = true
			break
		}
	}
	if !foundQuietCollapse {
		t.Errorf("expected fasti_fired kind:heartbeat reason:quiet_collapse in journal, got: %+v", events)
	}
}

// TestHeartbeat_DaemonNeverWritesFile: daemon NEVER modifies HEARTBEAT.md (spec D6, R8).
func TestHeartbeat_DaemonNeverWritesFile(t *testing.T) {
	home := t.TempDir()
	hbPath := HeartbeatFilePath(home)
	content := `cadence: hourly

- [ ] Immutable checklist
`
	if err := os.WriteFile(hbPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	statBefore, err := os.Stat(hbPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"

	runner := &testHeartbeatRunner{
		replyFn: func(sessionID, prompt string) string {
			return "HEARTBEAT_OK"
		},
	}

	eng, err := New(cfg, home, WithClock(clk), WithTextRunner(runner.Run))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Fire multiple wakes
	for range 5 {
		clk.Advance(1 * time.Hour)
		_ = eng.Sync(context.Background())
	}

	eng.Stop()

	statAfter, err := os.Stat(hbPath)
	if err != nil {
		t.Fatalf("Stat after: %v", err)
	}

	// Mtime and size must be completely unchanged
	if !statBefore.ModTime().Equal(statAfter.ModTime()) {
		t.Errorf("HEARTBEAT.md mtime changed: before=%v, after=%v", statBefore.ModTime(), statAfter.ModTime())
	}
	if statBefore.Size() != statAfter.Size() {
		t.Errorf("HEARTBEAT.md size changed: before=%d, after=%d", statBefore.Size(), statAfter.Size())
	}

	bytesAfter, err := os.ReadFile(hbPath)
	if err != nil {
		t.Fatalf("ReadFile after: %v", err)
	}
	if string(bytesAfter) != content {
		t.Errorf("HEARTBEAT.md content changed: %s", string(bytesAfter))
	}
}
