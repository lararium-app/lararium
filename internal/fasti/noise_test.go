package fasti

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

type recordingAnnouncer struct {
	mu       sync.Mutex
	messages []string
}

func (a *recordingAnnouncer) Announce(ctx context.Context, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append(a.messages, text)
	return nil
}

func (a *recordingAnnouncer) AllMessages() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	res := make([]string, len(a.messages))
	copy(res, a.messages)
	return res
}

func (a *recordingAnnouncer) Count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.messages)
}

// TestNoise_Budget8Allowed9thSilenced: 8 announcements pass, 9th dropped with reason:"noise_budget" (spec D7).
func TestNoise_Budget8Allowed9thSilenced(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"
	cfg.MaxAnnouncePerDay = 8

	ann := &recordingAnnouncer{}
	var runCount int
	runner := func(ctx context.Context, sessionID, prompt string) (string, error) {
		runCount++
		return fmt.Sprintf("Announcement payload %d", runCount), nil
	}

	eng, err := New(cfg, home, WithClock(clk), WithAnnouncer(ann), WithTextRunner(runner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := eng.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "interval", Every: "1h"},
		Prompt:   "budget test prompt",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Fire 8 times, spaced by 1 hour (all within same UTC day)
	for i := 1; i <= 8; i++ {
		clk.Advance(1 * time.Hour)
		_ = eng.Sync(context.Background())

		if ann.Count() != i {
			t.Fatalf("iteration %d: expected %d announcements, got %d", i, i, ann.Count())
		}
	}

	// 9th firing within same day
	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background())

	// Announcer should still have only 8 messages
	if ann.Count() != 8 {
		t.Fatalf("expected announcer count 8 after 9th firing, got %d", ann.Count())
	}

	// Verify journal recorded 9th firing as status:"silenced", reason:"noise_budget"
	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("Journal.Events: %v", err)
	}

	var silencedBudgetCount int
	for _, ev := range events {
		if ev.Type == "fasti_fired" && ev.JobID == job.ID && ev.Status == "silenced" && ev.Reason == "noise_budget" {
			silencedBudgetCount++
		}
	}
	if silencedBudgetCount != 1 {
		t.Errorf("expected 1 silenced noise_budget event, got %d", silencedBudgetCount)
	}
}

// TestNoise_UTCDayRolloverResetsCount: UTC-day rollover resets budget counter to 0 (spec D7).
func TestNoise_UTCDayRolloverResetsCount(t *testing.T) {
	home := t.TempDir()
	// Start late in the day: 2026-11-03 23:00:00 UTC
	t0 := time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"
	cfg.MaxAnnouncePerDay = 8

	ann := &recordingAnnouncer{}
	var runCount int
	runner := func(ctx context.Context, sessionID, prompt string) (string, error) {
		runCount++
		return fmt.Sprintf("Announcement payload %d", runCount), nil
	}

	eng, err := New(cfg, home, WithClock(clk), WithAnnouncer(ann), WithTextRunner(runner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Add job with 1-minute interval
	_, err = eng.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "interval", Every: "1m"},
		Prompt:   "rollover test prompt",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Fire 8 times before midnight (23:01 to 23:08)
	for i := 1; i <= 8; i++ {
		clk.Advance(1 * time.Minute)
		_ = eng.Sync(context.Background())
	}
	if ann.Count() != 8 {
		t.Fatalf("expected 8 announcements before midnight, got %d", ann.Count())
	}

	// 9th firing at 23:09 is silenced by noise_budget
	clk.Advance(1 * time.Minute)
	_ = eng.Sync(context.Background())
	if ann.Count() != 8 {
		t.Fatalf("expected 9th firing to be silenced, got %d", ann.Count())
	}

	// Advance clock past UTC midnight to 2026-11-04 00:01:00 UTC
	// Current is 23:09, so advance by 52 minutes
	clk.Advance(52 * time.Minute)
	_ = eng.Sync(context.Background())

	// 10th firing occurs on the new UTC day -> budget is reset, so announcement passes!
	if ann.Count() != 9 {
		t.Fatalf("expected 9 announcements after UTC day rollover, got %d", ann.Count())
	}
}

// TestNoise_DedupDropsSecondIdenticalWithin200Chars: identical normalized text within 24h dropped, distinct texts pass, 24h expiry (spec D7).
func TestNoise_DedupDropsSecondIdenticalWithin200Chars(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"

	ann := &recordingAnnouncer{}
	var currentReply string
	runner := func(ctx context.Context, sessionID, prompt string) (string, error) {
		return currentReply, nil
	}

	eng, err := New(cfg, home, WithClock(clk), WithAnnouncer(ann), WithTextRunner(runner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := eng.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "interval", Every: "1h"},
		Prompt:   "dedup test prompt",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// 1. Send first announcement
	currentReply = "System report: all servers operational. Memory 45%, Disk 60%."
	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background())

	if ann.Count() != 1 {
		t.Fatalf("expected 1 announcement, got %d", ann.Count())
	}

	// 2. Send second announcement identical under normalization (extra whitespace + lowercase)
	currentReply = "  system  report:   all servers operational. memory 45%, disk 60%.  "
	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background())

	if ann.Count() != 1 {
		t.Fatalf("expected second identical announcement to be deduplicated, count=%d", ann.Count())
	}

	// Verify journal recorded status:"silenced", reason:"dedup"
	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("Journal.Events: %v", err)
	}
	var dedupSilencedFound bool
	for _, ev := range events {
		if ev.Type == "fasti_fired" && ev.JobID == job.ID && ev.Status == "silenced" && ev.Reason == "dedup" {
			dedupSilencedFound = true
			break
		}
	}
	if !dedupSilencedFound {
		t.Errorf("expected fasti_fired status:silenced reason:dedup in journal")
	}

	// 3. Send distinct announcement -> passes
	currentReply = "System report: service degraded on worker 2!"
	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background())

	if ann.Count() != 2 {
		t.Fatalf("expected distinct announcement to pass, count=%d", ann.Count())
	}

	// 4. Pause job while advancing clock past 24h dedup window so no intermediate interval firings occur
	if err := eng.PauseJob(context.Background(), job.ID); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}

	clk.Advance(25 * time.Hour)
	_ = eng.Sync(context.Background())

	// Resume job and send original announcement again -> now passes because >24h elapsed
	currentReply = "System report: all servers operational. Memory 45%, Disk 60%."
	if err := eng.ResumeJob(context.Background(), job.ID); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}

	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background())

	if ann.Count() != 3 {
		t.Fatalf("expected original announcement to pass after 24h expiry, count=%d", ann.Count())
	}
}

// TestNoise_DigestAfter5OverflowDaysThenStreakReset: 5 overflow days within 7 days fires 1 digest notice and resets streak (spec D7).
func TestNoise_DigestAfter5OverflowDaysThenStreakReset(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"
	cfg.MaxAnnouncePerDay = 1 // 1 allowed per day so 2nd is overflow

	ann := &recordingAnnouncer{}
	var runCount int
	runner := func(ctx context.Context, sessionID, prompt string) (string, error) {
		runCount++
		return fmt.Sprintf("Daily notification %d", runCount), nil
	}

	eng, err := New(cfg, home, WithClock(clk), WithAnnouncer(ann), WithTextRunner(runner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, err = eng.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "interval", Every: "1h"},
		Prompt:   "streak test prompt",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Days 1 through 4: cause 1 success + 1 overflow on each day
	for day := 1; day <= 4; day++ {
		// First run: allowed
		clk.Advance(1 * time.Hour)
		_ = eng.Sync(context.Background())

		// Second run: overflow
		clk.Advance(1 * time.Hour)
		_ = eng.Sync(context.Background())

		// Advance to next day 10:00 UTC
		clk.Advance(22 * time.Hour)
		_ = eng.Sync(context.Background())
	}

	// Check messages so far: exactly 4 regular announcements, 0 digest notices
	for _, m := range ann.AllMessages() {
		if strings.Contains(m, "noise budget overflow") {
			t.Fatalf("unexpected noise digest before day 5: %v", m)
		}
	}

	// Day 5: 1 allowed + 1 overflow (this overflow is the 5th in 7 days!)
	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background()) // allowed

	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background()) // 5th overflow day! Dispatches digest notice!

	// Check announcer messages for the digest notice
	var digestCount int
	for _, m := range ann.AllMessages() {
		if strings.Contains(m, "noise budget overflow") {
			digestCount++
		}
	}
	if digestCount != 1 {
		t.Fatalf("expected exactly 1 noise digest notice after 5 overflow days, got %d. All messages: %v", digestCount, ann.AllMessages())
	}

	// Verify journal recorded fasti_notice kind:"noise_digest"
	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("Journal.Events: %v", err)
	}
	var foundDigestNotice bool
	for _, ev := range events {
		if ev.Type == "fasti_notice" && ev.Kind == "noise_digest" {
			foundDigestNotice = true
			break
		}
	}
	if !foundDigestNotice {
		t.Errorf("expected fasti_notice kind:noise_digest in journal")
	}

	// Verify streak was reset: Day 6 overflow should NOT emit a second digest
	clk.Advance(22 * time.Hour) // to day 6
	_ = eng.Sync(context.Background())

	clk.Advance(1 * time.Hour) // allowed
	_ = eng.Sync(context.Background())
	clk.Advance(1 * time.Hour) // overflow
	_ = eng.Sync(context.Background())

	var digestCountAfterDay6 int
	for _, m := range ann.AllMessages() {
		if strings.Contains(m, "noise budget overflow") {
			digestCountAfterDay6++
		}
	}
	if digestCountAfterDay6 != 1 {
		t.Fatalf("streak should be reset: expected 1 total digest notice, got %d", digestCountAfterDay6)
	}
}

// TestNoise_OperationalNoticeBypassesQuietAndBudget: auto-pause notice announces despite quiet hours and exhausted budget (spec D3, D7).
func TestNoise_OperationalNoticeBypassesQuietAndBudget(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"
	cfg.QuietHours = "00:00-23:59" // In quiet hours all day
	cfg.MaxAnnouncePerDay = 1      // Low budget

	ann := &recordingAnnouncer{}
	var turnCallCount int
	runner := func(ctx context.Context, sessionID, prompt string) (string, error) {
		turnCallCount++
		// Simulate session busy error so job gets skipped repeatedly
		return "", fmt.Errorf("session active: turn in flight")
	}

	eng, err := New(cfg, home, WithClock(clk), WithAnnouncer(ann), WithTextRunner(runner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := eng.AddJob(context.Background(), Job{
		ID:       "j_autopause_test",
		Name:     "AutoPause Test Job",
		Schedule: Schedule{Kind: "interval", Every: "1m"},
		Prompt:   "failing prompt",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Cause 5 consecutive skips (session busy)
	for i := 1; i <= 5; i++ {
		clk.Advance(1 * time.Minute)
		_ = eng.Sync(context.Background())
	}

	// Verify job is now paused
	j, err := eng.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !j.Paused {
		t.Errorf("job should be paused after 5 consecutive skips")
	}

	// Verify announcer received the auto-pause notice (even though inside quiet hours and budget)
	msgs := ann.AllMessages()
	var autoPauseNoticeReceived bool
	for _, m := range msgs {
		if strings.Contains(m, "paused after 5 consecutive skips") {
			autoPauseNoticeReceived = true
			break
		}
	}
	if !autoPauseNoticeReceived {
		t.Fatalf("expected auto-pause notice to bypass quiet hours and budget, received messages: %v", msgs)
	}

	// Verify fasti_notice was recorded in journal
	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("Journal.Events: %v", err)
	}
	var foundAutoPauseNotice bool
	for _, ev := range events {
		if ev.Type == "fasti_notice" && ev.Kind == "auto_pause" && ev.JobID == job.ID {
			foundAutoPauseNotice = true
			break
		}
	}
	if !foundAutoPauseNotice {
		t.Errorf("expected fasti_notice kind:auto_pause in journal")
	}
}

// TestNoise_StateJsonSurvivesEngineRestartMidBudget: state.json survives engine restart mid-budget (spec D7).
func TestNoise_StateJsonSurvivesEngineRestartMidBudget(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"
	cfg.MaxAnnouncePerDay = 8

	ann1 := &recordingAnnouncer{}
	var runCount int
	runner1 := func(ctx context.Context, sessionID, prompt string) (string, error) {
		runCount++
		return fmt.Sprintf("Announcement %d", runCount), nil
	}

	eng1, err := New(cfg, home, WithClock(clk), WithAnnouncer(ann1), WithTextRunner(runner1))
	if err != nil {
		t.Fatalf("New eng1: %v", err)
	}

	if err := eng1.Start(context.Background()); err != nil {
		t.Fatalf("Start eng1: %v", err)
	}

	_, err = eng1.AddJob(context.Background(), Job{
		ID:       "j_restart_budget",
		Schedule: Schedule{Kind: "interval", Every: "1h"},
		Prompt:   "restart budget prompt",
	})
	if err != nil {
		t.Fatalf("AddJob eng1: %v", err)
	}

	// Fire 5 times on eng1
	for i := 1; i <= 5; i++ {
		clk.Advance(1 * time.Hour)
		_ = eng1.Sync(context.Background())
	}
	if ann1.Count() != 5 {
		t.Fatalf("eng1 expected 5 announcements, got %d", ann1.Count())
	}

	// Stop eng1
	eng1.Stop()

	// Verify state.json exists on disk
	stateFile := filepath.Join(home, "fasti", "state.json")
	if _, err := os.Stat(stateFile); os.IsNotExist(err) {
		t.Fatalf("state.json does not exist at %s", stateFile)
	}

	// Start eng2 on same directory with same clock
	ann2 := &recordingAnnouncer{}
	runner2 := func(ctx context.Context, sessionID, prompt string) (string, error) {
		runCount++
		return fmt.Sprintf("Announcement %d", runCount), nil
	}

	eng2, err := New(cfg, home, WithClock(clk), WithAnnouncer(ann2), WithTextRunner(runner2))
	if err != nil {
		t.Fatalf("New eng2: %v", err)
	}
	defer eng2.Stop()

	if err := eng2.Start(context.Background()); err != nil {
		t.Fatalf("Start eng2: %v", err)
	}

	// Fire 3 more times on eng2 (5 + 3 = 8)
	for i := 1; i <= 3; i++ {
		clk.Advance(1 * time.Hour)
		_ = eng2.Sync(context.Background())
	}
	if ann2.Count() != 3 {
		t.Fatalf("eng2 expected 3 announcements, got %d", ann2.Count())
	}

	// Fire 9th time: should be silenced by noise_budget because 5+3=8 already used!
	clk.Advance(1 * time.Hour)
	_ = eng2.Sync(context.Background())

	if ann2.Count() != 3 {
		t.Fatalf("eng2 9th firing should have been silenced by budget, count=%d", ann2.Count())
	}
}

// TestNoise_NoAnnounceWithoutAnnouncerSet: nil announcer still journals status: "fired" (spec D7).
func TestNoise_NoAnnounceWithoutAnnouncerSet(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"

	var runCount int
	runner := func(ctx context.Context, sessionID, prompt string) (string, error) {
		runCount++
		return "text returned by model with nil announcer", nil
	}

	// Do NOT pass WithAnnouncer -> e.announcer is nil
	eng, err := New(cfg, home, WithClock(clk), WithTextRunner(runner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := eng.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "interval", Every: "1h"},
		Prompt:   "nil announcer prompt",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background())

	if runCount != 1 {
		t.Fatalf("runner should have been called once, got %d", runCount)
	}

	// Verify journal has status:"fired"
	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("Journal.Events: %v", err)
	}
	var foundFired bool
	for _, ev := range events {
		if ev.Type == "fasti_fired" && ev.JobID == job.ID && ev.Status == "fired" {
			foundFired = true
			break
		}
	}
	if !foundFired {
		t.Errorf("expected fasti_fired status:fired in journal with nil announcer")
	}
}
