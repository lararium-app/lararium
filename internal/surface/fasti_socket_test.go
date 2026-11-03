package surface_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/fasti"
	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/surface"
)

type sockRealClock struct{}

func (sockRealClock) Now() time.Time {
	return time.Now()
}

func (sockRealClock) AfterFunc(d time.Duration, f func()) loop.Timer {
	t := time.AfterFunc(d, f)
	return &sockRealTimer{t: t}
}

type sockRealTimer struct {
	t *time.Timer
}

func (sr *sockRealTimer) Stop() bool {
	return sr.t.Stop()
}

func (sr *sockRealTimer) Reset(d time.Duration) bool {
	return sr.t.Reset(d)
}

type socketTestEnv struct {
	srv      *surface.Server
	eng      *fasti.Engine
	home     string
	sockPath string
	closer   interface{ Close() error }
}

func setupSocketTestEnv(t *testing.T, withEngine bool) *socketTestEnv {
	t.Helper()
	home := t.TempDir()
	sockPath := filepath.Join(home, "hearthd.sock")

	_ = os.WriteFile(filepath.Join(home, "HEARTBEAT.md"), []byte("---\ncadence: daily\n---\n- check\n"), 0o600)

	srv := &surface.Server{}

	var eng *fasti.Engine
	if withEngine {
		cfg := fasti.DefaultConfig()
		cfg.Enabled = true
		dummyRunner := func(ctx context.Context, sessionID, prompt string) error { return nil }
		var err error
		eng, err = fasti.New(cfg, home, fasti.WithClock(sockRealClock{}), fasti.WithRunner(dummyRunner))
		if err != nil {
			t.Fatalf("fasti.New: %v", err)
		}
		if err := eng.Start(context.Background()); err != nil {
			t.Fatalf("eng.Start: %v", err)
		}
		srv.Fasti = eng
	}

	closer, err := srv.ServeSocket(sockPath, func() error { return nil })
	if err != nil {
		t.Fatalf("ServeSocket: %v", err)
	}

	t.Cleanup(func() {
		_ = closer.Close()
		if eng != nil {
			eng.Stop()
		}
	})

	return &socketTestEnv{
		srv:      srv,
		eng:      eng,
		home:     home,
		sockPath: sockPath,
		closer:   closer,
	}
}

func sendSocketLine(t *testing.T, sockPath, line string) string {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	if _, err := conn.Write([]byte(line)); err != nil {
		t.Fatalf("conn.Write: %v", err)
	}

	r := bufio.NewReader(conn)
	resp, err := r.ReadString('\n')
	if err != nil {
		return strings.TrimSpace(resp)
	}
	return strings.TrimSpace(resp)
}

func readSocketLines(t *testing.T, sockPath, line string) []string {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("net.Dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	if _, err := conn.Write([]byte(line)); err != nil {
		t.Fatalf("conn.Write: %v", err)
	}

	r := bufio.NewReader(conn)
	var lines []string
	for {
		l, err := r.ReadString('\n')
		l = strings.TrimSpace(l)
		if l != "" {
			lines = append(lines, l)
		}
		if err != nil {
			break
		}
	}
	return lines
}

func TestSocket_VerbMatrixHappyPaths(t *testing.T) {
	env := setupSocketTestEnv(t, true)

	var jobID string
	var goalID string

	// Table of operations covering full matrix
	t.Run("job add", func(t *testing.T) {
		req := `FASTI {"verb":"add","entity":"job","name":"test-job","schedule":{"kind":"interval","every":"30m"},"prompt":"do work"}`
		resp := sendSocketLine(t, env.sockPath, req)
		if !strings.HasPrefix(resp, "OK {") {
			t.Fatalf("job add failed: %s", resp)
		}
		var ack struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(resp, "OK ")), &ack); err != nil {
			t.Fatalf("parse ack: %v", err)
		}
		if ack.ID == "" {
			t.Fatalf("expected non-empty job ID in ack: %s", resp)
		}
		jobID = ack.ID
	})

	t.Run("job list", func(t *testing.T) {
		req := `FASTI {"verb":"list","entity":"job"}`
		lines := readSocketLines(t, env.sockPath, req)
		if len(lines) != 1 {
			t.Fatalf("expected 1 line, got %d (%v)", len(lines), lines)
		}
		var summary surface.JobSummary
		if err := json.Unmarshal([]byte(lines[0]), &summary); err != nil {
			t.Fatalf("unmarshal JobSummary: %v", err)
		}
		if summary.ID != jobID || summary.Name != "test-job" || !summary.Enabled {
			t.Fatalf("unexpected JobSummary: %+v", summary)
		}
	})

	t.Run("job pause", func(t *testing.T) {
		req := fmt.Sprintf(`FASTI {"verb":"pause","entity":"job","id":%q}`, jobID)
		resp := sendSocketLine(t, env.sockPath, req)
		if !strings.HasPrefix(resp, "OK {") || !strings.Contains(resp, jobID) {
			t.Fatalf("job pause failed: %s", resp)
		}
	})

	t.Run("job edit while paused", func(t *testing.T) {
		req := fmt.Sprintf(`FASTI {"verb":"edit","entity":"job","id":%q,"prompt":"updated prompt"}`, jobID)
		resp := sendSocketLine(t, env.sockPath, req)
		if !strings.HasPrefix(resp, "OK {") || !strings.Contains(resp, jobID) {
			t.Fatalf("job edit failed: %s", resp)
		}
	})

	t.Run("job resume", func(t *testing.T) {
		req := fmt.Sprintf(`FASTI {"verb":"resume","entity":"job","id":%q}`, jobID)
		resp := sendSocketLine(t, env.sockPath, req)
		if !strings.HasPrefix(resp, "OK {") || !strings.Contains(resp, jobID) {
			t.Fatalf("job resume failed: %s", resp)
		}
	})

	t.Run("job run", func(t *testing.T) {
		req := fmt.Sprintf(`FASTI {"verb":"run","entity":"job","id":%q}`, jobID)
		resp := sendSocketLine(t, env.sockPath, req)
		if !strings.HasPrefix(resp, "OK {") || !strings.Contains(resp, jobID) {
			t.Fatalf("job run failed: %s", resp)
		}
	})

	t.Run("job remove", func(t *testing.T) {
		req := fmt.Sprintf(`FASTI {"verb":"remove","entity":"job","id":%q}`, jobID)
		resp := sendSocketLine(t, env.sockPath, req)
		if !strings.HasPrefix(resp, "OK {") || !strings.Contains(resp, jobID) {
			t.Fatalf("job remove failed: %s", resp)
		}
	})

	t.Run("goal new", func(t *testing.T) {
		req := `FASTI {"verb":"new","entity":"goal","title":"goal-1","requirements":["req 1"],"turn_budget":15}`
		resp := sendSocketLine(t, env.sockPath, req)
		if !strings.HasPrefix(resp, "OK {") {
			t.Fatalf("goal new failed: %s", resp)
		}
		var ack struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(resp, "OK ")), &ack); err != nil {
			t.Fatalf("parse ack: %v", err)
		}
		if ack.ID == "" {
			t.Fatalf("expected non-empty goal ID in ack: %s", resp)
		}
		goalID = ack.ID
	})

	t.Run("goal list", func(t *testing.T) {
		req := `FASTI {"verb":"list","entity":"goal"}`
		lines := readSocketLines(t, env.sockPath, req)
		if len(lines) != 1 {
			t.Fatalf("expected 1 line, got %d (%v)", len(lines), lines)
		}
		var summary surface.GoalSummary
		if err := json.Unmarshal([]byte(lines[0]), &summary); err != nil {
			t.Fatalf("unmarshal GoalSummary: %v", err)
		}
		if summary.ID != goalID || summary.Status != "active" || summary.TurnBudget != 15 {
			t.Fatalf("unexpected GoalSummary: %+v", summary)
		}
	})

	t.Run("goal pause", func(t *testing.T) {
		req := fmt.Sprintf(`FASTI {"verb":"pause","entity":"goal","id":%q}`, goalID)
		resp := sendSocketLine(t, env.sockPath, req)
		if !strings.HasPrefix(resp, "OK {") || !strings.Contains(resp, goalID) {
			t.Fatalf("goal pause failed: %s", resp)
		}
	})

	t.Run("goal resume", func(t *testing.T) {
		req := fmt.Sprintf(`FASTI {"verb":"resume","entity":"goal","id":%q}`, goalID)
		resp := sendSocketLine(t, env.sockPath, req)
		if !strings.HasPrefix(resp, "OK {") || !strings.Contains(resp, goalID) {
			t.Fatalf("goal resume failed: %s", resp)
		}
	})

	t.Run("goal close", func(t *testing.T) {
		req := fmt.Sprintf(`FASTI {"verb":"close","entity":"goal","id":%q}`, goalID)
		resp := sendSocketLine(t, env.sockPath, req)
		if !strings.HasPrefix(resp, "OK {") || !strings.Contains(resp, goalID) {
			t.Fatalf("goal close failed: %s", resp)
		}
	})

	t.Run("goal cancel second goal", func(t *testing.T) {
		newReq := `FASTI {"verb":"new","entity":"goal","title":"goal-2","requirements":["req A"],"turn_budget":10}`
		newResp := sendSocketLine(t, env.sockPath, newReq)
		var ack struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(strings.TrimPrefix(newResp, "OK ")), &ack)
		cancelReq := fmt.Sprintf(`FASTI {"verb":"cancel","entity":"goal","id":%q}`, ack.ID)
		resp := sendSocketLine(t, env.sockPath, cancelReq)
		if !strings.HasPrefix(resp, "OK {") || !strings.Contains(resp, ack.ID) {
			t.Fatalf("goal cancel failed: %s", resp)
		}
	})
}

func TestSocket_ErrorsAndGating(t *testing.T) {
	env := setupSocketTestEnv(t, true)

	tests := []struct {
		name    string
		req     string
		wantErr string
	}{
		{
			name:    "unknown verb",
			req:     `FASTI {"verb":"dance","entity":"job"}`,
			wantErr: "ERR unknown_verb dance",
		},
		{
			name:    "unknown entity",
			req:     `FASTI {"verb":"add","entity":"wizard"}`,
			wantErr: "ERR unknown_entity wizard",
		},
		{
			name:    "bad payload malformed json",
			req:     `FASTI {malformed json`,
			wantErr: "ERR bad_payload",
		},
		{
			name:    "not found job",
			req:     `FASTI {"verb":"pause","entity":"job","id":"j_nonexistent0000000000000"}`,
			wantErr: "ERR not_found j_nonexistent0000000000000",
		},
		{
			name:    "not found goal",
			req:     `FASTI {"verb":"pause","entity":"goal","id":"g_nonexistent0000000000000"}`,
			wantErr: "ERR not_found g_nonexistent0000000000000",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := sendSocketLine(t, env.sockPath, tc.req)
			if !strings.HasPrefix(resp, tc.wantErr) {
				t.Fatalf("got %q, want prefix %q", resp, tc.wantErr)
			}
		})
	}

	t.Run("disabled fasti engine", func(t *testing.T) {
		noEngEnv := setupSocketTestEnv(t, false)
		resp := sendSocketLine(t, noEngEnv.sockPath, `FASTI {"verb":"list","entity":"job"}`)
		if resp != "ERR fasti_disabled" {
			t.Fatalf("got %q, want ERR fasti_disabled", resp)
		}
	})

	t.Run("over-cap payload returns ERR payload_too_large", func(t *testing.T) {
		// Create a line > 4096 bytes starting with FASTI
		hugePadding := strings.Repeat("A", 4200)
		hugeReq := `FASTI {"verb":"add","entity":"job","padding":"` + hugePadding + `"}`
		resp := sendSocketLine(t, env.sockPath, hugeReq)
		if resp != "ERR payload_too_large" {
			t.Fatalf("got %q, want ERR payload_too_large", resp)
		}
	})
}

func TestSocket_LifecycleIdempotence(t *testing.T) {
	env := setupSocketTestEnv(t, true)

	// Create job
	addResp := sendSocketLine(t, env.sockPath, `FASTI {"verb":"add","entity":"job","schedule":{"kind":"interval","every":"10m"},"prompt":"p"}`)
	var jobAck struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(strings.TrimPrefix(addResp, "OK ")), &jobAck)

	// Pause job twice: both return OK
	p1 := sendSocketLine(t, env.sockPath, fmt.Sprintf(`FASTI {"verb":"pause","entity":"job","id":%q}`, jobAck.ID))
	if !strings.HasPrefix(p1, "OK") {
		t.Fatalf("first pause failed: %s", p1)
	}
	p2 := sendSocketLine(t, env.sockPath, fmt.Sprintf(`FASTI {"verb":"pause","entity":"job","id":%q}`, jobAck.ID))
	if !strings.HasPrefix(p2, "OK") {
		t.Fatalf("second pause failed: %s", p2)
	}

	// Create goal
	goalResp := sendSocketLine(t, env.sockPath, `FASTI {"verb":"new","entity":"goal","title":"g","requirements":["r"]}`)
	var goalAck struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(strings.TrimPrefix(goalResp, "OK ")), &goalAck)

	// Pause goal twice
	gp1 := sendSocketLine(t, env.sockPath, fmt.Sprintf(`FASTI {"verb":"pause","entity":"goal","id":%q}`, goalAck.ID))
	if !strings.HasPrefix(gp1, "OK") {
		t.Fatalf("first goal pause failed: %s", gp1)
	}
	gp2 := sendSocketLine(t, env.sockPath, fmt.Sprintf(`FASTI {"verb":"pause","entity":"goal","id":%q}`, goalAck.ID))
	if !strings.HasPrefix(gp2, "OK") {
		t.Fatalf("second goal pause failed: %s", gp2)
	}

	// D8 retry-idempotence: a retried remove (target state = absence)
	// acks as a no-op success, and close/cancel are no-ops on goals
	// already in the target state. The law covers remove|close|cancel,
	// not just pause|resume.
	if r1 := sendSocketLine(t, env.sockPath, fmt.Sprintf(`FASTI {"verb":"remove","entity":"job","id":%q}`, jobAck.ID)); !strings.HasPrefix(r1, "OK") {
		t.Fatalf("first job remove failed: %s", r1)
	}
	if r2 := sendSocketLine(t, env.sockPath, fmt.Sprintf(`FASTI {"verb":"remove","entity":"job","id":%q}`, jobAck.ID)); !strings.HasPrefix(r2, "OK") {
		t.Fatalf("retried job remove must ack OK, got: %s", r2)
	}
	if c1 := sendSocketLine(t, env.sockPath, fmt.Sprintf(`FASTI {"verb":"close","entity":"goal","id":%q}`, goalAck.ID)); !strings.HasPrefix(c1, "OK") {
		t.Fatalf("first goal close failed: %s", c1)
	}
	if c2 := sendSocketLine(t, env.sockPath, fmt.Sprintf(`FASTI {"verb":"close","entity":"goal","id":%q}`, goalAck.ID)); !strings.HasPrefix(c2, "OK") {
		t.Fatalf("retried goal close failed: %s", c2)
	}
	var goalAck2 struct {
		ID string `json:"id"`
	}
	g2 := sendSocketLine(t, env.sockPath, `FASTI {"verb":"new","entity":"goal","title":"g2","requirements":["r"]}`)
	_ = json.Unmarshal([]byte(strings.TrimPrefix(g2, "OK ")), &goalAck2)
	if x1 := sendSocketLine(t, env.sockPath, fmt.Sprintf(`FASTI {"verb":"cancel","entity":"goal","id":%q}`, goalAck2.ID)); !strings.HasPrefix(x1, "OK") {
		t.Fatalf("first goal cancel failed: %s", x1)
	}
	if x2 := sendSocketLine(t, env.sockPath, fmt.Sprintf(`FASTI {"verb":"cancel","entity":"goal","id":%q}`, goalAck2.ID)); !strings.HasPrefix(x2, "OK") {
		t.Fatalf("retried goal cancel failed: %s", x2)
	}
}

func TestSocket_EditReplayDedup(t *testing.T) {
	env := setupSocketTestEnv(t, true)

	// Create job (starts enabled)
	addResp := sendSocketLine(t, env.sockPath, `FASTI {"verb":"add","entity":"job","schedule":{"kind":"interval","every":"10m"},"prompt":"initial"}`)
	var jobAck struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(strings.TrimPrefix(addResp, "OK ")), &jobAck)

	// Pause so prompt can be edited
	sendSocketLine(t, env.sockPath, fmt.Sprintf(`FASTI {"verb":"pause","entity":"job","id":%q}`, jobAck.ID))

	// Edit 1: change prompt
	editReq1 := fmt.Sprintf(`FASTI {"verb":"edit","entity":"job","id":%q,"prompt":"first edit text"}`, jobAck.ID)
	ack1 := sendSocketLine(t, env.sockPath, editReq1)
	if !strings.HasPrefix(ack1, "OK") || strings.Contains(ack1, "replayed") {
		t.Fatalf("expected first edit to succeed without replayed:true, got: %s", ack1)
	}

	// Replay exact same payload: second ack says replayed:true
	ack2 := sendSocketLine(t, env.sockPath, editReq1)
	if !strings.HasPrefix(ack2, "OK") || !strings.Contains(ack2, `"replayed":true`) {
		t.Fatalf("expected replayed:true on duplicate payload, got: %s", ack2)
	}

	// Send different payload to same ID: applies normally (not replayed)
	editReq2 := fmt.Sprintf(`FASTI {"verb":"edit","entity":"job","id":%q,"prompt":"second edit text"}`, jobAck.ID)
	ack3 := sendSocketLine(t, env.sockPath, editReq2)
	if !strings.HasPrefix(ack3, "OK") || strings.Contains(ack3, "replayed") {
		t.Fatalf("expected new payload to apply normally, got: %s", ack3)
	}

	// Verify job actually has second prompt text
	job, err := env.eng.GetJob(context.Background(), jobAck.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Prompt != "second edit text" {
		t.Fatalf("job prompt = %q, want 'second edit text'", job.Prompt)
	}
}

func TestSocket_ListSummaryProjectionShapeAndFit(t *testing.T) {
	env := setupSocketTestEnv(t, true)

	// 1. Write job directly into jobs.json with 4000-rune prompt
	hugePrompt := strings.Repeat("A", 4000)
	rawJob := fasti.Job{
		ID:        "j_01HNHUGEPROMPT0000000000",
		Name:      "huge-job",
		Schedule:  fasti.Schedule{Kind: "interval", Every: "1h"},
		Prompt:    hugePrompt,
		Enabled:   true,
		CreatedBy: "user",
		NextDue:   time.Now().UTC().Format(time.RFC3339),
	}
	jobsFile := filepath.Join(env.home, "fasti", "jobs.json")
	jobsData, err := json.Marshal([]fasti.Job{rawJob})
	if err != nil {
		t.Fatalf("marshal jobs: %v", err)
	}
	if err := os.WriteFile(jobsFile, jobsData, 0o600); err != nil {
		t.Fatalf("write jobs.json: %v", err)
	}

	// Re-start engine so it loads this job
	env.eng.Stop()
	cfg := fasti.DefaultConfig()
	cfg.Enabled = true
	dummyRunner := func(ctx context.Context, sessionID, prompt string) error { return nil }
	newEng, err := fasti.New(cfg, env.home, fasti.WithClock(sockRealClock{}), fasti.WithRunner(dummyRunner))
	if err != nil {
		t.Fatalf("fasti.New: %v", err)
	}
	if err := newEng.Start(context.Background()); err != nil {
		t.Fatalf("newEng.Start: %v", err)
	}
	env.srv.Fasti = newEng
	env.eng = newEng

	// Query list over socket
	lines := readSocketLines(t, env.sockPath, `FASTI {"verb":"list","entity":"job"}`)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	rowStr := lines[0]

	// Must fit 4096 bytes (in fact, it should be well under 500 bytes!)
	if len(rowStr) >= 4096 {
		t.Fatalf("summary row exceeded 4096 bytes: len=%d", len(rowStr))
	}

	// Must NOT contain the 4000-rune prompt
	if strings.Contains(rowStr, strings.Repeat("A", 50)) {
		t.Fatalf("summary row projected the prompt; it must exclude prompt")
	}

	// Summary shapes exact: {id, name, schedule, enabled, next_due}
	var m map[string]any
	if err := json.Unmarshal([]byte(rowStr), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	expectedKeys := []string{"id", "name", "schedule", "enabled", "next_due"}
	for _, k := range expectedKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("expected key %q in job summary, got keys: %v", k, m)
		}
	}
	if len(m) != len(expectedKeys) {
		t.Errorf("job summary has unexpected keys: got %d, want %d (%v)", len(m), len(expectedKeys), m)
	}

	// Goal summary shapes exact: {id, status, turn_count, turn_budget, next_due}
	gResp := sendSocketLine(t, env.sockPath, `FASTI {"verb":"new","entity":"goal","title":"goal-shape","requirements":["r1"]}`)
	if !strings.HasPrefix(gResp, "OK") {
		t.Fatalf("goal create failed: %s", gResp)
	}

	gLines := readSocketLines(t, env.sockPath, `FASTI {"verb":"list","entity":"goal"}`)
	if len(gLines) != 1 {
		t.Fatalf("expected 1 goal line, got %d", len(gLines))
	}
	var gm map[string]any
	if err := json.Unmarshal([]byte(gLines[0]), &gm); err != nil {
		t.Fatalf("unmarshal goal: %v", err)
	}
	expectedGoalKeys := []string{"id", "status", "turn_count", "turn_budget", "next_due"}
	for _, k := range expectedGoalKeys {
		if _, ok := gm[k]; !ok {
			t.Errorf("expected key %q in goal summary, got keys: %v", k, gm)
		}
	}
	if len(gm) != len(expectedGoalKeys) {
		t.Errorf("goal summary has unexpected keys: got %d, want %d (%v)", len(gm), len(expectedGoalKeys), gm)
	}

	// Rev-5 presence rule: un-armed rows omit next_due. Pause the job and
	// close the goal, then re-list: next_due must be absent and enabled
	// must read false for the paused job.
	if p := sendSocketLine(t, env.sockPath, fmt.Sprintf(`FASTI {"verb":"pause","entity":"job","id":%q}`, m["id"])); !strings.HasPrefix(p, "OK") {
		t.Fatalf("pause for presence check failed: %s", p)
	}
	pj := readSocketLines(t, env.sockPath, `FASTI {"verb":"list","entity":"job"}`)
	var pm map[string]any
	if err := json.Unmarshal([]byte(pj[0]), &pm); err != nil {
		t.Fatalf("unmarshal paused job row: %v", err)
	}
	if _, ok := pm["next_due"]; ok {
		t.Errorf("paused job row must omit next_due, got: %v", pm)
	}
	if pm["enabled"] == true {
		t.Errorf("paused job row must report enabled=false, got: %v", pm)
	}
	if c := sendSocketLine(t, env.sockPath, fmt.Sprintf(`FASTI {"verb":"close","entity":"goal","id":%q}`, gm["id"])); !strings.HasPrefix(c, "OK") {
		t.Fatalf("close for presence check failed: %s", c)
	}
	pgl := readSocketLines(t, env.sockPath, `FASTI {"verb":"list","entity":"goal"}`)
	var pgm map[string]any
	if err := json.Unmarshal([]byte(pgl[0]), &pgm); err != nil {
		t.Fatalf("unmarshal closed goal row: %v", err)
	}
	if _, ok := pgm["next_due"]; ok {
		t.Errorf("closed goal row must omit next_due, got: %v", pgm)
	}
}
