package main_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/fasti"
	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/surface"
)

type cliRealClock struct{}

func (cliRealClock) Now() time.Time {
	return time.Now()
}

func (cliRealClock) AfterFunc(d time.Duration, f func()) loop.Timer {
	t := time.AfterFunc(d, f)
	return &cliRealTimer{t: t}
}

type cliRealTimer struct {
	t *time.Timer
}

func (cr *cliRealTimer) Stop() bool {
	return cr.t.Stop()
}

func (cr *cliRealTimer) Reset(d time.Duration) bool {
	return cr.t.Reset(d)
}

func setupDaemonHarness(t *testing.T) (string, string, *fasti.Engine) {
	t.Helper()
	cfgPath, home := makeThrowawayHearth(t)
	sockPath := filepath.Join(home, "hearthd.sock")

	_ = os.WriteFile(filepath.Join(home, "HEARTBEAT.md"), []byte("---\ncadence: daily\n---\n- check\n"), 0o600)

	cfg := fasti.DefaultConfig()
	cfg.Enabled = true
	dummyRunner := func(ctx context.Context, sessionID, prompt string) error { return nil }
	eng, err := fasti.New(cfg, home, fasti.WithClock(cliRealClock{}), fasti.WithRunner(dummyRunner))
	if err != nil {
		t.Fatalf("fasti.New: %v", err)
	}
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("eng.Start: %v", err)
	}

	srv := &surface.Server{Fasti: eng}
	closer, err := srv.ServeSocket(sockPath, func() error { return nil })
	if err != nil {
		t.Fatalf("ServeSocket: %v", err)
	}

	t.Cleanup(func() {
		_ = closer.Close()
		eng.Stop()
	})

	return cfgPath, sockPath, eng
}

func TestCLI_JobAndGoalExecFlow(t *testing.T) {
	bin := buildHearthdCLI(t)
	cfgPath, _, _ := setupDaemonHarness(t)

	var jobID string
	var goalID string

	// 1. hearthd job add
	t.Run("job add", func(t *testing.T) {
		stdout, stderr, code := runHearthdArgs(t, bin, "-config", cfgPath, "job", "add", "--name", "daily-sync", "--every", "15m", "--prompt", "sync items")
		if code != 0 {
			t.Fatalf("job add failed: code=%d, stderr=%q, stdout=%q", code, stderr, stdout)
		}
		if !strings.Contains(stdout, "enabled") {
			t.Fatalf("expected stdout to contain 'enabled', got: %q", stdout)
		}
		fields := strings.Fields(stdout)
		if len(fields) < 1 {
			t.Fatalf("expected job id in stdout, got: %q", stdout)
		}
		jobID = fields[0]
		if !strings.HasPrefix(jobID, "j_") {
			t.Fatalf("expected job id prefix j_, got: %q", jobID)
		}
	})

	// 2. hearthd job list (human table and json)
	t.Run("job list", func(t *testing.T) {
		stdout, stderr, code := runHearthdArgs(t, bin, "-config", cfgPath, "job", "list")
		if code != 0 {
			t.Fatalf("job list failed: code=%d, stderr=%q", code, stderr)
		}
		if !strings.Contains(stdout, "ID") || !strings.Contains(stdout, "NAME") || !strings.Contains(stdout, "daily-sync") {
			t.Fatalf("unexpected table output: %q", stdout)
		}

		stdoutJSON, stderrJSON, codeJSON := runHearthdArgs(t, bin, "-config", cfgPath, "job", "list", "--json")
		if codeJSON != 0 {
			t.Fatalf("job list --json failed: code=%d, stderr=%q", codeJSON, stderrJSON)
		}
		var summary surface.JobSummary
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdoutJSON)), &summary); err != nil {
			t.Fatalf("unmarshal json line: %v, raw=%q", err, stdoutJSON)
		}
		if summary.ID != jobID || summary.Name != "daily-sync" || !summary.Enabled {
			t.Fatalf("unexpected summary: %+v", summary)
		}
	})

	// 3. hearthd job pause
	t.Run("job pause", func(t *testing.T) {
		stdout, stderr, code := runHearthdArgs(t, bin, "-config", cfgPath, "job", "pause", jobID)
		if code != 0 {
			t.Fatalf("job pause failed: code=%d, stderr=%q", code, stderr)
		}
		if !strings.Contains(stdout, jobID) {
			t.Fatalf("expected job id in pause output: %q", stdout)
		}

		// Verify list now shows enabled=false
		stdoutJSON, _, _ := runHearthdArgs(t, bin, "-config", cfgPath, "job", "list", "--json")
		var summary surface.JobSummary
		_ = json.Unmarshal([]byte(strings.TrimSpace(stdoutJSON)), &summary)
		if summary.Enabled {
			t.Fatalf("expected job to be disabled/paused, got enabled: true")
		}
	})

	// 4. hearthd job edit
	t.Run("job edit", func(t *testing.T) {
		stdout, stderr, code := runHearthdArgs(t, bin, "-config", cfgPath, "job", "edit", jobID, "--prompt", "updated task prompt")
		if code != 0 {
			t.Fatalf("job edit failed: code=%d, stderr=%q", code, stderr)
		}
		if !strings.Contains(stdout, jobID) {
			t.Fatalf("expected job id in edit output: %q", stdout)
		}
	})

	// 5. hearthd job resume
	t.Run("job resume", func(t *testing.T) {
		stdout, stderr, code := runHearthdArgs(t, bin, "-config", cfgPath, "job", "resume", jobID)
		if code != 0 {
			t.Fatalf("job resume failed: code=%d, stderr=%q", code, stderr)
		}
		if !strings.Contains(stdout, jobID) {
			t.Fatalf("expected job id in resume output: %q", stdout)
		}
	})

	// 6. hearthd job run
	t.Run("job run", func(t *testing.T) {
		stdout, stderr, code := runHearthdArgs(t, bin, "-config", cfgPath, "job", "run", jobID)
		if code != 0 {
			t.Fatalf("job run failed: code=%d, stderr=%q", code, stderr)
		}
		if !strings.Contains(stdout, jobID) {
			t.Fatalf("expected job id in run output: %q", stdout)
		}
	})

	// 7. hearthd job remove
	t.Run("job remove", func(t *testing.T) {
		stdout, stderr, code := runHearthdArgs(t, bin, "-config", cfgPath, "job", "remove", jobID)
		if code != 0 {
			t.Fatalf("job remove failed: code=%d, stderr=%q", code, stderr)
		}
		if !strings.Contains(stdout, jobID) {
			t.Fatalf("expected job id in remove output: %q", stdout)
		}
	})

	// 8. hearthd goal new
	t.Run("goal new", func(t *testing.T) {
		stdout, stderr, code := runHearthdArgs(t, bin, "-config", cfgPath, "goal", "new", "Ship Project", "--objective", "write tests", "--objective", "run linter", "--turns", "30")
		if code != 0 {
			t.Fatalf("goal new failed: code=%d, stderr=%q", code, stderr)
		}
		goalID = strings.TrimSpace(stdout)
		if !strings.HasPrefix(goalID, "j_") {
			t.Fatalf("expected goal id prefix j_, got: %q", goalID)
		}
	})

	// 9. hearthd goal list (human table and json)
	t.Run("goal list", func(t *testing.T) {
		stdout, stderr, code := runHearthdArgs(t, bin, "-config", cfgPath, "goal", "list")
		if code != 0 {
			t.Fatalf("goal list failed: code=%d, stderr=%q", code, stderr)
		}
		if !strings.Contains(stdout, "ID") || !strings.Contains(stdout, "STATUS") || !strings.Contains(stdout, "BUDGET") {
			t.Fatalf("unexpected goal table output: %q", stdout)
		}

		stdoutJSON, stderrJSON, codeJSON := runHearthdArgs(t, bin, "-config", cfgPath, "goal", "list", "--json")
		if codeJSON != 0 {
			t.Fatalf("goal list --json failed: code=%d, stderr=%q", codeJSON, stderrJSON)
		}
		var summary surface.GoalSummary
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdoutJSON)), &summary); err != nil {
			t.Fatalf("unmarshal goal summary: %v, raw=%q", err, stdoutJSON)
		}
		if summary.ID != goalID || summary.Status != "active" || summary.TurnBudget != 30 {
			t.Fatalf("unexpected goal summary: %+v", summary)
		}
	})

	// 10. hearthd goal pause
	t.Run("goal pause", func(t *testing.T) {
		stdout, stderr, code := runHearthdArgs(t, bin, "-config", cfgPath, "goal", "pause", goalID)
		if code != 0 {
			t.Fatalf("goal pause failed: code=%d, stderr=%q", code, stderr)
		}
		if !strings.Contains(stdout, goalID) {
			t.Fatalf("expected goal id in pause output: %q", stdout)
		}
	})

	// 11. hearthd goal resume
	t.Run("goal resume", func(t *testing.T) {
		stdout, stderr, code := runHearthdArgs(t, bin, "-config", cfgPath, "goal", "resume", goalID)
		if code != 0 {
			t.Fatalf("goal resume failed: code=%d, stderr=%q", code, stderr)
		}
		if !strings.Contains(stdout, goalID) {
			t.Fatalf("expected goal id in resume output: %q", stdout)
		}
	})

	// 12. hearthd goal close
	t.Run("goal close", func(t *testing.T) {
		stdout, stderr, code := runHearthdArgs(t, bin, "-config", cfgPath, "goal", "close", goalID)
		if code != 0 {
			t.Fatalf("goal close failed: code=%d, stderr=%q", code, stderr)
		}
		if !strings.Contains(stdout, goalID) {
			t.Fatalf("expected goal id in close output: %q", stdout)
		}
	})

	// 13. hearthd goal cancel
	t.Run("goal cancel", func(t *testing.T) {
		newOut, _, newCode := runHearthdArgs(t, bin, "-config", cfgPath, "goal", "new", "--objective", "cancel target")
		if newCode != 0 {
			t.Fatalf("goal new failed: %d", newCode)
		}
		cancelID := strings.TrimSpace(newOut)
		stdout, stderr, code := runHearthdArgs(t, bin, "-config", cfgPath, "goal", "cancel", cancelID)
		if code != 0 {
			t.Fatalf("goal cancel failed: code=%d, stderr=%q", code, stderr)
		}
		if !strings.Contains(stdout, cancelID) {
			t.Fatalf("expected goal id in cancel output: %q", stdout)
		}
	})

	// 14. Error handling: missing id positional
	t.Run("error missing id", func(t *testing.T) {
		_, stderr, code := runHearthdArgs(t, bin, "-config", cfgPath, "job", "pause")
		if code == 0 {
			t.Fatalf("expected non-zero code on missing id, got %d", code)
		}
		if len(strings.TrimSpace(stderr)) == 0 {
			t.Fatalf("expected stderr output on missing id")
		}
	})

	// 15. Error handling: daemon not listening
	t.Run("error daemon not listening", func(t *testing.T) {
		deadCfgPath, _ := makeThrowawayHearth(t)
		_, stderr, code := runHearthdArgs(t, bin, "-config", deadCfgPath, "job", "list")
		if code == 0 {
			t.Fatalf("expected non-zero exit code when daemon is dead")
		}
		if len(strings.TrimSpace(stderr)) == 0 {
			t.Fatalf("expected stderr one-liner on ERR")
		}
	})
}
