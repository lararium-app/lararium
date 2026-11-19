package fasti

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

var errOverflow = errors.New("all chain targets failed: [p(m): HTTP 400: request (65959 tokens) exceeds the available context size (32768 tokens)]")

func TestContextOverflow_WarningAfterThreeConsecutiveFailures(t *testing.T) {
	home := t.TempDir()
	clk := loop.NewFakeClock(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	fail := true
	runner := func(ctx context.Context, sessionID, prompt string) error {
		if fail {
			return errOverflow
		}
		return nil
	}
	eng, err := New(DefaultConfig(), home, WithClock(clk), WithRunner(runner))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Stop()
	if err := eng.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, err := eng.AddJob(context.Background(), Job{
		Schedule: Schedule{Kind: "interval", Every: "10m"},
		Prompt:   "tick",
	})
	if err != nil {
		t.Fatal(err)
	}
	get := func() Job {
		t.Helper()
		_ = eng.Sync(context.Background())
		j, err := eng.GetJob(context.Background(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	for i := 1; i <= 3; i++ {
		clk.Advance(10 * time.Minute)
		j := get()
		if i < 3 && j.Warning != "" {
			t.Fatalf("warning set after %d failures: %q", i, j.Warning)
		}
		if i == 3 {
			if j.ConsecutiveContextFailures != 3 || j.Warning == "" {
				t.Fatalf("after 3 failures: count=%d warning=%q", j.ConsecutiveContextFailures, j.Warning)
			}
		}
	}
	// Success clears the warning.
	fail = false
	clk.Advance(10 * time.Minute)
	if j := get(); j.Warning != "" || j.ConsecutiveContextFailures != 0 {
		t.Fatalf("warning not cleared on success: %+v", j)
	}
}
