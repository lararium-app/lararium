package fasti

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

type testAnnouncer struct {
	mu       sync.Mutex
	messages []string
}

func (a *testAnnouncer) Announce(ctx context.Context, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append(a.messages, text)
	return nil
}

func (a *testAnnouncer) Messages() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.messages))
	copy(out, a.messages)
	return out
}

func TestQuiet_ParseAndSpanLogic(t *testing.T) {
	loc := time.UTC

	// Normal window (09:00-17:00)
	wNormal := "09:00-17:00"
	t0859 := time.Date(2026, 11, 3, 8, 59, 59, 0, loc)
	t0900 := time.Date(2026, 11, 3, 9, 0, 0, 0, loc)
	t1659 := time.Date(2026, 11, 3, 16, 59, 59, 0, loc)
	t1700 := time.Date(2026, 11, 3, 17, 0, 0, 0, loc)

	if IsInQuietWindow(wNormal, t0859, loc) {
		t.Errorf("08:59 should not be in quiet window %s", wNormal)
	}
	if !IsInQuietWindow(wNormal, t0900, loc) {
		t.Errorf("09:00 should be in quiet window %s (inclusive start)", wNormal)
	}
	if !IsInQuietWindow(wNormal, t1659, loc) {
		t.Errorf("16:59 should be in quiet window %s", wNormal)
	}
	if IsInQuietWindow(wNormal, t1700, loc) {
		t.Errorf("17:00 should not be in quiet window %s (exclusive end)", wNormal)
	}

	wEndNormal := QuietWindowEnd(wNormal, t0900, loc)
	if !wEndNormal.Equal(t1700) {
		t.Errorf("QuietWindowEnd = %v, want %v", wEndNormal, t1700)
	}

	// Midnight-spanning window (22:00-07:00)
	wMidnight := "22:00-07:00"
	t2159 := time.Date(2026, 11, 3, 21, 59, 59, 0, loc)
	t2200 := time.Date(2026, 11, 3, 22, 0, 0, 0, loc)
	t0659 := time.Date(2026, 11, 4, 6, 59, 59, 0, loc)
	t0700 := time.Date(2026, 11, 4, 7, 0, 0, 0, loc)

	if IsInQuietWindow(wMidnight, t2159, loc) {
		t.Errorf("21:59 should not be in quiet window %s", wMidnight)
	}
	if !IsInQuietWindow(wMidnight, t2200, loc) {
		t.Errorf("22:00 should be in quiet window %s (inclusive start)", wMidnight)
	}
	if !IsInQuietWindow(wMidnight, t0659, loc) {
		t.Errorf("06:59 should be in quiet window %s", wMidnight)
	}
	if IsInQuietWindow(wMidnight, t0700, loc) {
		t.Errorf("07:00 should not be in quiet window %s (exclusive end)", wMidnight)
	}

	wEndMidnight := QuietWindowEnd(wMidnight, t2200, loc)
	if !wEndMidnight.Equal(t0700) {
		t.Errorf("QuietWindowEnd = %v, want %v", wEndMidnight, t0700)
	}

	// Empty window
	if IsInQuietWindow("", t0900, loc) {
		t.Errorf("empty window should never be in quiet")
	}
}

func TestQuiet_AnnounceOnlySilencesAnnouncementsButFires(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"
	ann := &testAnnouncer{}
	runner := func(ctx context.Context, sessionID, prompt string) (string, error) {
		return "candidate text to announce", nil
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
		Schedule: Schedule{Kind: "interval", Every: "30m"},
		Prompt:   "announce-only prompt",
		Quiet: &QuietOverride{
			Window: "12:00-14:00",
			Mode:   "announce_only",
		},
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Advance clock by 30m to 12:30 (inside quiet window 12:00-14:00)
	clk.Advance(30 * time.Minute)
	_ = eng.Sync(context.Background())

	// Verify job fired (runs incremented)
	j, err := eng.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if j.BudgetCounters.TotalRuns != 1 {
		t.Errorf("TotalRuns = %d, want 1", j.BudgetCounters.TotalRuns)
	}

	// Verify announcement was silenced (announcer never called)
	if msgs := ann.Messages(); len(msgs) != 0 {
		t.Errorf("announcer received %d messages, want 0 (silenced): %v", len(msgs), msgs)
	}

	// Verify journal event status:"silenced", reason:"quiet_hours"
	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("Journal.Events: %v", err)
	}
	var foundSilenced bool
	for _, ev := range events {
		if ev.Type == "fasti_fired" && ev.JobID == job.ID && ev.Status == "silenced" && ev.Reason == "quiet_hours" {
			foundSilenced = true
			break
		}
	}
	if !foundSilenced {
		t.Fatalf("expected fasti_fired status:silenced reason:quiet_hours, got: %+v", events)
	}
}

func TestQuiet_DefersCollapsesMultipleDuesToOneFiringAtEnd(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"
	ann := &testAnnouncer{}
	var runCount int
	runner := func(ctx context.Context, sessionID, prompt string) (string, error) {
		runCount++
		return "collapsed firing result", nil
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
		Schedule: Schedule{Kind: "interval", Every: "30m"},
		Prompt:   "defers test prompt",
		Quiet: &QuietOverride{
			Window: "12:00-14:00",
			Mode:   "defers",
		},
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Advance clock through the quiet window: 12:30, 13:00, 13:30
	clk.Advance(30 * time.Minute) // 12:30
	_ = eng.Sync(context.Background())
	clk.Advance(30 * time.Minute) // 13:00
	_ = eng.Sync(context.Background())
	clk.Advance(30 * time.Minute) // 13:30
	_ = eng.Sync(context.Background())

	// Verify NO firings occurred during quiet window
	if runCount != 0 {
		t.Errorf("runs during quiet window = %d, want 0", runCount)
	}

	// Journal law: the deferral is recorded once for the collapsed set
	// (first suppressed due at 12:30; later dues are subsumed by the jump).
	{
		events, err := eng.Journal().Events()
		if err != nil {
			t.Fatalf("Journal.Events: %v", err)
		}
		var deferredCount int
		for _, ev := range events {
			if ev.Type == "fasti_fired" && ev.JobID == job.ID && ev.Status == "deferred" && ev.Reason == "quiet_hours" {
				deferredCount++
			}
		}
		if deferredCount != 1 {
			t.Errorf("deferred quiet_hours rows = %d, want 1 (deferral of the collapsed set is recorded, never silent)", deferredCount)
		}
	}

	// Advance clock to window end (14:00)
	clk.Advance(30 * time.Minute) // 14:00
	_ = eng.Sync(context.Background())

	// Verify EXACTLY ONE firing occurred at window end
	if runCount != 1 {
		t.Errorf("runs at window end = %d, want 1", runCount)
	}

	// Verify journal event carrying reason:"quiet_collapse"
	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("Journal.Events: %v", err)
	}
	var foundCollapse bool
	for _, ev := range events {
		if ev.Type == "fasti_fired" && ev.JobID == job.ID && ev.Status == "fired" && ev.Reason == "quiet_collapse" {
			foundCollapse = true
			break
		}
	}
	if !foundCollapse {
		t.Fatalf("expected fasti_fired reason:quiet_collapse at window end, got: %+v", events)
	}

	// Verify the announcement at 14:00 succeeded outside quiet window
	if msgs := ann.Messages(); len(msgs) != 1 || msgs[0] != "collapsed firing result" {
		t.Errorf("announcer messages = %v, want ['collapsed firing result']", msgs)
	}
}

func TestQuiet_BootInsideQuietWindowDefersOfflineReplay(t *testing.T) {
	home := t.TempDir()
	tMissed := time.Date(2026, 11, 3, 11, 30, 0, 0, time.UTC)
	tBoot := time.Date(2026, 11, 3, 12, 30, 0, 0, time.UTC)

	// Pre-create a job that was due at 11:30 (offline missed due)
	jobs := []Job{
		{
			ID:      "j_boot_quiet",
			Name:    "Boot Quiet Job",
			Prompt:  "boot offline prompt",
			Session: "cron-j_boot_quiet",
			Schedule: Schedule{
				Kind:  "interval",
				Every: "1h",
			},
			Enabled: true,
			NextDue: tMissed.Format(time.RFC3339),
			Quiet: &QuietOverride{
				Window: "12:00-14:00",
				Mode:   "defers",
			},
		},
	}
	if err := SaveJobs(JobsFilePath(home), jobs); err != nil {
		t.Fatalf("SaveJobs: %v", err)
	}

	clk := loop.NewFakeClock(tBoot)
	cfg := DefaultConfig()
	cfg.LocalTimezone = "UTC"
	ann := &testAnnouncer{}
	var runCount int
	runner := func(ctx context.Context, sessionID, prompt string) (string, error) {
		runCount++
		return "boot replay done", nil
	}

	eng, err := New(cfg, home, WithClock(clk), WithAnnouncer(ann), WithTextRunner(runner))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	// Boot at 12:30 (inside quiet window 12:00-14:00)
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = eng.Sync(context.Background())

	// Verify NO firing occurred at boot
	if runCount != 0 {
		t.Errorf("runs at boot inside quiet window = %d, want 0", runCount)
	}

	// Journal law: the deferred offline replay is recorded at boot itself.
	{
		events, err := eng.Journal().Events()
		if err != nil {
			t.Fatalf("Journal.Events: %v", err)
		}
		var deferredBoot bool
		for _, ev := range events {
			if ev.Type == "fasti_fired" && ev.JobID == "j_boot_quiet" && ev.Status == "deferred" && ev.Reason == "quiet_hours" {
				deferredBoot = true
				break
			}
		}
		if !deferredBoot {
			t.Fatalf("expected fasti_fired status:deferred reason:quiet_hours at boot, got: %+v", events)
		}
	}

	// Advance clock to window end (14:00)
	clk.Advance(90 * time.Minute)
	_ = eng.Sync(context.Background())

	// Verify offline replay fired into the collapse at window end
	if runCount != 1 {
		t.Errorf("runs at window end = %d, want 1", runCount)
	}

	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("Journal.Events: %v", err)
	}
	var foundCollapse bool
	for _, ev := range events {
		if ev.Type == "fasti_fired" && ev.JobID == "j_boot_quiet" && ev.Status == "fired" && ev.Reason == "quiet_collapse" {
			foundCollapse = true
			break
		}
	}
	if !foundCollapse {
		t.Fatalf("expected fasti_fired reason:quiet_collapse after boot deferral, got: %+v", events)
	}
}
