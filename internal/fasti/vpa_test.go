package fasti

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

func TestVPa_CronGrammarCorpus(t *testing.T) {
	loc := time.UTC
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, loc) // Monday, June 1, 2026

	tests := []struct {
		name     string
		expr     string
		from     time.Time
		wantDue  time.Time
		wantFail bool
	}{
		{
			name:    "every minute wildcard",
			expr:    "* * * * *",
			from:    base,
			wantDue: time.Date(2026, 6, 1, 0, 1, 0, 0, loc),
		},
		{
			name:    "lists in minutes",
			expr:    "1,15,30 * * * *",
			from:    base,
			wantDue: time.Date(2026, 6, 1, 0, 1, 0, 0, loc),
		},
		{
			name:    "lists in minutes next element",
			expr:    "1,15,30 * * * *",
			from:    time.Date(2026, 6, 1, 0, 1, 0, 0, loc),
			wantDue: time.Date(2026, 6, 1, 0, 15, 0, 0, loc),
		},
		{
			name:    "ranges in hours",
			expr:    "0 9-17 * * *",
			from:    time.Date(2026, 6, 1, 8, 30, 0, 0, loc),
			wantDue: time.Date(2026, 6, 1, 9, 0, 0, 0, loc),
		},
		{
			name:    "ranges in hours next step",
			expr:    "0 9-17 * * *",
			from:    time.Date(2026, 6, 1, 9, 0, 0, 0, loc),
			wantDue: time.Date(2026, 6, 1, 10, 0, 0, 0, loc),
		},
		{
			name:    "ranges with step",
			expr:    "0 9-17/2 * * *",
			from:    time.Date(2026, 6, 1, 9, 0, 0, 0, loc),
			wantDue: time.Date(2026, 6, 1, 11, 0, 0, 0, loc),
		},
		{
			name:    "minute step */5",
			expr:    "*/5 * * * *",
			from:    time.Date(2026, 6, 1, 0, 12, 0, 0, loc),
			wantDue: time.Date(2026, 6, 1, 0, 15, 0, 0, loc),
		},
		{
			name:    "minute step range 1-30/2",
			expr:    "1-30/2 * * * *",
			from:    time.Date(2026, 6, 1, 0, 3, 0, 0, loc),
			wantDue: time.Date(2026, 6, 1, 0, 5, 0, 0, loc),
		},
		{
			name:    "month names",
			expr:    "0 0 1 JAN,DEC *",
			from:    time.Date(2026, 6, 1, 0, 0, 0, 0, loc),
			wantDue: time.Date(2026, 12, 1, 0, 0, 0, 0, loc),
		},
		{
			name:    "day of week names MON-FRI",
			expr:    "0 9 * * MON-FRI",
			from:    time.Date(2026, 6, 5, 9, 0, 0, 0, loc), // Friday
			wantDue: time.Date(2026, 6, 8, 9, 0, 0, 0, loc), // Next Monday
		},
		// DOM/DOW OR semantics (spec D4)
		{
			name:    "dom restricted dow wildcard: only dom matches",
			expr:    "0 0 15 * *",
			from:    time.Date(2026, 6, 1, 0, 0, 0, 0, loc),
			wantDue: time.Date(2026, 6, 15, 0, 0, 0, 0, loc),
		},
		{
			name:    "dom wildcard dow restricted: only dow matches",
			expr:    "0 0 * * 1", // Mondays
			from:    time.Date(2026, 6, 1, 0, 0, 0, 0, loc),
			wantDue: time.Date(2026, 6, 8, 0, 0, 0, 0, loc),
		},
		{
			name:    "both dom and dow restricted: OR semantics (matches DOW first)",
			expr:    "0 0 15 * 5",                           // 15th of month OR Fridays
			from:    time.Date(2026, 6, 1, 0, 0, 0, 0, loc), // Mon June 1
			wantDue: time.Date(2026, 6, 5, 0, 0, 0, 0, loc), // Fri June 5 matches DOW
		},
		{
			name:    "both dom and dow restricted: OR semantics (matches DOM next)",
			expr:    "0 0 15 * 5",                            // 15th of month OR Fridays
			from:    time.Date(2026, 6, 12, 0, 0, 0, 0, loc), // Fri June 12
			wantDue: time.Date(2026, 6, 15, 0, 0, 0, 0, loc), // Mon June 15 matches DOM
		},
		// Error cases
		{
			name:     "too few fields",
			expr:     "* * * *",
			wantFail: true,
		},
		{
			name:     "too many fields",
			expr:     "* * * * * *",
			wantFail: true,
		},
		{
			name:     "invalid minute range",
			expr:     "60 * * * *",
			wantFail: true,
		},
		{
			name:     "invalid step 0",
			expr:     "*/0 * * * *",
			wantFail: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sched, err := ParseCron(tt.expr)
			if tt.wantFail {
				if err == nil {
					t.Fatalf("ParseCron(%q) expected error, got nil", tt.expr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCron(%q) unexpected error: %v", tt.expr, err)
			}

			res, err := sched.Next(tt.from, loc)
			if err != nil {
				t.Fatalf("Next() unexpected error: %v", err)
			}
			if !res.Due.Equal(tt.wantDue) {
				t.Errorf("Next() = %v, want %v", res.Due, tt.wantDue)
			}
		})
	}
}

func TestVPa_DSTSpringForwardSequence(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load location America/New_York: %v", err)
	}

	// 2026-03-08 is spring forward in America/New_York: 2:00 -> 3:00 EST->EDT.
	// Schedule: 30 2 * * * (02:30 every day)
	t0 := time.Date(2026, 3, 7, 2, 0, 0, 0, loc)
	clk := loop.NewFakeClock(t0)

	home := t.TempDir()
	cfg := DefaultConfig()
	cfg.LocalTimezone = "America/New_York"

	var firedCount int
	var firedTimes []time.Time
	runner := func(ctx context.Context, sessionID, prompt string) error {
		firedCount++
		firedTimes = append(firedTimes, clk.Now().In(loc))
		return nil
	}

	eng, err := New(cfg, home, WithClock(clk), WithRunner(runner), WithLocation(loc))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := eng.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "cron", Expr: "30 2 * * *"},
		Prompt:   "dst spring test",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// 1. First due: 2026-03-07 02:30:00 EST
	wantDue1 := time.Date(2026, 3, 7, 2, 30, 0, 0, loc)
	due1, _ := time.Parse(time.RFC3339, job.NextDue)
	if !due1.Equal(wantDue1) {
		t.Fatalf("initial due = %v, want %v", due1, wantDue1)
	}

	clk.Advance(30 * time.Minute) // Advance to 02:30 EST
	_ = eng.Sync(context.Background())
	// Job 1 fires
	if firedCount != 1 {
		t.Fatalf("expected 1 firing at %v, got %d", wantDue1, firedCount)
	}

	// Check next due: falls in 2026-03-08 spring-forward gap (02:30)!
	// Gap endpoint is 2026-03-08 03:00:00 EDT (07:00:00 UTC).
	wantGapDue := time.Date(2026, 3, 8, 3, 0, 0, 0, loc)
	j1, _ := eng.GetJob(context.Background(), job.ID)
	dueGap, _ := time.Parse(time.RFC3339, j1.NextDue)
	if !dueGap.Equal(wantGapDue) {
		t.Fatalf("next due across spring-forward gap = %v, want %v", dueGap, wantGapDue)
	}

	// 2. Advance to the gap time (03:00 EDT)
	clk.Advance(wantGapDue.Sub(clk.Now()))
	_ = eng.Sync(context.Background())
	// The due time falls in the gap: it must be skipped with dst_gap event and NOT fire turn (spec D4)
	if firedCount != 1 {
		t.Fatalf("firing count increased during dst_gap, want 1, got %d", firedCount)
	}

	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}

	var foundDstGap bool
	for _, ev := range events {
		if ev.Status == "skipped" && ev.Reason == "dst_gap" {
			foundDstGap = true
			break
		}
	}
	if !foundDstGap {
		t.Fatalf("expected journal to contain skipped reason: dst_gap event, got %+v", events)
	}

	// Check next due: advances to 2026-03-09 02:30:00 EDT
	wantDue3 := time.Date(2026, 3, 9, 2, 30, 0, 0, loc)
	j2, _ := eng.GetJob(context.Background(), job.ID)
	due3, _ := time.Parse(time.RFC3339, j2.NextDue)
	if !due3.Equal(wantDue3) {
		t.Fatalf("next due after gap = %v, want %v", due3, wantDue3)
	}

	// 3. Advance to 2026-03-09 02:30:00 EDT
	clk.Advance(wantDue3.Sub(clk.Now()))
	_ = eng.Sync(context.Background())
	if firedCount != 2 {
		t.Fatalf("expected 2nd firing at %v, got total %d", wantDue3, firedCount)
	}
}

func TestVPa_DSTFallBackSequence(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load location America/New_York: %v", err)
	}

	// 2026-11-01 is fall back in America/New_York: 2:00 EDT -> 1:00 EST.
	// Schedule: 30 1 * * * (01:30 every day)
	t0 := time.Date(2026, 10, 31, 1, 0, 0, 0, loc) // 01:00 EDT
	clk := loop.NewFakeClock(t0)

	home := t.TempDir()
	cfg := DefaultConfig()
	cfg.LocalTimezone = "America/New_York"

	var firedCount int
	runner := func(ctx context.Context, sessionID, prompt string) error {
		firedCount++
		return nil
	}

	eng, err := New(cfg, home, WithClock(clk), WithRunner(runner), WithLocation(loc))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := eng.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "cron", Expr: "30 1 * * *"},
		Prompt:   "dst fall back test",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// 1. Advance to 2026-10-31 01:30:00 EDT
	clk.Advance(30 * time.Minute)
	_ = eng.Sync(context.Background())
	if firedCount != 1 {
		t.Fatalf("expected firing 1, got %d", firedCount)
	}

	// Next due must be 2026-11-01 01:30:00 EDT (first occurrence in overlap)
	wantDue1 := time.Date(2026, 11, 1, 1, 30, 0, 0, loc)
	j1, _ := eng.GetJob(context.Background(), job.ID)
	due1, _ := time.Parse(time.RFC3339, j1.NextDue)
	if !due1.Equal(wantDue1) {
		t.Fatalf("next due on fall back day = %v, want %v", due1, wantDue1)
	}

	// 2. Advance to 2026-11-01 01:30:00 EDT
	clk.Advance(wantDue1.Sub(clk.Now()))
	_ = eng.Sync(context.Background())
	if firedCount != 2 {
		t.Fatalf("expected firing 2 at 01:30 EDT, got %d", firedCount)
	}

	// Monotonic next due: must NOT re-arm for 01:30 EST on the same day! (spec D4)
	// Next due must be 2026-11-02 01:30:00 EST.
	wantDue2 := time.Date(2026, 11, 2, 1, 30, 0, 0, loc)
	j2, _ := eng.GetJob(context.Background(), job.ID)
	due2, _ := time.Parse(time.RFC3339, j2.NextDue)
	if !due2.Equal(wantDue2) {
		t.Fatalf("next due after overlap firing = %v, want %v", due2, wantDue2)
	}

	// 3. Advance through the overlap period (1 hour later is 01:30 EST)
	clk.Advance(1 * time.Hour)
	_ = eng.Sync(context.Background())
	// Must NOT fire again during the overlap!
	if firedCount != 2 {
		t.Fatalf("repeated time in fall-back overlap fired, want 2 firings, got %d", firedCount)
	}

	// 4. Advance to 2026-11-02 01:30:00 EST
	clk.Advance(wantDue2.Sub(clk.Now()))
	_ = eng.Sync(context.Background())
	if firedCount != 3 {
		t.Fatalf("expected firing 3 on 2026-11-02, got %d", firedCount)
	}
}

func TestVPa_IntervalSchedules(t *testing.T) {
	tests := []struct {
		name     string
		every    string
		wantDur  time.Duration
		wantFail bool
	}{
		{name: "1 minute floor", every: "1m", wantDur: 1 * time.Minute},
		{name: "30 minutes standard", every: "30m", wantDur: 30 * time.Minute},
		{name: "1 hour standard", every: "1h", wantDur: 1 * time.Hour},
		{name: "24 hours", every: "24h", wantDur: 24 * time.Hour},
		{name: "7 days", every: "7d", wantDur: 7 * 24 * time.Hour},
		{name: "1 year ceiling", every: "8760h", wantDur: 8760 * time.Hour},
		{name: "below 1m floor", every: "30s", wantFail: true},
		{name: "above 1yr ceiling", every: "400d", wantFail: true},
		{name: "invalid duration format", every: "abc", wantFail: true},
		{name: "empty interval", every: "", wantFail: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Schedule{Kind: "interval", Every: tt.every}
			ps, err := ParseSchedule(s)
			if tt.wantFail {
				if err == nil {
					t.Fatalf("ParseSchedule(%+v) expected error, got nil", s)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSchedule(%+v) unexpected error: %v", s, err)
			}
			if ps.interval != tt.wantDur {
				t.Errorf("interval = %v, want %v", ps.interval, tt.wantDur)
			}
		})
	}
}

func TestVPa_StarFlagWithStep(t *testing.T) {
	loc := time.UTC

	// 1. */2 * * * 5 (DOM begins with *, so star=true per Vixie; DOW restricted to Friday)
	// Must fire Fridays only, NOT an OR union of even DOMs and Fridays!
	// June 1, 2026 is Monday. June 2 is Tuesday (even day). June 5 is Friday.
	cs1, err := ParseCron("0 0 */2 * 5")
	if err != nil {
		t.Fatalf("ParseCron(0 0 */2 * 5): %v", err)
	}
	if !cs1.domWildcard {
		t.Errorf("expected domWildcard=true for */2, got false")
	}
	if cs1.dowWildcard {
		t.Errorf("expected dowWildcard=false for 5, got true")
	}

	from1 := time.Date(2026, 6, 1, 0, 0, 0, 0, loc)
	res1, err := cs1.Next(from1, loc)
	if err != nil {
		t.Fatalf("cs1.Next: %v", err)
	}
	want1 := time.Date(2026, 6, 5, 0, 0, 0, 0, loc) // Friday, June 5 (NOT Tuesday, June 2)
	if !res1.Due.Equal(want1) {
		t.Errorf("cs1.Next = %v, want %v (Fridays only, not OR union)", res1.Due, want1)
	}

	// 2. * * 1-3/2 * * (range-with-step: base is 1-3, does not begin with *, so star=false)
	cs2, err := ParseCron("0 0 1-3/2 * *")
	if err != nil {
		t.Fatalf("ParseCron(0 0 1-3/2 * *): %v", err)
	}
	if cs2.domWildcard {
		t.Errorf("expected domWildcard=false for 1-3/2, got true")
	}
	if !cs2.dowWildcard {
		t.Errorf("expected dowWildcard=true for *, got false")
	}

	// When combined with a restricted DOW: 0 0 1-3/2 * 5
	// Both DOM and DOW are restricted -> OR union applies!
	cs3, err := ParseCron("0 0 1-3/2 * 5")
	if err != nil {
		t.Fatalf("ParseCron(0 0 1-3/2 * 5): %v", err)
	}
	if cs3.domWildcard || cs3.dowWildcard {
		t.Errorf("expected both domWildcard and dowWildcard false for 1-3/2 and 5")
	}
	from3 := time.Date(2026, 6, 3, 12, 0, 0, 0, loc)
	res3, err := cs3.Next(from3, loc)
	if err != nil {
		t.Fatalf("cs3.Next: %v", err)
	}
	want3 := time.Date(2026, 6, 5, 0, 0, 0, 0, loc) // Friday June 5 matches DOW
	if !res3.Due.Equal(want3) {
		t.Errorf("cs3.Next = %v, want %v", res3.Due, want3)
	}
}

type stepClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*stepTimer
}

type stepTimer struct {
	clock    *stepClock
	deadline time.Time
	fn       func()
	stopped  bool
}

func (t *stepTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped {
		return false
	}
	t.stopped = true
	return true
}

func newStepClock(initial time.Time) *stepClock {
	return &stepClock{now: initial.UTC()}
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) AfterFunc(d time.Duration, f func()) loop.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d < 0 {
		d = 0
	}
	t := &stepTimer{clock: c, deadline: c.now.Add(d), fn: f}
	c.timers = append(c.timers, t)
	return t
}

func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	target := c.now
	c.mu.Unlock()

	for {
		c.mu.Lock()
		var next *stepTimer
		for _, t := range c.timers {
			if t.stopped || t.deadline.After(target) {
				continue
			}
			if next == nil || t.deadline.Before(next.deadline) {
				next = t
			}
		}
		if next == nil {
			c.mu.Unlock()
			return
		}
		next.stopped = true
		c.now = next.deadline
		fn := next.fn
		c.mu.Unlock()

		fn()

		c.mu.Lock()
		if c.now.Before(target) {
			c.now = target
		}
		c.mu.Unlock()
	}
}

func (c *stepClock) StepBackward(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(-d)
}

func TestD4_ClockJumpLastFireFloor(t *testing.T) {
	t.Run("cron", func(t *testing.T) {
		home := t.TempDir()
		t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
		clk := newStepClock(t0)
		cfg := DefaultConfig()

		var firedJobs []string
		runner := func(ctx context.Context, sessionID, prompt string) error {
			firedJobs = append(firedJobs, prompt)
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

		// Add cron job (every 5m: due at 12:05)
		cronJob, err := eng.AddJob(context.Background(), Job{
			Schedule: Schedule{Kind: "cron", Expr: "*/5 * * * *"},
			Prompt:   "cron-clockjump",
		})
		if err != nil {
			t.Fatalf("AddJob cron: %v", err)
		}

		// Advance clock to 12:05: cron job fires!
		clk.Advance(5 * time.Minute)
		_ = eng.Sync(context.Background())

		if len(firedJobs) != 1 {
			t.Fatalf("expected 1 firing at 12:05, got %d: %v", len(firedJobs), firedJobs)
		}

		// Verify recorded LastFire at 12:05
		cj, _ := eng.GetJob(context.Background(), cronJob.ID)
		wantLastFire := time.Date(2026, 11, 2, 12, 5, 0, 0, time.UTC).Format(time.RFC3339)
		if cj.LastFire != wantLastFire {
			t.Fatalf("cron LastFire = %v, want %v", cj.LastFire, wantLastFire)
		}

		// Now simulate NTP backward clock step: clock steps back by 3 minutes to 12:02!
		clk.StepBackward(3 * time.Minute)

		// Spec D4: "a backward NTP step cannot re-arm a due time already fired (last_fire is the floor)"
		// Computed next-due must never be <= last_fire (12:05).
		cronRes, err := eng.calculateNextDue(&cj, clk.Now())
		if err != nil {
			t.Fatalf("calculateNextDue cron after backward step: %v", err)
		}
		wantNextDue := time.Date(2026, 11, 2, 12, 10, 0, 0, time.UTC)
		if !cronRes.Due.Equal(wantNextDue) {
			t.Errorf("cron next due after backward step = %v, want %v (last_fire is the floor)", cronRes.Due, wantNextDue)
		}

		// Advance clock through the stepped-back window to 12:05 (where the old due was)
		clk.Advance(3 * time.Minute) // clock is back at 12:05
		_ = eng.Sync(context.Background())

		// No duplicate firing at 12:05!
		if len(firedJobs) != 1 {
			t.Fatalf("backward clock step re-armed already fired due: firings = %d, want 1", len(firedJobs))
		}

		// Advance to 12:10: next scheduled firing occurs
		clk.Advance(5 * time.Minute)
		_ = eng.Sync(context.Background())

		if len(firedJobs) != 2 {
			t.Fatalf("expected 2 total firings at 12:10, got %d", len(firedJobs))
		}
	})

	t.Run("interval", func(t *testing.T) {
		home := t.TempDir()
		t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
		clk := newStepClock(t0)
		cfg := DefaultConfig()

		var firedJobs []string
		runner := func(ctx context.Context, sessionID, prompt string) error {
			firedJobs = append(firedJobs, prompt)
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

		// Add interval job (every 5m: due at 12:05)
		intervalJob, err := eng.AddJob(context.Background(), Job{
			Schedule: Schedule{Kind: "interval", Every: "5m"},
			Prompt:   "interval-clockjump",
		})
		if err != nil {
			t.Fatalf("AddJob interval: %v", err)
		}

		// Advance clock to 12:05: interval job fires!
		clk.Advance(5 * time.Minute)
		_ = eng.Sync(context.Background())

		if len(firedJobs) != 1 {
			t.Fatalf("expected 1 firing at 12:05, got %d: %v", len(firedJobs), firedJobs)
		}

		// Verify recorded LastFire at 12:05
		ij, _ := eng.GetJob(context.Background(), intervalJob.ID)
		wantLastFire := time.Date(2026, 11, 2, 12, 5, 0, 0, time.UTC).Format(time.RFC3339)
		if ij.LastFire != wantLastFire {
			t.Fatalf("interval LastFire = %v, want %v", ij.LastFire, wantLastFire)
		}

		// Now simulate NTP backward clock step: clock steps back by 3 minutes to 12:02!
		clk.StepBackward(3 * time.Minute)

		// Spec D4: "a backward NTP step cannot re-arm a due time already fired (last_fire is the floor)"
		// Computed next-due must never be <= last_fire (12:05).
		intervalRes, err := eng.calculateNextDue(&ij, clk.Now())
		if err != nil {
			t.Fatalf("calculateNextDue interval after backward step: %v", err)
		}
		wantNextDue := time.Date(2026, 11, 2, 12, 10, 0, 0, time.UTC)
		if !intervalRes.Due.Equal(wantNextDue) {
			t.Errorf("interval next due after backward step = %v, want %v (anchored from last_fire)", intervalRes.Due, wantNextDue)
		}

		// Advance clock through the stepped-back window to 12:05 (where the old due was)
		clk.Advance(3 * time.Minute) // clock is back at 12:05
		_ = eng.Sync(context.Background())

		// No duplicate firing at 12:05!
		if len(firedJobs) != 1 {
			t.Fatalf("backward clock step re-armed already fired due: firings = %d, want 1", len(firedJobs))
		}

		// Advance to 12:10: next scheduled firing occurs
		clk.Advance(5 * time.Minute)
		_ = eng.Sync(context.Background())

		if len(firedJobs) != 2 {
			t.Fatalf("expected 2 total firings at 12:10, got %d", len(firedJobs))
		}
	})
}

func TestVPa_StepAcrossGap(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load location America/New_York: %v", err)
	}

	// 2026-03-08 is spring forward in America/New_York: 2:00 -> 3:00 EST->EDT.
	// Schedule: */5 * * * *
	// Initial time: 2026-03-08 01:55:00 EST
	t0 := time.Date(2026, 3, 8, 1, 55, 0, 0, loc)
	clk := loop.NewFakeClock(t0)

	home := t.TempDir()
	cfg := DefaultConfig()
	cfg.LocalTimezone = "America/New_York"

	var firedCount int
	var firedTimes []time.Time
	runner := func(ctx context.Context, sessionID, prompt string) error {
		firedCount++
		firedTimes = append(firedTimes, clk.Now().In(loc))
		return nil
	}

	eng, err := New(cfg, home, WithClock(clk), WithRunner(runner), WithLocation(loc))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	job, err := eng.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "cron", Expr: "*/5 * * * *"},
		Prompt:   "step across gap test",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Next due across the gap lands at gapEnd = 2026-03-08 03:00:00 EDT
	wantGapEnd := time.Date(2026, 3, 8, 3, 0, 0, 0, loc)
	j1, err := eng.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	dueGap, err := time.Parse(time.RFC3339, j1.NextDue)
	if err != nil {
		t.Fatalf("parse next_due: %v", err)
	}
	if !dueGap.Equal(wantGapEnd) {
		t.Fatalf("initial due across gap = %v, want %v", dueGap, wantGapEnd)
	}

	// Advance clock to 03:00:00 EDT (5 minutes of elapsed time from 01:55 EST)
	clk.Advance(wantGapEnd.Sub(clk.Now()))
	_ = eng.Sync(context.Background())

	// 1. Skip event must be logged naming the skipped wall due 02:00 (Spec B3)
	// One-event-per-armed-due rule collapses gap dues to this skip event.
	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}

	var skipEvent *JournalEvent
	for _, ev := range events {
		if ev.Status == "skipped" && ev.Reason == "dst_gap" && ev.JobID == job.ID {
			skipEvent = &ev
			break
		}
	}
	if skipEvent == nil {
		t.Fatalf("expected skip event with dst_gap, got events: %+v", events)
	}
	// Skip event must name the wall-clock due 02:00 (Spec B3)
	if !strings.Contains(skipEvent.DueAt, "02:00") {
		t.Errorf("skip event due_at = %q, expected it to name 02:00 wall clock due", skipEvent.DueAt)
	}

	// 2. A real firing must happen at 03:00 EDT sharp (Spec B3: gap-end occurrence not swallowed)
	if firedCount != 1 {
		t.Fatalf("expected 1 real firing at 03:00 EDT, got %d", firedCount)
	}
	if !firedTimes[0].Equal(wantGapEnd) {
		t.Errorf("firing time = %v, want %v (03:00 EDT sharp)", firedTimes[0], wantGapEnd)
	}

	// 3. Next due after 03:00 EDT firing must be 03:05 EDT
	j2, err := eng.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetJob after 03:00: %v", err)
	}
	wantNextDue := time.Date(2026, 3, 8, 3, 5, 0, 0, loc)
	dueNext, err := time.Parse(time.RFC3339, j2.NextDue)
	if err != nil {
		t.Fatalf("parse next_due: %v", err)
	}
	if !dueNext.Equal(wantNextDue) {
		t.Errorf("next due after 03:00 firing = %v, want %v", dueNext, wantNextDue)
	}

	// Advance to 03:05 EDT: fires normally
	clk.Advance(5 * time.Minute)
	_ = eng.Sync(context.Background())
	if firedCount != 2 {
		t.Fatalf("expected 2nd firing at 03:05 EDT, got %d", firedCount)
	}
}

func TestVPa_NoTimeNowInPackage(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob *.go: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no .go files found in internal/fasti")
	}

	forbidden := "time" + ".Now"
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if strings.Contains(string(data), forbidden) {
			t.Errorf("file %s contains forbidden %s (R1 violation)", file, forbidden)
		}
	}
}
