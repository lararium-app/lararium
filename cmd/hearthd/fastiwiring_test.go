package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/fasti"
	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/nuntius"
	"github.com/lararium-app/lararium/internal/surface"
)

func TestFastiConfig(t *testing.T) {
	baseYAML := `hearth:
  home: /tmp/hearth-test
models:
  default: [local/m]
providers:
  - name: local
    base_url: http://127.0.0.1:8080/v1
`

	cases := []struct {
		name    string
		extra   string
		wantErr string
		check   func(t *testing.T, cfg *Config)
	}{
		{
			name:  "absent fasti key uses DefaultConfig",
			extra: "",
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				def := fasti.DefaultConfig()
				if cfg.Fasti != def {
					t.Fatalf("got %+v, want %+v", cfg.Fasti, def)
				}
				if cfg.Fasti.Enabled {
					t.Fatalf("expected Fasti.Enabled=false by default")
				}
				if cfg.Fasti.Heartbeat.DailyAt != "07:30" {
					t.Fatalf("got DailyAt=%q, want 07:30", cfg.Fasti.Heartbeat.DailyAt)
				}
				if cfg.Fasti.MaxAnnouncePerDay != 8 {
					t.Fatalf("got MaxAnnouncePerDay=%d, want 8", cfg.Fasti.MaxAnnouncePerDay)
				}
				if cfg.Fasti.CatchupWindow != 24*time.Hour {
					t.Fatalf("got CatchupWindow=%v, want 24h", cfg.Fasti.CatchupWindow)
				}
			},
		},
		{
			name: "explicit enabled false preserves defaults",
			extra: `fasti:
  enabled: false
`,
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Fasti.Enabled {
					t.Fatalf("expected Enabled=false")
				}
				if cfg.Fasti.MaxAnnouncePerDay != 8 {
					t.Fatalf("got MaxAnnouncePerDay=%d, want 8", cfg.Fasti.MaxAnnouncePerDay)
				}
				if cfg.Fasti.Heartbeat.DailyAt != "07:30" {
					t.Fatalf("got DailyAt=%q, want 07:30", cfg.Fasti.Heartbeat.DailyAt)
				}
			},
		},
		{
			name: "explicit custom values and zero max_announce_per_day",
			extra: `fasti:
  enabled: true
  quiet_hours: "22:00-07:00"
  max_announce_per_day: 0
  catchup_window: 12h
  heartbeat:
    daily_at: "08:15"
`,
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if !cfg.Fasti.Enabled {
					t.Fatalf("expected Enabled=true")
				}
				if cfg.Fasti.MaxAnnouncePerDay != 0 {
					t.Fatalf("got MaxAnnouncePerDay=%d, want 0", cfg.Fasti.MaxAnnouncePerDay)
				}
				if cfg.Fasti.CatchupWindow != 12*time.Hour {
					t.Fatalf("got CatchupWindow=%v, want 12h", cfg.Fasti.CatchupWindow)
				}
				if cfg.Fasti.Heartbeat.DailyAt != "08:15" {
					t.Fatalf("got DailyAt=%q, want 08:15", cfg.Fasti.Heartbeat.DailyAt)
				}
			},
		},
		{
			name: "unknown sub-key under fasti fails KnownFields",
			extra: `fasti:
  enabled: true
  bogus_sub_key: 42
`,
			wantErr: "field bogus_sub_key not found",
		},
		{
			name: "unknown sub-key under fasti.heartbeat fails KnownFields",
			extra: `fasti:
  heartbeat:
    bogus_hb: "07:30"
`,
			wantErr: "field bogus_hb not found",
		},
		{
			name: "invalid catchup_window fails validation",
			extra: `fasti:
  catchup_window: 0s
`,
			wantErr: "catchup_window",
		},
		{
			name: "invalid daily_at fails validation",
			extra: `fasti:
  heartbeat:
    daily_at: "25:00"
`,
			wantErr: "daily_at",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "lararium.yaml")
			if err := os.WriteFile(p, []byte(baseYAML+tc.extra), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(p)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got err=%v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig failed: %v", err)
			}
			if tc.check != nil {
				tc.check(t, cfg)
			}
		})
	}
}

func TestFastiDisabledSurfaceAndSocket(t *testing.T) {
	home := t.TempDir()
	cfg := fasti.DefaultConfig() // Enabled: false

	eng, holder, stopFasti, err := startFasti(context.Background(), cfg, home, nil, nil)
	if err != nil {
		t.Fatalf("startFasti(disabled) returned error: %v", err)
	}
	if eng != nil || holder != nil || stopFasti != nil {
		t.Fatalf("expected all nil when fasti is disabled, got eng=%v holder=%v", eng, holder)
	}

	store, err := surface.OpenTokenStore(filepath.Join(home, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	rawTok, err := store.Create("test")
	if err != nil {
		t.Fatal(err)
	}

	srv := &surface.Server{
		Cfg:   surface.ServeConfig{Listen: "127.0.0.1:0"},
		Store: store,
		Fasti: eng, // nil
	}
	ts := httptest.NewServer(surface.NewServer(srv))
	defer ts.Close()

	httpCases := []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/v1/fasti/state"},
		{method: http.MethodPost, path: "/v1/fasti/jobs", body: `{"name":"j1","prompt":"hi","schedule":{"kind":"interval","every":"5m"}}`},
	}

	for _, hc := range httpCases {
		req, err := http.NewRequest(hc.method, ts.URL+hc.path, strings.NewReader(hc.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+rawTok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		resp.Body.Close()

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: got status %d, want 503", hc.method, hc.path, resp.StatusCode)
		}
		if errBody.Error != "fasti_disabled" {
			t.Fatalf("%s %s: got error %q, want fasti_disabled", hc.method, hc.path, errBody.Error)
		}
	}

	sockPath := filepath.Join(home, "hearthd.sock")
	sock, err := srv.ServeSocket(sockPath, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer sock.Close()

	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(`FASTI {"op":"state"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "ERR fasti_disabled" {
		t.Fatalf("socket reply = %q, want %q", strings.TrimSpace(line), "ERR fasti_disabled")
	}
}

type turnDoorCall struct {
	sessionID string
	text      string
	channel   string
	updateID  int64
}

type spyTurnDoor struct {
	mu      sync.Mutex
	calls   []turnDoorCall
	callCh  chan turnDoorCall
	deltas  []string
	retErr  error
	runHook func(ctx context.Context, call turnDoorCall, onDelta func(string)) error
}

func newSpyTurnDoor(deltas ...string) *spyTurnDoor {
	return &spyTurnDoor{
		callCh: make(chan turnDoorCall, 32),
		deltas: deltas,
	}
}

func (s *spyTurnDoor) RunHeadlessTurn(
	ctx context.Context,
	sessionID, text, channel string,
	updateID int64,
	onDelta func(delta string),
) error {
	call := turnDoorCall{
		sessionID: sessionID,
		text:      text,
		channel:   channel,
		updateID:  updateID,
	}
	s.mu.Lock()
	s.calls = append(s.calls, call)
	hook := s.runHook
	deltas := append([]string(nil), s.deltas...)
	retErr := s.retErr
	s.mu.Unlock()

	if hook != nil {
		err := hook(ctx, call, onDelta)
		select {
		case s.callCh <- call:
		default:
		}
		return err
	}

	if onDelta != nil {
		for _, d := range deltas {
			onDelta(d)
		}
	}
	select {
	case s.callCh <- call:
	default:
	}
	return retErr
}

func (s *spyTurnDoor) waitCall(t *testing.T) turnDoorCall {
	t.Helper()
	select {
	case c := <-s.callCh:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for RunHeadlessTurn call")
		return turnDoorCall{}
	}
}

type spyAnnouncer struct {
	mu    sync.Mutex
	texts []string
	ch    chan string
}

func newSpyAnnouncer() *spyAnnouncer {
	return &spyAnnouncer{ch: make(chan string, 16)}
}

func (a *spyAnnouncer) Announce(_ context.Context, text string) error {
	a.mu.Lock()
	a.texts = append(a.texts, text)
	a.mu.Unlock()
	select {
	case a.ch <- text:
	default:
	}
	return nil
}

func TestFastiWiringTurnDoorChannels(t *testing.T) {
	home := t.TempDir()
	hbContent := `---
enabled: true
cadence: hourly
---
- verify backups
`
	if err := os.WriteFile(filepath.Join(home, "HEARTBEAT.md"), []byte(hbContent), 0o600); err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 6, 1, 10, 10, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)
	door := newSpyTurnDoor("  hello ", "world\n")

	cfg := fasti.DefaultConfig()
	cfg.Enabled = true

	eng, holder, stopFasti, err := startFasti(context.Background(), cfg, home, door, clk, fasti.WithLocation(time.UTC))
	if err != nil {
		t.Fatalf("startFasti: %v", err)
	}
	defer stopFasti()

	ann := newSpyAnnouncer()
	holder.Set(ann)

	// 1. Job due in 5m -> reaches door with "cron" channel and job session, trimmed deltas announced.
	job, err := eng.AddJob(context.Background(), fasti.Job{
		ID:       "job-5m",
		Name:     "five-min-check",
		Schedule: fasti.Schedule{Kind: "interval", Every: "5m"},
		Prompt:   "check status",
		Session:  "ops-sess",
	})
	if err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	clk.Advance(5 * time.Minute) // 10:15:00Z
	call := door.waitCall(t)
	if call.channel != "cron" || call.sessionID != "ops-sess" || call.text != "check status" {
		t.Fatalf("job door call = %+v, want channel=cron sessionID=ops-sess text='check status'", call)
	}
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case gotAnn := <-ann.ch:
		if gotAnn != "hello world" {
			t.Fatalf("announced text = %q, want %q", gotAnn, "hello world")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected announcement from job turn deltas")
	}

	// Pause the job so it doesn't fire while testing heartbeat & goal.
	if err := eng.PauseJob(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}

	// 2. Heartbeat wake at top of hour (11:00:00Z) -> reaches door with "heartbeat" channel & "heartbeat" session.
	clk.Advance(45 * time.Minute) // 11:00:00Z
	hbCall := door.waitCall(t)
	if hbCall.channel != "heartbeat" || hbCall.sessionID != "heartbeat" {
		t.Fatalf("heartbeat door call = %+v, want channel=heartbeat sessionID=heartbeat", hbCall)
	}
	if !strings.Contains(hbCall.text, "verify backups") {
		t.Fatalf("heartbeat prompt missing checklist: %q", hbCall.text)
	}
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 3. Goal step -> arms immediately at AddGoal and reaches door with "goal" channel.
	goal, err := eng.AddGoal(context.Background(), fasti.GoalSpec{
		ID:           "goal-1",
		Title:        "deploy v2",
		Requirements: []string{"smoke test passes"},
		SessionID:    "goal-sess",
		StepAfter:    10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("AddGoal: %v", err)
	}
	clk.Advance(0)
	goalCall := door.waitCall(t)
	if goalCall.channel != "goal" || goalCall.sessionID != "goal-sess" {
		t.Fatalf("goal door call = %+v, want channel=goal sessionID=goal-sess", goalCall)
	}
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := eng.PauseGoal(context.Background(), goal.ID); err != nil {
		t.Fatal(err)
	}

	// 4. ErrTurnInFlight sentinel mapping: surface.ErrTurnInFlight increments ConsecutiveSkips.
	door.mu.Lock()
	door.retErr = surface.ErrTurnInFlight
	door.mu.Unlock()

	if err := eng.ResumeJob(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	clk.Advance(5 * time.Minute)
	_ = door.waitCall(t)
	if err := eng.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	updatedJob, err := eng.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedJob.ConsecutiveSkips != 1 {
		t.Fatalf("ConsecutiveSkips = %d, want 1 after surface.ErrTurnInFlight", updatedJob.ConsecutiveSkips)
	}
}

func readSSEEvent(t *testing.T, ch <-chan fasti.Event, match func(fasti.Event) bool) fasti.Event {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("SSE event channel closed unexpectedly")
			}
			if match(ev) {
				return ev
			}
		case <-timeout:
			t.Fatal("timed out waiting for matching SSE event")
		}
	}
}

func decodeEventRow[T any](ev fasti.Event, wantKind string) (T, bool) {
	var zero T
	if ev.Kind != wantKind || ev.Op != "upsert" {
		return zero, false
	}
	raw, err := json.Marshal(ev.Row)
	if err != nil {
		return zero, false
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, false
	}
	return out, true
}

func connectFastiSSE(t *testing.T, home string, eng *fasti.Engine) <-chan fasti.Event {
	t.Helper()
	store, err := surface.OpenTokenStore(filepath.Join(home, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	rawTok, err := store.Create("sse-client")
	if err != nil {
		t.Fatal(err)
	}

	srv := &surface.Server{
		Cfg:   surface.ServeConfig{Listen: "127.0.0.1:0"},
		Store: store,
		Fasti: eng,
	}
	ts := httptest.NewServer(surface.NewServer(srv))
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/fasti/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+rawTok)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/fasti/events status = %d, want 200", resp.StatusCode)
	}

	sseCh := make(chan fasti.Event, 64)
	go func() {
		defer close(sseCh)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev fasti.Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err == nil {
				sseCh <- ev
			}
		}
	}()
	return sseCh
}

func TestFastiLoopSideSSE(t *testing.T) {
	home := t.TempDir()
	hbInitial := `---
enabled: true
cadence: hourly
quiet: "22:00-06:00"
---
- initial check
`
	hbPath := filepath.Join(home, "HEARTBEAT.md")
	if err := os.WriteFile(hbPath, []byte(hbInitial), 0o600); err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)
	door := newSpyTurnDoor("HEARTBEAT_OK")

	cfg := fasti.DefaultConfig()
	cfg.Enabled = true

	eng, _, stopFasti, err := startFasti(context.Background(), cfg, home, door, clk, fasti.WithLocation(time.UTC))
	if err != nil {
		t.Fatalf("startFasti: %v", err)
	}
	defer stopFasti()

	// Seed a job (every 5m) before connecting SSE.
	seededJob, err := eng.AddJob(context.Background(), fasti.Job{
		ID:       "sse-job-1",
		Name:     "sse-job",
		Schedule: fasti.Schedule{Kind: "interval", Every: "5m"},
		Prompt:   "run job",
		Session:  "sess-job",
	})
	if err != nil {
		t.Fatal(err)
	}

	sseCh := connectFastiSSE(t, home, eng)

	// Wait for bootstrap heartbeat event so we know the SSE stream is connected.
	_ = readSSEEvent(t, sseCh, func(ev fasti.Event) bool {
		return ev.Kind == "heartbeat" && ev.Op == "upsert"
	})

	// (a) Advance FakeClock past job due with NO HTTP/CLI mutation -> assert job SSE upsert arrives.
	clk.Advance(5 * time.Minute) // 10:05:00Z
	_ = door.waitCall(t)

	var gotJob fasti.Job
	_ = readSSEEvent(t, sseCh, func(ev fasti.Event) bool {
		j, ok := decodeEventRow[fasti.Job](ev, "job")
		if ok && j.ID == seededJob.ID && j.LastFire != "" && j.BudgetCounters.TotalRuns == 1 {
			gotJob = j
			return true
		}
		return false
	})
	if gotJob.NextDue != "2026-06-01T10:10:00Z" {
		t.Fatalf("job SSE NextDue = %q, want 2026-06-01T10:10:00Z", gotJob.NextDue)
	}

	// Pause job so we can isolate goal & heartbeat loop-side SSE events.
	if err := eng.PauseJob(context.Background(), seededJob.ID); err != nil {
		t.Fatal(err)
	}

	// (b) Add a goal with TurnBudget=2 and StepAfter=10m; first step fires immediately on FakeClock.
	_, err = eng.AddGoal(context.Background(), fasti.GoalSpec{
		ID:           "sse-goal-1",
		Title:        "sse goal",
		Requirements: []string{"req 1"},
		TurnBudget:   2,
		StepAfter:    10 * time.Minute,
		SessionID:    "sess-goal",
	})
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(0)
	_ = door.waitCall(t)

	// Wait for loop-side SSE event reflecting TurnCount == 1 (goal turn advance).
	var gotGoal1 fasti.Goal
	_ = readSSEEvent(t, sseCh, func(ev fasti.Event) bool {
		g, ok := decodeEventRow[fasti.Goal](ev, "goal")
		if ok && g.ID == "sse-goal-1" && g.TurnCount == 1 && g.Status == "active" {
			gotGoal1 = g
			return true
		}
		return false
	})
	if gotGoal1.NextDue != "2026-06-01T10:15:00Z" {
		t.Fatalf("goal step 1 SSE NextDue = %q, want 2026-06-01T10:15:00Z", gotGoal1.NextDue)
	}

	// (c) Advance FakeClock 10m with NO HTTP/CLI mutation -> step 2 hits TurnBudget=2 with unmet req -> status transitions to "stale".
	clk.Advance(10 * time.Minute) // 10:15:00Z
	_ = door.waitCall(t)

	_ = readSSEEvent(t, sseCh, func(ev fasti.Event) bool {
		g, ok := decodeEventRow[fasti.Goal](ev, "goal")
		return ok && g.ID == "sse-goal-1" && g.TurnCount == 2 && g.Status == "stale"
	})

	// (d) Edit HEARTBEAT.md on disk before the 11:00:00Z wake boundary, then advance FakeClock to 11:00:00Z.
	hbUpdated := `---
enabled: true
cadence: daily
quiet: "23:00-05:00"
---
- updated check
`
	if err := os.WriteFile(hbPath, []byte(hbUpdated), 0o600); err != nil {
		t.Fatal(err)
	}
	clk.Advance(45 * time.Minute) // 11:00:00Z (hourly wake timer fires and re-parses HEARTBEAT.md)

	_ = readSSEEvent(t, sseCh, func(ev fasti.Event) bool {
		hb, ok := decodeEventRow[fasti.HeartbeatState](ev, "heartbeat")
		return ok && hb.Enabled && hb.Cadence == "daily" && hb.Quiet == "23:00-05:00"
	})

	// (e) Job 5-skip auto-pause (engine.go:1637) publishes paused=true on SSE.
	door.mu.Lock()
	door.retErr = surface.ErrTurnInFlight
	door.mu.Unlock()
	if err := eng.ResumeJob(context.Background(), seededJob.ID); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		clk.Advance(5 * time.Minute)
		_ = door.waitCall(t)
		if err := eng.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	_ = readSSEEvent(t, sseCh, func(ev fasti.Event) bool {
		j, ok := decodeEventRow[fasti.Job](ev, "job")
		return ok && j.ID == seededJob.ID && j.Paused && j.ConsecutiveSkips >= 5
	})
}

func TestFastiStartFailClosedOnCorruptStore(t *testing.T) {
	home := t.TempDir()
	fastiDir := filepath.Join(home, "fasti")
	if err := os.MkdirAll(fastiDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fastiDir, "jobs.json"), []byte("{corrupt json"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := fasti.DefaultConfig()
	cfg.Enabled = true
	eng, holder, stopFasti, err := startFasti(context.Background(), cfg, home, nil, loop.NewFakeClock(time.Now().UTC()))
	if err == nil {
		t.Fatal("expected startFasti to fail-closed on corrupt jobs.json")
	}
	if eng != nil || holder != nil || stopFasti != nil {
		t.Fatalf("expected nil returns on startFasti error, got eng=%v holder=%v", eng, holder)
	}
}

func TestFastiAnnouncerIndirection(t *testing.T) {
	holder := newFastiAnnouncerHolder()

	// 1. Before bridge is set -> returns error, no panic.
	if err := holder.Announce(context.Background(), "early msg"); err == nil {
		t.Fatal("expected error when announcing before bridge is set")
	}

	// 2. Typed-nil *nuntius.Bridge -> returns error, no panic.
	var nilBridge *nuntius.Bridge
	holder.Set(nilBridge)
	if err := holder.Announce(context.Background(), "still nil"); err == nil {
		t.Fatal("expected error when announcing with typed-nil *nuntius.Bridge")
	}

	// 3. Real *nuntius.Bridge backed by fakeTransport and paired owner chat -> delivers message.
	env := setupVSuite(t, vsuiteOpts{
		ownerID: "owner-1",
		chatID:  "chat-42",
	})
	holder.Set(env.bridge)
	if err := holder.Announce(context.Background(), "hello from fasti"); err != nil {
		t.Fatalf("holder.Announce after Set(bridge): %v", err)
	}
	if env.tg.SentCount() != 1 {
		t.Fatalf("fakeTransport SentCount = %d, want 1", env.tg.SentCount())
	}
	sent := env.tg.LastSent()
	if sent.ChatID != "chat-42" || sent.Text != "hello from fasti" {
		t.Fatalf("fakeTransport LastSent = %+v, want ChatID=chat-42 Text='hello from fasti'", sent)
	}
}

func TestFastiShutdownMidTurn(t *testing.T) {
	home := t.TempDir()
	t0 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	clk := loop.NewFakeClock(t0)

	turnStarted := make(chan struct{})
	turnCtxDone := make(chan error, 1)

	door := newSpyTurnDoor()
	door.runHook = func(ctx context.Context, _ turnDoorCall, _ func(string)) error {
		close(turnStarted)
		<-ctx.Done()
		turnCtxDone <- ctx.Err()
		return ctx.Err()
	}

	cfg := fasti.DefaultConfig()
	cfg.Enabled = true

	eng, _, stopFasti, err := startFasti(context.Background(), cfg, home, door, clk, fasti.WithLocation(time.UTC))
	if err != nil {
		t.Fatalf("startFasti: %v", err)
	}

	if _, err := eng.AddJob(context.Background(), fasti.Job{
		ID:       "mid-turn-job",
		Name:     "mid-turn",
		Schedule: fasti.Schedule{Kind: "interval", Every: "5m"},
		Prompt:   "long running prompt",
		Session:  "sess-shutdown",
	}); err != nil {
		t.Fatal(err)
	}

	clk.Advance(5 * time.Minute)
	select {
	case <-turnStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for turn to start")
	}

	// Stop engine mid-turn: must cancel door ctx and return cleanly without panic.
	stopDone := make(chan struct{})
	go func() {
		stopFasti()
		close(stopDone)
	}()

	select {
	case err := <-turnCtxDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("turn ctx err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for door ctx cancellation")
	}

	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stopFasti() to return")
	}
}
