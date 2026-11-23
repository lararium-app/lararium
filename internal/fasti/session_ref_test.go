package fasti

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

func TestValidateSessionRef(t *testing.T) {
	for _, ok := range []string{"main", "s_01ARZ3NDEKTSV4RRFFQ69G5FAV", "cron-j_abc", "goal-g1", "ops-nightly"} {
		if err := ValidateSessionRef(ok); err != nil {
			t.Errorf("ValidateSessionRef(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "..", "../x", "a/b", "/abs", ".hidden", "-x", "a\x00b", strings.Repeat("a", 129)} {
		if err := ValidateSessionRef(bad); err == nil {
			t.Errorf("ValidateSessionRef(%q) = nil, want error", bad)
		}
	}
}

// The error echoes caller input; a very long value must not be echoed whole.
func TestValidateSessionRefErrorIsBounded(t *testing.T) {
	err := ValidateSessionRef(strings.Repeat("x/", 5000))
	if err == nil {
		t.Fatal("want error")
	}
	if len(err.Error()) > 300 {
		t.Fatalf("error echoes unbounded caller input (%d bytes)", len(err.Error()))
	}
}

func TestJobValidateRejectsEscapingSession(t *testing.T) {
	base := func() Job {
		return Job{
			ID:       "j_1",
			Name:     "n",
			Schedule: Schedule{Kind: "cron", Expr: "0 2 * * *"},
			Prompt:   "p",
			Enabled:  true,
		}
	}
	good := base()
	if err := good.Validate(); err != nil {
		t.Fatalf("valid job rejected: %v", err)
	}
	if good.Session != "cron-j_1" {
		t.Fatalf("default session = %q, want cron-j_1", good.Session)
	}
	for _, sess := range []string{"..", "../../x", "a/b", ".hidden"} {
		j := base()
		j.Session = sess
		if err := j.Validate(); err == nil {
			t.Errorf("session %q accepted", sess)
		}
	}
	// A caller-chosen job id that would poison the derived default session.
	j := base()
	j.ID = "../../x"
	if err := j.Validate(); err == nil {
		t.Error("job id that poisons the default session accepted")
	}
}

// A row already on disk with a hostile session must not break boot; the
// sink guard (surface.RunHeadlessTurn) is what stops it from firing.
func TestBootWithLegacyEscapingSessionRow(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "fasti"), 0o700); err != nil {
		t.Fatal(err)
	}
	rows := `[{"id":"j_legacy","name":"legacy","schedule":{"kind":"cron","expr":"0 2 * * *"},"prompt":"p","session":"../../escape","enabled":true,"created_at":"2026-01-01T00:00:00Z"}]`
	if err := os.WriteFile(JobsFilePath(home), []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := loop.NewFakeClock(time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC))
	eng, err := New(DefaultConfig(), home, WithClock(clk), WithRunner(func(context.Context, string, string) error { return nil }))
	if err != nil {
		t.Fatalf("New with legacy row: %v", err)
	}
	defer eng.Stop()
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start with legacy row: %v", err)
	}
	jobs, err := eng.ListJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Session != "../../escape" {
		t.Fatalf("legacy row not loaded as-is: %+v", jobs)
	}
}
