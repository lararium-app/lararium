package fasti

// F4a reviewer-fix law tests for the atomic EditJob verb (spec D8 PATCH,
// D9 adoption law, D5 created_by row law):
//   - prompt edits adopt created_by = user:<surface> + fasti_job_adopted audit
//   - live jobs (enabled, not paused) refuse prompt edits
//   - a prompt-only edit never moves next_due and never duplicates the row
//   - a schedule edit recomputes next_due and preserves id and pause state
//   - replay of an identical edit is a no-op; unknown ids map to ErrJobNotFound

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

func startEditEngine(t *testing.T) (*Engine, *loop.FakeClock) {
	t.Helper()
	home := t.TempDir()
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)
	eng, err := New(DefaultConfig(), home, WithClock(clk))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { eng.Stop() })
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return eng, clk
}

func addLawJob(t *testing.T, eng *Engine, id, prompt string) Job {
	t.Helper()
	job, err := eng.AddJob(context.Background(), Job{
		ID:       id,
		Name:     "law job",
		Schedule: Schedule{Kind: "interval", Every: "1h"},
		Prompt:   prompt,
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	return job
}

func TestEditJobAdoptionLaw(t *testing.T) {
	eng, clk := startEditEngine(t)
	ctx := context.Background()
	created := addLawJob(t, eng, "j_adopt00000000000000000001", "original text")

	// D9: the edit path is pause -> edit -> resume.
	if err := eng.PauseJob(ctx, created.ID); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}

	updated, err := eng.EditJob(ctx, created.ID, strPtr("revised text"), nil)
	if err != nil {
		t.Fatalf("EditJob: %v", err)
	}
	if updated.Prompt != "revised text" {
		t.Errorf("prompt = %q, want %q", updated.Prompt, "revised text")
	}
	// Adoption law: created_by becomes user:<surface> (D5/D9).
	if updated.CreatedBy != "user:rest" {
		t.Errorf("CreatedBy = %q, want %q (D9 adoption law)", updated.CreatedBy, "user:rest")
	}
	// Identity + history laws: same id, same single row, next_due untouched
	// by a prompt-only edit.
	if updated.ID != created.ID {
		t.Errorf("id changed: %q -> %q", created.ID, updated.ID)
	}
	if updated.NextDue != created.NextDue {
		t.Errorf("prompt-only edit moved next_due: %q -> %q", created.NextDue, updated.NextDue)
	}
	jobs, err := eng.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("job count = %d, want 1 (edit must not duplicate rows)", len(jobs))
	}
	// Pause state survives the edit.
	if !updated.Paused {
		t.Errorf("Paused lost through edit")
	}

	// Audit: fasti_job_adopted with the job id (D5 audit list).
	events, err := eng.Journal().Events()
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	var adopted bool
	for _, ev := range events {
		if ev.Type == "fasti_job_adopted" && ev.JobID == created.ID {
			adopted = true
			break
		}
	}
	if !adopted {
		t.Fatalf("expected fasti_job_adopted audit event, got: %+v", events)
	}
	_ = clk
}

func TestEditJobPromptWhileLiveRefused(t *testing.T) {
	eng, _ := startEditEngine(t)
	ctx := context.Background()
	created := addLawJob(t, eng, "j_live0000000000000000000001", "keep me auditable")

	// D9: a cron prompt is immutable while enabled — user tier must pause first.
	if _, err := eng.EditJob(ctx, created.ID, strPtr("sneaky rewrite"), nil); !errors.Is(err, ErrPromptWhileEnabled) {
		t.Fatalf("EditJob while enabled: err = %v, want ErrPromptWhileEnabled", err)
	}
	got, err := eng.GetJob(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Prompt != "keep me auditable" {
		t.Errorf("refused edit still changed prompt: %q", got.Prompt)
	}
}

func TestEditJobScheduleRecomputesNextDue(t *testing.T) {
	eng, clk := startEditEngine(t)
	ctx := context.Background()
	created := addLawJob(t, eng, "j_sched000000000000000000001", "tick")

	clk.Advance(20 * time.Minute)
	newSched := Schedule{Kind: "interval", Every: "2h"}
	updated, err := eng.EditJob(ctx, created.ID, nil, &newSched)
	if err != nil {
		t.Fatalf("EditJob schedule: %v", err)
	}
	wantDue := clk.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	if updated.NextDue != wantDue {
		t.Errorf("next_due = %q, want %q (schedule edit recomputes)", updated.NextDue, wantDue)
	}
	// A schedule-only edit does NOT adopt (no prompt authorship change).
	if updated.CreatedBy != created.CreatedBy {
		t.Errorf("CreatedBy = %q after schedule-only edit, want %q", updated.CreatedBy, created.CreatedBy)
	}

	// Prompt edit while enabled-but-paused still works through the pause.
	if err := eng.PauseJob(ctx, created.ID); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	if _, err := eng.EditJob(ctx, created.ID, strPtr("paused edit ok"), nil); err != nil {
		t.Fatalf("EditJob prompt on paused job: %v", err)
	}
}

func TestEditJobNoopAndMissing(t *testing.T) {
	eng, _ := startEditEngine(t)
	ctx := context.Background()
	created := addLawJob(t, eng, "j_noop0000000000000000000001", "noop target")

	// Replay idempotence (D8): an edit with no fields returns the same row.
	same, err := eng.EditJob(ctx, created.ID, nil, nil)
	if err != nil {
		t.Fatalf("no-op EditJob: %v", err)
	}
	if same.NextDue != created.NextDue || same.Prompt != created.Prompt {
		t.Errorf("no-op edit changed the row: %+v vs %+v", same, created)
	}

	if _, err := eng.EditJob(ctx, "j_absent00000000000000000000", strPtr("x"), nil); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("EditJob missing: err = %v, want ErrJobNotFound", err)
	}
}

func strPtr(s string) *string { return &s }
