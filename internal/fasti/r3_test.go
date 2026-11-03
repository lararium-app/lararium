package fasti

import (
	"context"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

func TestR3_SkipOnErrTurnInFlight(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()

	runner := func(ctx context.Context, sessionID, prompt string) error {
		return ErrTurnInFlight
	}

	eng, err := New(cfg, home, WithClock(clk), WithRunner(runner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := eng.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "interval", Every: "10m"},
		Prompt:   "skip test prompt",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Advance clock to due time (12:10 UTC)
	clk.Advance(10 * time.Minute)
	_ = eng.Sync(context.Background())

	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}

	var foundSkip bool
	for _, ev := range events {
		if ev.Status == "skipped" && ev.Reason == "session_busy" && ev.JobID == job.ID {
			foundSkip = true
			break
		}
	}
	if !foundSkip {
		t.Fatalf("expected skip event with session_busy, got %+v", events)
	}

	j, err := eng.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if j.ConsecutiveSkips != 1 {
		t.Errorf("ConsecutiveSkips = %d, want 1", j.ConsecutiveSkips)
	}
	if j.Paused {
		t.Errorf("job should not be paused after 1 skip")
	}
}

func TestR3_SelfPauseOnFifthConsecutiveSkip(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()

	runner := func(ctx context.Context, sessionID, prompt string) error {
		return ErrTurnInFlight
	}

	eng, err := New(cfg, home, WithClock(clk), WithRunner(runner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := eng.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "interval", Every: "5m"},
		Prompt:   "self-pause test prompt",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Trigger 5 consecutive skips by advancing time through 5 due windows
	for step := 1; step <= 5; step++ {
		clk.Advance(5 * time.Minute)
		_ = eng.Sync(context.Background())

		j, err := eng.GetJob(context.Background(), job.ID)
		if err != nil {
			t.Fatalf("GetJob step %d: %v", step, err)
		}

		if step < 5 {
			if j.ConsecutiveSkips != step {
				t.Fatalf("step %d: ConsecutiveSkips = %d, want %d", step, j.ConsecutiveSkips, step)
			}
			if j.Paused {
				t.Fatalf("step %d: job prematurely paused", step)
			}
		} else {
			// On the 5th consecutive skip, job must self-pause! (spec D3/R3)
			if j.ConsecutiveSkips != 5 {
				t.Fatalf("step 5: ConsecutiveSkips = %d, want 5", j.ConsecutiveSkips)
			}
			if !j.Paused {
				t.Fatalf("step 5: job expected to be paused, got paused=false")
			}
			// Spec D3 / M11: self-pause pauses itself: set Paused only; Enabled stays true
			if !j.Enabled {
				t.Fatalf("step 5: job expected to stay enabled (enabled=true), got enabled=false")
			}
		}
	}

	// Verify self-pause event was recorded in journal
	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}

	var foundPauseEvent bool
	for _, ev := range events {
		if ev.Type == "fasti_job_paused" && ev.Reason == "consecutive_skips" && ev.JobID == job.ID {
			foundPauseEvent = true
			break
		}
	}
	if !foundPauseEvent {
		t.Fatalf("expected fasti_job_paused event with consecutive_skips, got: %+v", events)
	}

	// Advance time further: verify no more firings occur for this paused job
	clk.Advance(30 * time.Minute)
	eventsAfter, _ := eng.Journal().Events()
	if len(eventsAfter) != len(events) {
		t.Fatalf("paused job emitted additional events after self-pause: %+v", eventsAfter[len(events):])
	}
}

func TestR3_LoopIndependenceFromWorkerLatency(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()

	// Channel-gated fake runner to simulate slow provider latency
	unblockFirstJob := make(chan struct{})
	job1Started := make(chan struct{})

	runner := func(ctx context.Context, sessionID, prompt string) error {
		if prompt == "blocking prompt" {
			close(job1Started)
			select {
			case <-unblockFirstJob:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}

	eng, err := New(cfg, home, WithClock(clk), WithRunner(runner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Add Job 1 (fires at 12:05)
	job1, err := eng.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "interval", Every: "5m"},
		Prompt:   "blocking prompt",
	})
	if err != nil {
		t.Fatalf("AddJob 1: %v", err)
	}

	// Advance clock to fire Job 1
	clk.Advance(5 * time.Minute)

	// Wait until worker starts executing Job 1
	<-job1Started

	// WHILE WORKER IS BLOCKED ON JOB 1:
	// Verify that the command loop can still process commands and re-arm timers promptly!
	// 1. Command processing (AddJob) succeeds without blocking
	job2, err := eng.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "interval", Every: "1m"},
		Prompt:   "independent job prompt",
	})
	if err != nil {
		t.Fatalf("AddJob while worker blocked failed: %v", err)
	}

	// 2. Advance clock by 1 minute: Job 2 becomes due while worker is STILL blocked!
	// The command loop must detect engine_busy at dispatch, record the event,
	// and arm retry at +60s without stalling or deadlocking!
	clk.Advance(1 * time.Minute)

	// Issue a command to ensure the loop has processed the due timer event
	_, _ = eng.GetJob(context.Background(), job2.ID)

	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}

	var foundEngineBusy bool
	for _, ev := range events {
		if ev.Reason == "engine_busy" && ev.JobID == job2.ID {
			foundEngineBusy = true
			break
		}
	}
	if !foundEngineBusy {
		t.Fatalf("expected engine_busy event for job2, got %+v", events)
	}

	// 3. Unblock Job 1: worker completes
	close(unblockFirstJob)
	_ = eng.Sync(context.Background())

	j1, _ := eng.GetJob(context.Background(), job1.ID)
	if j1.LastFire == "" {
		t.Fatalf("job 1 did not record last fire after unblocking")
	}

	// Advance 60s for Job 2 retry: now that worker is free, retry dispatches and fires
	clk.Advance(60 * time.Second)
	_ = eng.Sync(context.Background())

	j2, _ := eng.GetJob(context.Background(), job2.ID)
	if j2.LastFire == "" {
		t.Fatalf("job 2 retry did not fire after worker freed")
	}
}

func TestD3_StopDuringInFlightFiring(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)
	cfg := DefaultConfig()

	workerStarted := make(chan struct{})

	runner := func(ctx context.Context, sessionID, prompt string) error {
		close(workerStarted)
		<-ctx.Done()
		return ctx.Err()
	}

	eng, err := New(cfg, home, WithClock(clk), WithRunner(runner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := eng.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "interval", Every: "5m"},
		Prompt:   "in-flight-firing-prompt",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Advance clock to trigger firing
	clk.Advance(5 * time.Minute)

	// Wait until worker is blocked mid-turn
	<-workerStarted

	// Call Stop() while worker is blocked mid-turn (Spec B4)
	eng.Stop()

	// Assert journal recorded fasti_fired with status: skipped, reason: shutdown (Spec D13, B4)
	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}

	var foundShutdown bool
	for _, ev := range events {
		if ev.Status == "skipped" && ev.Reason == "shutdown" && ev.JobID == job.ID {
			foundShutdown = true
			break
		}
	}
	if !foundShutdown {
		t.Fatalf("expected journal to contain fasti_fired with status: skipped, reason: shutdown, got: %+v", events)
	}
}

func TestM5_IntervalAnchorDriftSlowWorker(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)
	cfg := DefaultConfig()

	// Slow worker taking 2 minutes per turn
	runner := func(ctx context.Context, sessionID, prompt string) error {
		clk.Advance(2 * time.Minute)
		return nil
	}

	eng, err := New(cfg, home, WithClock(clk), WithRunner(runner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// every=5m. Start at 12:00.
	job, err := eng.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "interval", Every: "5m"},
		Prompt:   "slow-worker-prompt",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Initial due: 12:05
	wantDue1 := time.Date(2026, 11, 2, 12, 5, 0, 0, time.UTC)
	j, _ := eng.GetJob(context.Background(), job.ID)
	d1, _ := time.Parse(time.RFC3339, j.NextDue)
	if !d1.Equal(wantDue1) {
		t.Fatalf("initial due = %v, want %v", d1, wantDue1)
	}

	// 1. Advance to 12:05. Worker runs from 12:05 to 12:07 (2m).
	clk.Advance(5 * time.Minute)
	_ = eng.Sync(context.Background())

	// Next due must be 12:10 (exact 5m tick from prev due 12:05, NOT 12:12!)
	wantDue2 := time.Date(2026, 11, 2, 12, 10, 0, 0, time.UTC)
	j, _ = eng.GetJob(context.Background(), job.ID)
	d2, _ := time.Parse(time.RFC3339, j.NextDue)
	if !d2.Equal(wantDue2) {
		t.Fatalf("next due after turn 1 = %v, want %v (drift detected!)", d2, wantDue2)
	}

	// 2. Advance to 12:10 (from current 12:07 -> advance 3m). Worker runs from 12:10 to 12:12 (2m).
	clk.Advance(wantDue2.Sub(clk.Now().UTC()))
	_ = eng.Sync(context.Background())

	// Next due must be 12:15 (exact 5m tick, NOT 12:14 or 12:17!)
	wantDue3 := time.Date(2026, 11, 2, 12, 15, 0, 0, time.UTC)
	j, _ = eng.GetJob(context.Background(), job.ID)
	d3, _ := time.Parse(time.RFC3339, j.NextDue)
	if !d3.Equal(wantDue3) {
		t.Fatalf("next due after turn 2 = %v, want %v (drift detected!)", d3, wantDue3)
	}
}

func TestM10_SilentJobDeath(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)
	cfg := DefaultConfig()

	eng1, err := New(cfg, home, WithClock(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := eng1.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := eng1.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "interval", Every: "5m"},
		Prompt:   "death-test-prompt",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	eng1.Stop()

	// Corrupt schedule in jobs.json to simulate recalculation failure after firing
	jobs, err := LoadJobs(JobsFilePath(home))
	if err != nil {
		t.Fatalf("LoadJobs: %v", err)
	}
	for i := range jobs {
		if jobs[i].ID == job.ID {
			jobs[i].Schedule.Kind = "corrupted_kind"
		}
	}
	if err := SaveJobs(JobsFilePath(home), jobs); err != nil {
		t.Fatalf("SaveJobs: %v", err)
	}

	// Restart engine
	eng2, err := New(cfg, home, WithClock(clk))
	if err != nil {
		t.Fatalf("New eng2: %v", err)
	}
	defer eng2.Stop()

	if err := eng2.Start(context.Background()); err != nil {
		t.Fatalf("Start eng2: %v", err)
	}

	// Advance clock to fire the job
	clk.Advance(5 * time.Minute)
	_ = eng2.Sync(context.Background())

	// Assert event logged in journal: fasti_fired with status: failed, reason: schedule_recompute_failed
	events, err := eng2.Journal().Events()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}

	var foundFailure bool
	for _, ev := range events {
		if ev.Status == "failed" && ev.Reason == "schedule_recompute_failed" && ev.JobID == job.ID {
			foundFailure = true
			break
		}
	}
	if !foundFailure {
		t.Fatalf("expected journal to contain schedule_recompute_failed event, got: %+v", events)
	}

	// Assert job is auto-paused
	j, err := eng2.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !j.Paused {
		t.Fatalf("job expected to be auto-paused, got paused=false")
	}
}
