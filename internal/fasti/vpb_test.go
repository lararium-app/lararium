package fasti

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

func TestVPb_RestartSurvival(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()

	eng1, err := New(cfg, home, WithClock(clk))
	if err != nil {
		t.Fatalf("New eng1: %v", err)
	}

	if err := eng1.Start(context.Background()); err != nil {
		t.Fatalf("Start eng1: %v", err)
	}

	job := Job{
		ID:               "j_test1234567890123456789012",
		Name:             "nightly-sync",
		Schedule:         Schedule{Kind: "cron", Expr: "0 2 * * *"},
		Prompt:           "run nightly data sync",
		Session:          "cron-custom-sess",
		Announce:         "ops",
		Enabled:          true,
		Paused:           false,
		CreatedBy:        "user",
		ConsecutiveSkips: 2,
		BudgetCounters: BudgetCounters{
			TotalRuns:     10,
			TotalSkips:    2,
			TotalDefers:   1,
			TotalFailures: 0,
		},
	}

	created, err := eng1.AddJob(context.Background(), job)
	if err != nil {
		t.Fatalf("AddJob eng1: %v", err)
	}

	eng1.Stop()

	// Simulate restart: boot second engine instance pointing to the same home
	eng2, err := New(cfg, home, WithClock(clk))
	if err != nil {
		t.Fatalf("New eng2: %v", err)
	}
	defer eng2.Stop()

	if err := eng2.Start(context.Background()); err != nil {
		t.Fatalf("Start eng2: %v", err)
	}

	loaded, err := eng2.GetJob(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetJob eng2: %v", err)
	}

	if loaded.ID != created.ID {
		t.Errorf("ID = %q, want %q", loaded.ID, created.ID)
	}
	if loaded.Name != created.Name {
		t.Errorf("Name = %q, want %q", loaded.Name, created.Name)
	}
	if loaded.Schedule != created.Schedule {
		t.Errorf("Schedule = %+v, want %+v", loaded.Schedule, created.Schedule)
	}
	if loaded.Prompt != created.Prompt {
		t.Errorf("Prompt = %q, want %q", loaded.Prompt, created.Prompt)
	}
	if loaded.Session != created.Session {
		t.Errorf("Session = %q, want %q", loaded.Session, created.Session)
	}
	if loaded.Announce != created.Announce {
		t.Errorf("Announce = %q, want %q", loaded.Announce, created.Announce)
	}
	if loaded.Enabled != created.Enabled {
		t.Errorf("Enabled = %v, want %v", loaded.Enabled, created.Enabled)
	}
	if loaded.Paused != created.Paused {
		t.Errorf("Paused = %v, want %v", loaded.Paused, created.Paused)
	}
	if loaded.CreatedBy != created.CreatedBy {
		t.Errorf("CreatedBy = %q, want %q", loaded.CreatedBy, created.CreatedBy)
	}
	if loaded.NextDue != created.NextDue {
		t.Errorf("NextDue = %q, want %q", loaded.NextDue, created.NextDue)
	}
	if loaded.ConsecutiveSkips != created.ConsecutiveSkips {
		t.Errorf("ConsecutiveSkips = %d, want %d", loaded.ConsecutiveSkips, created.ConsecutiveSkips)
	}
	if loaded.BudgetCounters != created.BudgetCounters {
		t.Errorf("BudgetCounters = %+v, want %+v", loaded.BudgetCounters, created.BudgetCounters)
	}
}

func TestVPb_ExactlyOnceSlippedFiring(t *testing.T) {
	home := t.TempDir()

	// Initial time: 10:00 UTC
	t0 := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	cfg := DefaultConfig()
	cfg.CatchupWindow = 24 * time.Hour

	var firedCount int
	var firedPrompts []string
	runner := func(ctx context.Context, sessionID, prompt string) error {
		firedCount++
		firedPrompts = append(firedPrompts, prompt)
		return nil
	}

	eng1, err := New(cfg, home, WithClock(clk), WithRunner(runner))
	if err != nil {
		t.Fatalf("New eng1: %v", err)
	}
	if err := eng1.Start(context.Background()); err != nil {
		t.Fatalf("Start eng1: %v", err)
	}

	// Job runs every 30m. First due: 10:30 UTC.
	job, err := eng1.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "interval", Every: "30m"},
		Prompt:   "frequent sync prompt",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Daemon goes offline at 10:05 UTC.
	eng1.Stop()

	// Time advances 6 hours while offline (from 10:05 to 16:05 UTC).
	// During this 6-hour offline outage, ~12 scheduled due times slipped!
	offlineAdvance := 6*time.Hour + 5*time.Minute
	clk.Advance(offlineAdvance) // clk is now 16:05 UTC

	// Restart daemon: engine boots at 16:05 UTC.
	eng2, err := New(cfg, home, WithClock(clk), WithRunner(runner))
	if err != nil {
		t.Fatalf("New eng2: %v", err)
	}
	defer eng2.Stop()

	if err := eng2.Start(context.Background()); err != nil {
		t.Fatalf("Start eng2: %v", err)
	}
	_ = eng2.Sync(context.Background())

	// Verify downtime collapse law (spec D7, V-Pb):
	// Slipped dues collapse to EXACTLY ONE replay firing!
	if firedCount != 1 {
		t.Fatalf("offline catchup fired %d times, want EXACTLY 1", firedCount)
	}

	events, err := eng2.Journal().Events()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}

	var foundSlippedEvent bool
	for _, ev := range events {
		if ev.Status == "fired" && ev.Reason == "slipped_while_offline" && ev.JobID == job.ID {
			foundSlippedEvent = true
			break
		}
	}
	if !foundSlippedEvent {
		t.Fatalf("expected slipped_while_offline journal event, got events: %+v", events)
	}

	// Verify future due time is preserved and in the future (> 16:05 UTC)
	j2, err := eng2.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetJob eng2: %v", err)
	}

	due2, err := time.Parse(time.RFC3339, j2.NextDue)
	if err != nil {
		t.Fatalf("parse next_due %q: %v", j2.NextDue, err)
	}

	now := clk.Now().UTC()
	if !due2.After(now) {
		t.Fatalf("future due %v should be after current time %v", due2, now)
	}

	// Advancing slightly should NOT fire any remaining slipped past dues
	clk.Advance(5 * time.Minute)
	if firedCount != 1 {
		t.Fatalf("unexpected firing count %d after slight advance", firedCount)
	}

	// Advancing to the next future due fires normally
	clk.Advance(due2.Sub(clk.Now().UTC()))
	_ = eng2.Sync(context.Background())
	if firedCount != 2 {
		t.Fatalf("expected 2nd firing at future due, got %d", firedCount)
	}
}

func TestVPb_KillMidWriteStoreParseability(t *testing.T) {
	home := t.TempDir()
	fastiDir := filepath.Join(home, "fasti")
	if err := os.MkdirAll(fastiDir, 0o700); err != nil {
		t.Fatalf("mkdir fasti: %v", err)
	}

	// Valid jobs.json file
	jobsPath := filepath.Join(fastiDir, "jobs.json")
	validJobs := []Job{
		{
			ID:       "j_valid001",
			Prompt:   "valid job 1",
			Schedule: Schedule{Kind: "interval", Every: "1h"},
			Enabled:  true,
		},
		{
			ID:       "j_valid002",
			Prompt:   "valid job 2",
			Schedule: Schedule{Kind: "cron", Expr: "0 12 * * *"},
			Enabled:  true,
		},
	}
	if err := SaveJobs(jobsPath, validJobs); err != nil {
		t.Fatalf("SaveJobs: %v", err)
	}

	// Simulate crashed writer: leave a partially written temp file
	crashedTemp := filepath.Join(fastiDir, "jobs-crash9999.tmp")
	if err := os.WriteFile(crashedTemp, []byte(`{"id": "j_corrupt`), 0o600); err != nil {
		t.Fatalf("write crash temp: %v", err)
	}

	// LoadJobs must ignore or not be affected by the abandoned temp file
	loaded, err := LoadJobs(jobsPath)
	if err != nil {
		t.Fatalf("LoadJobs failed after crash: %v", err)
	}

	if len(loaded) != 2 {
		t.Fatalf("loaded %d jobs, want 2", len(loaded))
	}
	if loaded[0].ID != "j_valid001" || loaded[1].ID != "j_valid002" {
		t.Errorf("loaded jobs mismatched: %+v", loaded)
	}

	// An engine booted on this directory must load successfully
	t0 := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)
	cfg := DefaultConfig()

	eng, err := New(cfg, home, WithClock(clk))
	if err != nil {
		t.Fatalf("New engine with crashed temp file present: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start engine: %v", err)
	}

	jobs, err := eng.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs failed: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("engine listed %d jobs, want 2", len(jobs))
	}
}

func TestAddJob_CreatedByValidation(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)
	cfg := DefaultConfig()

	eng, err := New(cfg, home, WithClock(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer eng.Stop()

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// 1. Rejected created_by values (Spec D5 / M9)
	badValues := []string{"user:web", "admin", "system", "other"}
	for _, val := range badValues {
		_, err := eng.AddJob(context.Background(), Job{
			Schedule:  Schedule{Kind: "interval", Every: "1h"},
			Prompt:    "test created_by prompt",
			CreatedBy: val,
		})
		if err == nil {
			t.Errorf("AddJob with CreatedBy=%q expected error, got nil", val)
		}
	}

	// 2. Allowed created_by values: "", "user", "model"
	goodValues := []string{"", "user", "model"}
	for _, val := range goodValues {
		j, err := eng.AddJob(context.Background(), Job{
			Schedule:  Schedule{Kind: "interval", Every: "1h"},
			Prompt:    "test created_by prompt",
			CreatedBy: val,
		})
		if err != nil {
			t.Errorf("AddJob with CreatedBy=%q unexpected error: %v", val, err)
		}
		expected := val
		if expected == "" {
			expected = "user"
		}
		if j.CreatedBy != expected {
			t.Errorf("AddJob with CreatedBy=%q resulted in %q, want %q", val, j.CreatedBy, expected)
		}
	}
}
