package surface

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/fasti"
	"github.com/lararium-app/lararium/internal/loop"
)

type testRealClock struct{}

func (testRealClock) Now() time.Time {
	return time.Now()
}

func (testRealClock) AfterFunc(d time.Duration, f func()) loop.Timer {
	t := time.AfterFunc(d, f)
	return &testRealTimer{t: t}
}

type testRealTimer struct {
	t *time.Timer
}

func (tr *testRealTimer) Stop() bool {
	return tr.t.Stop()
}

func (tr *testRealTimer) Reset(d time.Duration) bool {
	return tr.t.Reset(d)
}

type fastiTestEnv struct {
	srv  *Server
	h    http.Handler
	tok  string
	eng  *fasti.Engine
	home string
}

func setupFastiEnv(t *testing.T, withFasti bool) *fastiTestEnv {
	t.Helper()
	home := t.TempDir()

	// Write fixture HEARTBEAT.md
	hbContent := `---
cadence: daily
quiet: 22:00-08:00
---
- daily backup check
`
	if err := os.WriteFile(filepath.Join(home, "HEARTBEAT.md"), []byte(hbContent), 0o600); err != nil {
		t.Fatal(err)
	}

	tokensPath := filepath.Join(home, "tokens.json")
	store, err := OpenTokenStore(tokensPath)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := store.Create("fasti-test-token")
	if err != nil {
		t.Fatal(err)
	}

	srv := &Server{
		Store:    store,
		Sessions: &fakeSessionSource{},
		Hub:      &fakeHub{},
		Cfg:      ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}

	var eng *fasti.Engine
	if withFasti {
		cfg := fasti.DefaultConfig()
		cfg.Enabled = true
		dummyRunner := func(ctx context.Context, sessionID, prompt string) error { return nil }
		eng, err = fasti.New(cfg, home, fasti.WithClock(testRealClock{}), fasti.WithRunner(dummyRunner))
		if err != nil {
			t.Fatal(err)
		}
		if err := eng.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { eng.Stop() })
		srv.Fasti = eng
	}

	h := NewServer(srv)
	return &fastiTestEnv{
		srv:  srv,
		h:    h,
		tok:  tok,
		eng:  eng,
		home: home,
	}
}

func (e *fastiTestEnv) do(method, path, body string) (int, []byte) {
	w := credDo(e.h, method, path, e.tok, body)
	return w.Code, w.Body.Bytes()
}

func TestVPhSurface_StateBootstrap(t *testing.T) {
	// 1. Nil engine returns 503 {"error":"fasti_disabled"}
	envNil := setupFastiEnv(t, false)
	code, body := envNil.do(http.MethodGet, "/v1/fasti/state", "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for nil fasti, got %d", code)
	}
	var errResp map[string]string
	_ = json.Unmarshal(body, &errResp)
	if errResp["error"] != "fasti_disabled" {
		t.Fatalf("expected error fasti_disabled, got %v", errResp)
	}

	// 2. Running engine returns 200 snapshot
	env := setupFastiEnv(t, true)
	code, body = env.do(http.MethodGet, "/v1/fasti/state", "")
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", code, body)
	}

	var state FastiStateResponse
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("failed to unmarshal state: %v", err)
	}

	if state.Jobs == nil || state.Goals == nil {
		t.Fatalf("jobs and goals slices should not be nil")
	}
	if !state.Heartbeat.Enabled || state.Heartbeat.Cadence != "daily" || state.Heartbeat.Quiet != "22:00-08:00" {
		t.Fatalf("unexpected heartbeat state: %+v", state.Heartbeat)
	}
	if state.Noise.MaxAnnouncePerDay <= 0 {
		t.Fatalf("expected non-zero MaxAnnouncePerDay in noise snapshot, got %+v", state.Noise)
	}
}

func TestVPf_JobCreate201AndShape(t *testing.T) {
	env := setupFastiEnv(t, true)

	// Valid cron job
	reqBody := `{
		"name": "backup",
		"schedule": {"kind": "cron", "expr": "0 2 * * *"},
		"prompt": "run nightly database backup"
	}`
	code, body := env.do(http.MethodPost, "/v1/fasti/jobs", reqBody)
	if code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d (body: %s)", code, body)
	}

	var job fasti.Job
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatalf("unmarshal job: %v", err)
	}
	if job.ID == "" || !strings.HasPrefix(job.ID, "j_") {
		t.Fatalf("expected valid job ID, got %q", job.ID)
	}
	if job.Name != "backup" || job.Prompt != "run nightly database backup" {
		t.Fatalf("unexpected job fields: %+v", job)
	}
	if !job.Enabled || job.Paused {
		t.Fatalf("created job should be enabled and unpaused: %+v", job)
	}
	if job.NextDue == "" {
		t.Fatalf("expected next_due to be populated")
	}

	// Invalid schedule shape (missing expr)
	badSched := `{
		"name": "bad",
		"schedule": {"kind": "cron"},
		"prompt": "some prompt"
	}`
	code, body = env.do(http.MethodPost, "/v1/fasti/jobs", badSched)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad schedule, got %d (body: %s)", code, body)
	}

	// Invalid schedule kind
	badKind := `{
		"name": "bad",
		"schedule": {"kind": "unknown"},
		"prompt": "some prompt"
	}`
	code, _ = env.do(http.MethodPost, "/v1/fasti/jobs", badKind)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad kind, got %d", code)
	}

	// Multi-paragraph prompt refused
	multiPara := `{
		"name": "multi",
		"schedule": {"kind": "interval", "every": "10m"},
		"prompt": "paragraph one\nparagraph two"
	}`
	code, _ = env.do(http.MethodPost, "/v1/fasti/jobs", multiPara)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for multi-paragraph prompt, got %d", code)
	}
}

func TestVPf_PromptRuneLaw(t *testing.T) {
	env := setupFastiEnv(t, true)

	// 1001-rune ASCII prompt refused 400
	longPrompt := strings.Repeat("a", 1001)
	req1 := `{
		"name": "long-ascii",
		"schedule": {"kind": "interval", "every": "10m"},
		"prompt": "` + longPrompt + `"
	}`
	code, body := env.do(http.MethodPost, "/v1/fasti/jobs", req1)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for 1001-rune prompt, got %d (body: %s)", code, body)
	}

	// Multi-byte unicode prompt >1000 bytes but <=1000 runes ACCEPTED
	// \u4e16 is 3 UTF-8 bytes. 500 characters = 1500 bytes, but only 500 runes!
	runeChar := string([]rune{0x4e16})
	unicode500 := strings.Repeat(runeChar, 500)
	if len(unicode500) <= 1000 {
		t.Fatalf("fixture check: byte length should be >1000, got %d", len(unicode500))
	}
	req2 := `{
		"name": "unicode-ok",
		"schedule": {"kind": "interval", "every": "10m"},
		"prompt": "` + unicode500 + `"
	}`
	code, body = env.do(http.MethodPost, "/v1/fasti/jobs", req2)
	if code != http.StatusCreated {
		t.Fatalf("expected 201 for unicode <=1000 runes, got %d (body: %s)", code, body)
	}

	// Unicode prompt with 1001 runes REFUSED 400
	unicode1001 := strings.Repeat(runeChar, 1001)
	req3 := `{
		"name": "unicode-too-long",
		"schedule": {"kind": "interval", "every": "10m"},
		"prompt": "` + unicode1001 + `"
	}`
	code, body = env.do(http.MethodPost, "/v1/fasti/jobs", req3)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for 1001-rune unicode prompt, got %d (body: %s)", code, body)
	}
}

func TestVPf_PatchPromptOnEnabledAndDisabled(t *testing.T) {
	env := setupFastiEnv(t, true)

	// 1. Create job (created enabled)
	createReq := `{
		"name": "edit-target",
		"schedule": {"kind": "interval", "every": "15m"},
		"prompt": "initial prompt text"
	}`
	code, body := env.do(http.MethodPost, "/v1/fasti/jobs", createReq)
	if code != http.StatusCreated {
		t.Fatalf("create failed: %d (%s)", code, body)
	}
	var job fasti.Job
	_ = json.Unmarshal(body, &job)

	// 2. Prompt update while enabled: true -> refused 400
	patch1 := `{"prompt": "attempted update while enabled"}`
	code, body = env.do(http.MethodPatch, "/v1/fasti/jobs/"+job.ID, patch1)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 updating prompt on enabled job, got %d (%s)", code, body)
	}

	// 3. Disable the job via PATCH enabled: false
	patch2 := `{"enabled": false}`
	code, body = env.do(http.MethodPatch, "/v1/fasti/jobs/"+job.ID, patch2)
	if code != http.StatusOK {
		t.Fatalf("expected 200 disabling job, got %d (%s)", code, body)
	}

	// 4. Update prompt while disabled -> 200 OK!
	patch3 := `{"prompt": "successfully updated prompt text"}`
	code, body = env.do(http.MethodPatch, "/v1/fasti/jobs/"+job.ID, patch3)
	if code != http.StatusOK {
		t.Fatalf("expected 200 updating prompt on disabled job, got %d (%s)", code, body)
	}

	var updated fasti.Job
	_ = json.Unmarshal(body, &updated)
	if updated.Prompt != "successfully updated prompt text" {
		t.Fatalf("prompt was not updated: %q", updated.Prompt)
	}
	if updated.CreatedBy != "user:rest" {
		t.Fatalf("expected created_by user:rest (D9 adoption law), got %q", updated.CreatedBy)
	}

	// 5. Re-enable the job via PATCH enabled: true
	patch4 := `{"enabled": true}`
	code, body = env.do(http.MethodPatch, "/v1/fasti/jobs/"+job.ID, patch4)
	if code != http.StatusOK {
		t.Fatalf("expected 200 re-enabling job, got %d (%s)", code, body)
	}

	// Verify prompt update is once again refused now that it is enabled
	code, _ = env.do(http.MethodPatch, "/v1/fasti/jobs/"+job.ID, `{"prompt": "another try"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 on re-enabled job prompt patch, got %d", code)
	}
}

func TestVPf_JobDelete(t *testing.T) {
	env := setupFastiEnv(t, true)

	// Create job
	code, body := env.do(http.MethodPost, "/v1/fasti/jobs", `{
		"name": "to-delete",
		"schedule": {"kind": "interval", "every": "5m"},
		"prompt": "temp"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	var job fasti.Job
	_ = json.Unmarshal(body, &job)

	// Delete job -> 204
	code, _ = env.do(http.MethodDelete, "/v1/fasti/jobs/"+job.ID, "")
	if code != http.StatusNoContent {
		t.Fatalf("expected 204 on delete, got %d", code)
	}

	// Second delete on same ID -> 404
	code, _ = env.do(http.MethodDelete, "/v1/fasti/jobs/"+job.ID, "")
	if code != http.StatusNotFound {
		t.Fatalf("expected 404 for deleted job, got %d", code)
	}

	// PATCH on deleted job -> 404
	code, _ = env.do(http.MethodPatch, "/v1/fasti/jobs/"+job.ID, `{"enabled": true}`)
	if code != http.StatusNotFound {
		t.Fatalf("expected 404 for patch on deleted job, got %d", code)
	}
}

func TestVPf_JobRunNow(t *testing.T) {
	env := setupFastiEnv(t, true)

	// Create job
	code, body := env.do(http.MethodPost, "/v1/fasti/jobs", `{
		"name": "run-test",
		"schedule": {"kind": "interval", "every": "1h"},
		"prompt": "run prompt"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	var job fasti.Job
	_ = json.Unmarshal(body, &job)

	// POST .../run -> 202 Accepted
	code, body = env.do(http.MethodPost, "/v1/fasti/jobs/"+job.ID+"/run", "")
	if code != http.StatusAccepted {
		t.Fatalf("expected 202 on run-now, got %d (%s)", code, body)
	}

	// Run non-existent job -> 404
	code, _ = env.do(http.MethodPost, "/v1/fasti/jobs/nonexistent/run", "")
	if code != http.StatusNotFound {
		t.Fatalf("expected 404 on nonexistent run, got %d", code)
	}
}

func TestVPf_GoalCreateDefaults(t *testing.T) {
	env := setupFastiEnv(t, true)

	// Goal create with defaults (omitting turn_budget and step_after)
	req := `{
		"title": "Refactor surface handlers",
		"requirements": ["split handlers", "add tests"]
	}`
	code, body := env.do(http.MethodPost, "/v1/fasti/goals", req)
	if code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d (%s)", code, body)
	}

	var goal fasti.Goal
	if err := json.Unmarshal(body, &goal); err != nil {
		t.Fatalf("unmarshal goal: %v", err)
	}

	if goal.ID == "" {
		t.Fatalf("expected goal ID to be generated")
	}
	if goal.TurnBudget != 25 {
		t.Fatalf("expected default turn_budget 25, got %d", goal.TurnBudget)
	}
	if goal.StepAfter != 15*time.Minute {
		t.Fatalf("expected default step_after 15m, got %v", goal.StepAfter)
	}
	if len(goal.Requirements) != 2 {
		t.Fatalf("expected 2 requirements, got %d", len(goal.Requirements))
	}

	// Empty requirements -> 400
	code, _ = env.do(http.MethodPost, "/v1/fasti/goals", `{"title": "no reqs", "requirements": []}`)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty requirements, got %d", code)
	}
}

func TestVPf_GoalPatchStatusMap(t *testing.T) {
	env := setupFastiEnv(t, true)

	// Create goal
	code, body := env.do(http.MethodPost, "/v1/fasti/goals", `{
		"title": "Status test goal",
		"requirements": ["req1"]
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create goal: %d", code)
	}
	var goal fasti.Goal
	_ = json.Unmarshal(body, &goal)

	// PATCH status "paused"
	code, body = env.do(http.MethodPatch, "/v1/fasti/goals/"+goal.ID, `{"status": "paused"}`)
	if code != http.StatusOK {
		t.Fatalf("pause: %d (%s)", code, body)
	}
	var g1 fasti.Goal
	_ = json.Unmarshal(body, &g1)
	if g1.Status != "paused" {
		t.Fatalf("expected status paused, got %s", g1.Status)
	}

	// PATCH status "active"
	code, body = env.do(http.MethodPatch, "/v1/fasti/goals/"+goal.ID, `{"status": "active"}`)
	if code != http.StatusOK {
		t.Fatalf("resume: %d (%s)", code, body)
	}
	var g2 fasti.Goal
	_ = json.Unmarshal(body, &g2)
	if g2.Status != "active" {
		t.Fatalf("expected status active, got %s", g2.Status)
	}

	// PATCH budget 50
	code, body = env.do(http.MethodPatch, "/v1/fasti/goals/"+goal.ID, `{"turn_budget": 50}`)
	if code != http.StatusOK {
		t.Fatalf("budget: %d (%s)", code, body)
	}
	var g3 fasti.Goal
	_ = json.Unmarshal(body, &g3)
	if g3.TurnBudget != 50 {
		t.Fatalf("expected budget 50, got %d", g3.TurnBudget)
	}

	// PATCH status "failed" (cancel path)
	code, body = env.do(http.MethodPatch, "/v1/fasti/goals/"+goal.ID, `{"status": "failed"}`)
	if code != http.StatusOK {
		t.Fatalf("cancel: %d (%s)", code, body)
	}
	var g4 fasti.Goal
	_ = json.Unmarshal(body, &g4)
	if g4.Status != "failed" {
		t.Fatalf("expected status failed, got %s", g4.Status)
	}

	// Invalid status
	code, _ = env.do(http.MethodPatch, "/v1/fasti/goals/"+goal.ID, `{"status": "unknown"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown status, got %d", code)
	}
}

func TestVPf_GoalCloseOnlyUserForce(t *testing.T) {
	env := setupFastiEnv(t, true)

	// Create goal
	code, body := env.do(http.MethodPost, "/v1/fasti/goals", `{
		"title": "Close test goal",
		"requirements": ["req1"]
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create goal: %d", code)
	}
	var goal fasti.Goal
	_ = json.Unmarshal(body, &goal)

	// Mode "agent" refused with 400
	code, body = env.do(http.MethodPost, "/v1/fasti/goals/"+goal.ID+"/close", `{"mode": "agent"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-user_force close, got %d (%s)", code, body)
	}

	// Mode "user_force" accepted with 200
	code, body = env.do(http.MethodPost, "/v1/fasti/goals/"+goal.ID+"/close", `{"mode": "user_force"}`)
	if code != http.StatusOK {
		t.Fatalf("expected 200 for user_force close, got %d (%s)", code, body)
	}
	var closed fasti.Goal
	_ = json.Unmarshal(body, &closed)
	if closed.Status != "done" {
		t.Fatalf("expected status done, got %s", closed.Status)
	}

	// Close on nonexistent goal -> 404
	code, _ = env.do(http.MethodPost, "/v1/fasti/goals/nonexistent/close", `{"mode": "user_force"}`)
	if code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent goal close, got %d", code)
	}
}

// F4a reviewer-fix law test: a PATCH must be atomic and identity-preserving
// (no remove-and-re-add): id survives, pause state survives, and a prompt
// edit through the user tier adopts per D9 (created_by -> user:rest).
func TestVPf_PatchIdentityAndAdoptionLaws(t *testing.T) {
	env := setupFastiEnv(t, true)

	code, body := env.do(http.MethodPost, "/v1/fasti/jobs", `{
		"name": "identity",
		"schedule": {"kind": "interval", "every": "1h"},
		"prompt": "original prompt text"
	}`)
	if code != http.StatusCreated {
		t.Fatalf("create: got %d (%s)", code, body)
	}
	var created fasti.Job
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Pause first (D9 edit path), then edit schedule and prompt separately.
	if code, _ := env.do(http.MethodPatch, "/v1/fasti/jobs/"+created.ID, `{"enabled": false}`); code != http.StatusOK {
		t.Fatalf("pause: got %d", code)
	}

	code, body = env.do(http.MethodPatch, "/v1/fasti/jobs/"+created.ID, `{"schedule": {"kind": "interval", "every": "3h"}}`)
	if code != http.StatusOK {
		t.Fatalf("schedule patch: got %d (%s)", code, body)
	}
	var afterSched fasti.Job
	if err := json.Unmarshal(body, &afterSched); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if afterSched.ID != created.ID {
		t.Fatalf("identity law: id changed %q -> %q", created.ID, afterSched.ID)
	}
	if !afterSched.Paused {
		t.Fatalf("pause state lost through schedule edit: %+v", afterSched)
	}
	if afterSched.CreatedBy != "user" {
		t.Fatalf("schedule-only edit must not adopt, created_by = %q", afterSched.CreatedBy)
	}

	code, body = env.do(http.MethodPatch, "/v1/fasti/jobs/"+created.ID, `{"prompt": "revised prompt text"}`)
	if code != http.StatusOK {
		t.Fatalf("prompt patch: got %d (%s)", code, body)
	}
	var afterPrompt fasti.Job
	if err := json.Unmarshal(body, &afterPrompt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if afterPrompt.ID != created.ID {
		t.Fatalf("identity law: id changed through prompt edit")
	}
	if afterPrompt.CreatedBy != "user:rest" {
		t.Fatalf("D9 adoption law: created_by = %q, want user:rest", afterPrompt.CreatedBy)
	}

	// Replay: PATCH with no fields returns the same row (declarative no-op).
	code, body = env.do(http.MethodPatch, "/v1/fasti/jobs/"+created.ID, `{}`)
	if code != http.StatusOK {
		t.Fatalf("no-op patch: got %d (%s)", code, body)
	}
	var replayed fasti.Job
	if err := json.Unmarshal(body, &replayed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if replayed.Prompt != "revised prompt text" || replayed.CreatedBy != "user:rest" {
		t.Fatalf("no-op patch changed the row: %+v", replayed)
	}
}
