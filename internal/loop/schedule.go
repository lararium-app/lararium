package loop

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

const (
	// MinWakeDelaySeconds is the minimum allowed delay for schedule_wake.
	MinWakeDelaySeconds = 1
	// MaxWakeDelaySeconds is the maximum allowed delay for schedule_wake (1 year).
	MaxWakeDelaySeconds = 31536000
	// MaxPendingWakes caps the number of simultaneously pending wakes.
	MaxPendingWakes = 50

	// ScheduleWakeSchema is the JSON schema registered for the schedule_wake tool.
	ScheduleWakeSchema = `{"type":"object","properties":{"delay_seconds":{"type":"integer","minimum":1,"maximum":31536000,"description":"delay in seconds before firing (1 to 31536000)"},"prompt":{"type":"string","description":"short text prompt to inject when the wake fires"}},"required":["delay_seconds","prompt"]}`
)

type sessionCtxKey struct{}

func withSession(ctx context.Context, s *Session) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionCtxKey{}, s)
}

func sessionFromContext(ctx context.Context) *Session {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(sessionCtxKey{}).(*Session)
	return s
}

// Timer represents a scheduled callback that can be stopped before firing.
type Timer interface {
	Stop() bool
}

// Clock abstracts time and timer creation so tests can use a deterministic
// fake clock without sleeping.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

type realClock struct{}

// Now returns the current UTC time.
func (realClock) Now() time.Time {
	return time.Now().UTC()
}

// AfterFunc returns a Timer that fires f after d has elapsed.
func (realClock) AfterFunc(d time.Duration, f func()) Timer {
	return time.AfterFunc(d, f)
}

// FakeClock is a deterministic, thread-safe fake clock for tests.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clock    *FakeClock
	deadline time.Time
	fn       func()
	stopped  bool
	fired    bool
}

// NewFakeClock creates a FakeClock initialized to initial (converted to UTC).
func NewFakeClock(initial time.Time) *FakeClock {
	if initial.IsZero() {
		initial = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	}
	return &FakeClock{now: initial.UTC()}
}

// Now returns the fake clock's current UTC time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// AfterFunc schedules f to run once the clock advances by at least d.
func (c *FakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d < 0 {
		d = 0
	}
	ft := &fakeTimer{
		clock:    c,
		deadline: c.now.Add(d),
		fn:       f,
	}
	c.timers = append(c.timers, ft)
	return ft
}

// Advance moves the fake clock forward by d and synchronously fires all active
// timers whose deadlines are <= the new current time, in deadline order.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	target := c.now
	c.mu.Unlock()

	for {
		c.mu.Lock()
		var next *fakeTimer
		for _, t := range c.timers {
			if t.stopped || t.fired || t.deadline.After(target) {
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
		next.fired = true
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

// Stop cancels a pending fake timer, reporting whether it had not fired.
func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

// Wake is one persisted scheduled wake entry under the hearth data directory.
type Wake struct {
	ID        string `json:"id"`
	DueAt     string `json:"due_at"`
	SessionID string `json:"session_id"`
	Prompt    string `json:"prompt"`
}

func (w Wake) dueTime() (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, w.DueAt); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, w.DueAt)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// WakeResult is the JSON payload returned by the schedule_wake tool.
type WakeResult struct {
	ID    string `json:"id"`
	DueAt string `json:"due_at"`
}

// TurnRunnerFunc injects a prompt into sessionID as a new agent turn.
type TurnRunnerFunc func(ctx context.Context, sessionID, prompt string) error

// SchedulerOption configures a Scheduler.
type SchedulerOption func(*Scheduler)

// WithClock overrides the scheduler's clock (used by tests).
func WithClock(c Clock) SchedulerOption {
	return func(s *Scheduler) {
		if c != nil {
			s.clock = c
		}
	}
}

// WithTurnFunc sets a custom turn entry point for firing wakes.
func WithTurnFunc(fn TurnRunnerFunc) SchedulerOption {
	return func(s *Scheduler) {
		s.turnFunc = fn
	}
}

// WithTools sets the tool registry and approver used when firing wakes against
// a bound Session.
func WithTools(tools []Tool, ap Approver) SchedulerOption {
	return func(s *Scheduler) {
		s.tools = tools
		s.approver = ap
	}
}

// Scheduler manages persisted scheduled wakes in the daemon.
type Scheduler struct {
	mu       sync.Mutex
	home     string
	sess     *Session
	sessions map[string]*Session
	tools    []Tool
	approver Approver
	turnFunc TurnRunnerFunc
	clock    Clock
	wakes    []Wake
	timer    Timer
	started  bool
	stopped  bool
}

// NewScheduler loads any persisted wakes from home/wakes.json. If a session or
// turnFunc is already wired, it immediately processes any missed wakes that
// slipped while offline and arms the timer for remaining wakes.
func NewScheduler(home string, sess *Session, opts ...SchedulerOption) (*Scheduler, error) {
	s := &Scheduler{
		home:     home,
		sess:     sess,
		sessions: make(map[string]*Session),
		clock:    realClock{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	wakes, err := s.loadFromDisk()
	if err != nil {
		return nil, err
	}
	s.wakes = wakes
	if s.sess != nil || s.turnFunc != nil {
		s.Start(context.Background())
	}
	return s, nil
}

// WakesFile returns the path to the persisted wakes JSON file under the hearth data dir.
func (s *Scheduler) WakesFile() string {
	return filepath.Join(s.home, "wakes.json")
}

// SetTurnFunc configures the turn entry point used when wakes fire.
func (s *Scheduler) SetTurnFunc(fn TurnRunnerFunc) {
	s.mu.Lock()
	s.turnFunc = fn
	s.mu.Unlock()
}

// SetSession binds a Session, tool list, and approver for firing turns.
func (s *Scheduler) SetSession(sess *Session, tools []Tool, ap Approver) {
	s.mu.Lock()
	s.sess = sess
	s.tools = tools
	s.approver = ap
	s.mu.Unlock()
}

// Start processes any wakes whose due time passed while the daemon was offline
// (firing each once immediately and writing an offline-startup audit event) and
// arms the timer for future pending wakes.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	alreadyStarted := s.started
	s.started = true

	var missed []Wake
	if !alreadyStarted && len(s.wakes) > 0 {
		now := s.clock.Now().UTC()
		var remaining []Wake
		for _, w := range s.wakes {
			due, err := w.dueTime()
			if err != nil || !due.After(now) {
				missed = append(missed, w)
			} else {
				remaining = append(remaining, w)
			}
		}
		if len(missed) > 0 {
			s.wakes = remaining
			_ = s.saveLocked()
		}
	}
	s.mu.Unlock()

	for _, w := range missed {
		s.fireWake(ctx, w, true)
	}

	s.mu.Lock()
	if !s.stopped {
		s.rearmLocked()
	}
	s.mu.Unlock()
}

// Stop halts the scheduler's in-process timer. Pending wakes remain on disk for
// the next daemon start.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
}

// Close implements io.Closer by stopping the scheduler.
func (s *Scheduler) Close() error {
	s.Stop()
	return nil
}

// Pending returns a snapshot of the currently pending wakes.
func (s *Scheduler) Pending() []Wake {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Wake, len(s.wakes))
	copy(out, s.wakes)
	return out
}

// Schedule validates and persists a new wake, appends a schedule_created event
// to the session's events.jsonl trail, and arms the timer.
func (s *Scheduler) Schedule(ctx context.Context, sessionID string, delaySeconds int, prompt string) (Wake, error) {
	if delaySeconds < MinWakeDelaySeconds || delaySeconds > MaxWakeDelaySeconds {
		return Wake{}, fmt.Errorf("delay_seconds must be between %d and %d (got %d)", MinWakeDelaySeconds, MaxWakeDelaySeconds, delaySeconds)
	}
	trimmedPrompt := strings.TrimSpace(prompt)
	if trimmedPrompt == "" {
		return Wake{}, errors.New("prompt must not be empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.wakes) >= MaxPendingWakes {
		return Wake{}, fmt.Errorf("cannot schedule wake: maximum of %d pending wakes reached", MaxPendingWakes)
	}

	ctxSess := sessionFromContext(ctx)
	if sessionID == "" {
		sessionID = s.resolveSessionIDLocked(ctxSess)
	}
	if ctxSess != nil && s.turnFunc == nil {
		s.sessions[sessionID] = ctxSess
		if s.sess == nil {
			s.sess = ctxSess
		}
	}

	now := s.clock.Now().UTC()
	due := now.Add(time.Duration(delaySeconds) * time.Second)
	w := Wake{
		ID:        newWakeID(),
		DueAt:     due.Format(time.RFC3339),
		SessionID: sessionID,
		Prompt:    trimmedPrompt,
	}

	s.wakes = append(s.wakes, w)
	if err := s.saveLocked(); err != nil {
		s.wakes = s.wakes[:len(s.wakes)-1]
		return Wake{}, fmt.Errorf("persist wake: %w", err)
	}

	if err := s.appendSessionEventLocked(ctxSess, sessionID, "schedule_created", now, map[string]json.RawMessage{
		"id":            raw(w.ID),
		"wake_id":       raw(w.ID),
		"session_id":    raw(w.SessionID),
		"delay_seconds": mustJSON(delaySeconds),
		"due_at":        raw(w.DueAt),
		"prompt":        raw(w.Prompt),
	}); err != nil {
		return Wake{}, fmt.Errorf("log schedule_created: %w", err)
	}

	if !s.stopped {
		s.started = true
		s.rearmLocked()
	}

	return w, nil
}

// Tool returns the model-callable schedule_wake loop.Tool bound to this Scheduler.
func (s *Scheduler) Tool() Tool {
	return ScheduleWakeTool(s)
}

// ScheduleWakeTool constructs the model-callable schedule_wake tool registered
// the same way as existing loop tools.
func ScheduleWakeTool(sched *Scheduler) Tool {
	return Tool{
		Spec: router.ToolSpec{
			Name:             "schedule_wake",
			Description:      "Schedule a delayed wake that injects a prompt into the session as a new agent turn when due.",
			ParamsJSONSchema: ScheduleWakeSchema,
		},
		Trusted: true,
		Run: func(ctx context.Context, argsJSON string) (string, error) {
			if sched == nil {
				return "", errors.New("scheduler is not configured")
			}
			var a struct {
				DelaySeconds int    `json:"delay_seconds"`
				Prompt       string `json:"prompt"`
			}
			if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
				return "", fmt.Errorf("bad args: %w", err)
			}
			w, err := sched.Schedule(ctx, "", a.DelaySeconds, a.Prompt)
			if err != nil {
				return "", err
			}
			res, err := json.Marshal(WakeResult{
				ID:    w.ID,
				DueAt: w.DueAt,
			})
			if err != nil {
				return "", err
			}
			return string(res), nil
		},
	}
}

func (s *Scheduler) rearmLocked() {
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if s.stopped || len(s.wakes) == 0 {
		return
	}
	now := s.clock.Now().UTC()
	var earliest time.Time
	for i, w := range s.wakes {
		due, err := w.dueTime()
		if err != nil {
			due = now
		}
		if i == 0 || due.Before(earliest) {
			earliest = due
		}
	}
	delay := earliest.Sub(now)
	if delay < 0 {
		delay = 0
	}
	s.timer = s.clock.AfterFunc(delay, s.onTimer)
}

func (s *Scheduler) onTimer() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	now := s.clock.Now().UTC()
	var dueWakes []Wake
	var remaining []Wake
	for _, w := range s.wakes {
		due, err := w.dueTime()
		if err != nil || !due.After(now) {
			dueWakes = append(dueWakes, w)
		} else {
			remaining = append(remaining, w)
		}
	}
	if len(dueWakes) > 0 {
		s.wakes = remaining
		_ = s.saveLocked()
	}
	s.mu.Unlock()

	sort.SliceStable(dueWakes, func(i, j int) bool {
		di, _ := dueWakes[i].dueTime()
		dj, _ := dueWakes[j].dueTime()
		return di.Before(dj)
	})

	for _, w := range dueWakes {
		s.fireWake(context.Background(), w, false)
	}

	s.mu.Lock()
	if !s.stopped {
		s.rearmLocked()
	}
	s.mu.Unlock()
}

func (s *Scheduler) fireWake(ctx context.Context, w Wake, missedOffline bool) {
	s.mu.Lock()
	now := s.clock.Now().UTC()
	turnFunc := s.turnFunc
	sess := s.sessionForLocked(w.SessionID)
	tools := s.tools
	ap := s.approver

	// If turnFunc is set without a persistent bound Session (e.g., Hub.RunHeadlessTurn
	// in hearthd serve), Hub opens a fresh penatus.Log from disk for each turn, so we
	// append schedule_fired to disk before invoking turnFunc. Otherwise, we append
	// through sess.Log.
	var eventSess *Session
	if turnFunc == nil || s.sess != nil {
		eventSess = sess
	}
	fields := map[string]json.RawMessage{
		"id":         raw(w.ID),
		"wake_id":    raw(w.ID),
		"session_id": raw(w.SessionID),
		"due_at":     raw(w.DueAt),
		"fired_at":   raw(now.Format(time.RFC3339)),
		"prompt":     raw(w.Prompt),
		"missed":     mustJSON(missedOffline),
	}
	if missedOffline {
		fields["offline"] = mustJSON(true)
		fields["reason"] = raw("slipped_while_offline")
		fields["note"] = raw("wake slipped while offline; fired on startup")
	}
	_ = s.appendSessionEventLocked(eventSess, w.SessionID, "schedule_fired", now, fields)
	s.mu.Unlock()

	if turnFunc != nil {
		_ = turnFunc(ctx, w.SessionID, w.Prompt)
		return
	}
	if sess != nil {
		if len(tools) > 0 {
			_, _ = sess.RunTurnTools(ctx, w.Prompt, nil, tools, ap)
		} else {
			_, _ = sess.RunTurn(ctx, w.Prompt, nil)
		}
	}
}

func (s *Scheduler) resolveSessionIDLocked(ctxSess *Session) string {
	if ctxSess != nil && ctxSess.Log != nil {
		base := filepath.Base(filepath.Clean(ctxSess.Log.Dir()))
		if base != "" && base != "." && base != "/" {
			return base
		}
	}
	if s.sess != nil && s.sess.Log != nil {
		base := filepath.Base(filepath.Clean(s.sess.Log.Dir()))
		if base != "" && base != "." && base != "/" {
			return base
		}
	}
	return "main"
}

func (s *Scheduler) sessionForLocked(sessionID string) *Session {
	if sessionID != "" {
		if sess, ok := s.sessions[sessionID]; ok && sess != nil {
			return sess
		}
	}
	return s.sess
}

func (s *Scheduler) appendSessionEventLocked(sess *Session, sessionID, eventType string, ts time.Time, fields map[string]json.RawMessage) error {
	ev := penatus.Event{
		T:      eventType,
		TS:     ts.UTC().Format(time.RFC3339),
		Fields: fields,
	}
	if sess != nil && sess.Log != nil {
		return sess.Log.Append(ev)
	}
	if s.home == "" {
		return nil
	}
	sessDir := resolveSessionDir(s.home, sessionID)
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		return err
	}
	log, err := penatus.OpenLog(sessDir)
	if err != nil {
		return err
	}
	return log.Append(ev)
}

func resolveSessionDir(home, sessionID string) string {
	if sessionID == "" {
		sessionID = "main"
	}
	sessSub := filepath.Join(home, "sessions", sessionID)
	if info, err := os.Stat(sessSub); err == nil && info.IsDir() {
		return sessSub
	}
	if _, err := os.Stat(filepath.Join(home, "events.jsonl")); err == nil {
		return home
	}
	if _, err := os.Stat(filepath.Join(home, "session.json")); err == nil {
		return home
	}
	return sessSub
}

func (s *Scheduler) loadFromDisk() ([]Wake, error) {
	if s.home == "" {
		return nil, nil
	}
	data, err := os.ReadFile(s.WakesFile())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read wakes file: %w", err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, nil
	}
	var wakes []Wake
	if err := json.Unmarshal([]byte(trimmed), &wakes); err == nil {
		return wakes, nil
	}
	// Fallback: support one JSON wake object per line (JSONL).
	scanner := bufio.NewScanner(strings.NewReader(trimmed))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var w Wake
		if err := json.Unmarshal([]byte(line), &w); err != nil {
			return nil, fmt.Errorf("parse wakes file: %w", err)
		}
		wakes = append(wakes, w)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan wakes file: %w", err)
	}
	return wakes, nil
}

func (s *Scheduler) saveLocked() error {
	if s.home == "" {
		return nil
	}
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		return err
	}
	out := s.wakes
	if out == nil {
		out = []Wake{}
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.home, "wakes-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, s.WakesFile())
}

func newWakeID() string {
	return "w_" + strings.TrimPrefix(penatus.NewID(), "s_")
}
