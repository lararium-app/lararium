package surface

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lararium-app/lararium/internal/fasti"
)

// badSessionRefs are caller-supplied session values that must never reach
// a filesystem join under <home>/sessions.
var badSessionRefs = []string{
	"..",
	"../escape",
	"../../../../tmp/escape",
	"a/b",
	"/abs/path",
	`a\b`,
	".hidden",
	"-flag",
	"a\x00b",
	"has space",
	strings.Repeat("a", 129),
}

func jsonString(s string) string {
	b := []byte{'"'}
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b = append(b, '\\', byte(r))
		case r < 0x20:
			b = append(b, []byte(`\u00`)...)
			b = append(b, "0123456789abcdef"[r>>4], "0123456789abcdef"[r&0xf])
		default:
			b = append(b, string(r)...)
		}
	}
	return string(append(b, '"'))
}

// TestFastiJobCreateRejectsEscapingSession: the job create route refuses a
// session that is not a single safe path component, and nothing lands in
// jobs.json.
func TestFastiJobCreateRejectsEscapingSession(t *testing.T) {
	env := setupFastiEnv(t, true)
	for _, sess := range badSessionRefs {
		body := `{"name":"n","schedule":{"kind":"cron","expr":"0 2 * * *"},"prompt":"p","session":` + jsonString(sess) + `}`
		code, resp := env.do(http.MethodPost, "/v1/fasti/jobs", body)
		if code != http.StatusBadRequest {
			t.Errorf("session %q: got %d (%s), want 400", sess, code, resp)
		}
	}
	if raw, err := os.ReadFile(fasti.JobsFilePath(env.home)); err == nil && len(strings.TrimSpace(string(raw))) > 2 {
		t.Fatalf("a rejected job was persisted: %s", raw)
	}

	// Control: a plain custom session and the default are still accepted.
	for _, body := range []string{
		`{"name":"ok1","schedule":{"kind":"cron","expr":"0 2 * * *"},"prompt":"p","session":"ops-nightly"}`,
		`{"name":"ok2","schedule":{"kind":"cron","expr":"0 3 * * *"},"prompt":"p"}`,
	} {
		if code, resp := env.do(http.MethodPost, "/v1/fasti/jobs", body); code != http.StatusCreated {
			t.Fatalf("valid job rejected: %d %s", code, resp)
		}
	}
	// Positive control: the path read above is where jobs really land, so
	// the "nothing persisted" check before it is not vacuous.
	raw, err := os.ReadFile(fasti.JobsFilePath(env.home))
	if err != nil || !strings.Contains(string(raw), "ops-nightly") {
		t.Fatalf("accepted job not found at %s (err=%v): %s", fasti.JobsFilePath(env.home), err, raw)
	}
}

// TestFastiGoalCreateRejectsEscapingSession: the goal create route refuses
// the same shapes.
func TestFastiGoalCreateRejectsEscapingSession(t *testing.T) {
	env := setupFastiEnv(t, true)
	for _, sess := range badSessionRefs {
		body := `{"title":"t","requirements":["r"],"session":` + jsonString(sess) + `}`
		code, resp := env.do(http.MethodPost, "/v1/fasti/goals", body)
		if code != http.StatusBadRequest {
			t.Errorf("session %q: got %d (%s), want 400", sess, code, resp)
		}
	}
	if raw, err := os.ReadFile(fasti.GoalsFilePath(env.home)); err == nil && len(strings.TrimSpace(string(raw))) > 2 {
		t.Fatalf("a rejected goal was persisted: %s", raw)
	}
	body := `{"title":"t","requirements":["r"],"session":"goal-ok"}`
	if code, resp := env.do(http.MethodPost, "/v1/fasti/goals", body); code != http.StatusCreated {
		t.Fatalf("valid goal rejected: %d %s", code, resp)
	}
	raw, err := os.ReadFile(fasti.GoalsFilePath(env.home))
	if err != nil || !strings.Contains(string(raw), "goal-ok") {
		t.Fatalf("accepted goal not found at %s (err=%v): %s", fasti.GoalsFilePath(env.home), err, raw)
	}
}

// TestRunHeadlessTurnRejectsEscapingSession: the sink refuses a hostile id
// even when a row already on disk or a future caller supplies it, and no
// directory is created outside the sessions root.
func TestRunHeadlessTurnRejectsEscapingSession(t *testing.T) {
	hub := setupHub(t, &stubProvider{})
	home := hub.hearthHome
	// Every place a hostile id could materialise a directory: above the
	// hearth, directly under it, beside the sessions root, and as a nested
	// path inside the sessions root.
	probes := []string{
		filepath.Join(filepath.Dir(home), "escaped-by-headless"),
		filepath.Join(home, "escaped-by-headless"),
		filepath.Join(home, "sessions", "a"),
	}
	t.Cleanup(func() {
		for _, p := range probes[:2] {
			_ = os.RemoveAll(p)
		}
	})

	for _, sess := range []string{"../../escaped-by-headless", "../escaped-by-headless", "a/b", "..", ""} {
		err := hub.RunHeadlessTurn(context.Background(), sess, "hi", "cron", 0, nil)
		if !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("session %q: got %v, want ErrInvalidSessionID", sess, err)
		}
	}
	for _, p := range probes {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("hostile session id materialised %s (stat err=%v)", p, err)
		}
	}
}
