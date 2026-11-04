package fasti

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

// ErrTurnInFlight is returned when a turn is already running for the session.
// fasti must not import internal/surface; this mirrors turns.go:24.
var ErrTurnInFlight = errors.New("turn in flight")

// ErrJobNotFound indicates that a requested job ID does not exist in the store.
var ErrJobNotFound = errors.New("job not found")

// ErrPromptWhileEnabled refuses prompt edits on a live job row: the edit path
// is pause → edit → resume (spec D9 "A cron prompt is immutable while
// enabled"; D5 M11 keeps Paused distinct from Enabled, so a paused row is
// off the schedule and freely editable).
var ErrPromptWhileEnabled = errors.New("cannot update prompt while job is enabled")

// TurnRunner runs a headless turn for sessionID with prompt.
// Implementations return ErrTurnInFlight if a turn is already running for sessionID
// (such as Hub.RunHeadlessTurn in internal/surface/turns.go:491).
type TurnRunner func(ctx context.Context, sessionID, prompt string) error

// Announcer defines the interface for delivering announcements (spec D7).
type Announcer interface {
	Announce(ctx context.Context, text string) error
}

// TextTurnRunner runs a headless turn and returns candidate announcement text (spec D2/D7).
type TextTurnRunner func(ctx context.Context, sessionID, prompt string) (string, error)

// EngineOption configures a fasti Engine.
type EngineOption func(*Engine)

// WithClock overrides the injected Clock (used by tests with FakeClock).
func WithClock(c loop.Clock) EngineOption {
	return func(e *Engine) {
		if c != nil {
			e.clock = c
		}
	}
}

// WithRunner sets the turn runner for executing headless turns.
func WithRunner(r TurnRunner) EngineOption {
	return func(e *Engine) {
		e.runner = r
	}
}

// WithAnnouncer sets the announcer for external message delivery (spec D7).
func WithAnnouncer(a Announcer) EngineOption {
	return func(e *Engine) {
		e.announcer = a
	}
}

// WithTextRunner sets the text-returning turn runner (spec D2/D7).
func WithTextRunner(r TextTurnRunner) EngineOption {
	return func(e *Engine) {
		e.textRunner = r
	}
}

// WithLocation sets the local timezone used for next-due calculation.
func WithLocation(loc *time.Location) EngineOption {
	return func(e *Engine) {
		if loc != nil {
			e.loc = loc
		}
	}
}

// WithSessionLog sets the session log interface used for goal high-water stamps and sequence checks (spec D10/R7, F2-4).
func WithSessionLog(sl SessionLog) EngineOption {
	return func(e *Engine) {
		e.sessionLog = sl
	}
}

// Engine implements the fasti proactivity scheduler (spec D1-D5, F1-1..F1-6).
type Engine struct {
	cfg        Config
	home       string
	clock      loop.Clock
	runner     TurnRunner
	textRunner TextTurnRunner
	announcer  Announcer
	loc        *time.Location
	sessionLog SessionLog

	jobsPath  string
	goalsPath string
	statePath string
	journal   *Journal

	// Concurrency channels:
	cmdCh        chan any
	workerCh     chan firingTask
	workerDoneCh chan firingResult
	stopCh       chan struct{}
	stopped      bool
	stopOnce     sync.Once
	loopWg       sync.WaitGroup
	workerWg     sync.WaitGroup

	ctx       context.Context //nolint:containedctx // engine-scoped lifecycle ctx: the worker honors ctx.Err() for D13 shutdown
	cancelCtx context.CancelFunc

	// State owned strictly by the command loop goroutine:
	jobs                   []Job
	goals                  []Goal
	noiseState             *NoiseState
	activeTimers           map[string]loop.Timer
	retryTimers            map[string]loop.Timer
	goalTimers             map[string]loop.Timer
	goalRetryTimers        map[string]loop.Timer
	quietCollapseTimers    map[string]loop.Timer
	quietCollapses         map[string]quietCollapseEntry
	heartbeatTimer         loop.Timer
	heartbeatIdleTimer     loop.Timer
	heartbeatRetryTimer    loop.Timer
	heartbeatCollapseTimer loop.Timer
	heartbeatCollapse      *quietCollapseEntry
	lastHeartbeat          *HeartbeatState
	activeStep             *activeGoalStep
	workerBusy             bool
	currentFiring          *firingTask
}

type firingTask struct {
	jobID       string
	goalID      string
	isGoal      bool
	isHeartbeat bool
	isNotice    bool
	noticeKind  string
	sessionID   string
	prompt      string
	dueAt       time.Time
	reason      string
	quietWindow string
	quietMode   string
}

func (t firingTask) kind() string {
	if t.isHeartbeat {
		return "heartbeat"
	}
	if t.isGoal {
		return "goal"
	}
	return "cron"
}

type quietCollapseEntry struct {
	jobID       string
	isHeartbeat bool
	prompt      string
	windowEnd   time.Time
}

type activeGoalStep struct {
	goalID      string
	sessionID   string
	windowStart int64
	windowEnd   int64
}

type firingResult struct {
	task             firingTask
	err              error
	replyText        string
	announceDecision *announceDecision
}

// Command types handled by the command loop.
type (
	cmdAddJob struct {
		job  Job
		resp chan addJobResp
	}
	addJobResp struct {
		job Job
		err error
	}

	cmdRemoveJob struct {
		id   string
		resp chan error
	}

	cmdPauseJob struct {
		id   string
		resp chan error
	}

	cmdResumeJob struct {
		id   string
		resp chan error
	}

	cmdRunJob struct {
		id   string
		resp chan error
	}

	cmdEditJob struct {
		id       string
		prompt   *string
		schedule *Schedule
		resp     chan editJobResp
	}

	editJobResp struct {
		job Job
		err error
	}

	cmdGetJob struct {
		id   string
		resp chan getJobResp
	}
	getJobResp struct {
		job Job
		err error
	}

	cmdListJobs struct {
		resp chan []Job
	}

	cmdJobDue struct {
		jobID      string
		due        time.Time
		isGap      bool
		skippedDue time.Time
	}

	cmdDispatchTask struct {
		task firingTask
	}

	cmdShutdown struct {
		resp chan struct{}
	}

	cmdJobRetry struct {
		jobID       string
		originalDue time.Time
	}

	cmdSync struct {
		resp chan struct{}
	}

	cmdAddGoal struct {
		spec GoalSpec
		resp chan addGoalResp
	}
	addGoalResp struct {
		goal Goal
		err  error
	}

	cmdPauseGoal struct {
		id   string
		resp chan error
	}

	cmdResumeGoal struct {
		id   string
		resp chan error
	}

	cmdCancelGoal struct {
		id   string
		resp chan error
	}

	cmdSetGoalBudget struct {
		id     string
		budget int
		resp   chan error
	}

	cmdForceCloseGoal struct {
		id   string
		resp chan error
	}

	cmdGetGoal struct {
		id   string
		resp chan getGoalResp
	}
	getGoalResp struct {
		goal Goal
		err  error
	}

	cmdListGoals struct {
		resp chan []Goal
	}

	cmdGoalStep struct {
		goalID        string
		nextHint      string
		progressProse string
		satisfiedReq  int
		resp          chan error
	}

	cmdGoalClose struct {
		goalID string
		resp   chan error
	}

	cmdGoalDue struct {
		goalID string
		due    time.Time
	}

	cmdGoalRetry struct {
		goalID      string
		originalDue time.Time
	}

	cmdCheckAnnounce struct {
		task firingTask
		text string
		resp chan announceDecision
	}
	announceDecision struct {
		allowed    bool
		status     string
		reason     string
		noticeText string
	}

	cmdQuietCollapseDue struct {
		jobID string
	}

	cmdHeartbeatDue struct {
		due time.Time
	}

	cmdHeartbeatIdleTick struct{}

	cmdHeartbeatRetry struct {
		originalDue time.Time
	}

	cmdHeartbeatCollapseDue struct{}

	cmdNoiseSnapshot struct {
		resp chan NoiseSnapshot
	}

	cmdCheckEditDedup struct {
		id   string
		hash string
		resp chan bool
	}

	cmdRecordEditDedup struct {
		id   string
		hash string
		resp chan error
	}
)

// New creates and initializes a new fasti Engine.
func New(cfg Config, home string, opts ...EngineOption) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	loc, err := cfg.Location()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	e := &Engine{
		cfg:                 cfg,
		home:                home,
		loc:                 loc,
		jobsPath:            JobsFilePath(home),
		goalsPath:           GoalsFilePath(home),
		statePath:           NoiseStateFilePath(home),
		cmdCh:               make(chan any, 128),
		workerCh:            make(chan firingTask, 1), // Depth 1 worker mailbox (D3)
		workerDoneCh:        make(chan firingResult, 16),
		stopCh:              make(chan struct{}),
		ctx:                 ctx,
		cancelCtx:           cancel,
		activeTimers:        make(map[string]loop.Timer),
		retryTimers:         make(map[string]loop.Timer),
		goalTimers:          make(map[string]loop.Timer),
		goalRetryTimers:     make(map[string]loop.Timer),
		quietCollapseTimers: make(map[string]loop.Timer),
		quietCollapses:      make(map[string]quietCollapseEntry),
	}

	for _, opt := range opts {
		opt(e)
	}

	if e.clock == nil {
		return nil, errors.New("a Clock must be provided (e.g. WithClock)")
	}

	// Initialize journal
	j, err := OpenJournal(filepath.Join(home, "fasti"), e.clock)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open journal: %w", err)
	}
	e.journal = j

	// Load jobs from store
	jobs, err := LoadJobs(e.jobsPath)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("load jobs: %w", err)
	}
	e.jobs = jobs

	// Load goals from store (spec D10, F2-1)
	goals, err := LoadGoals(e.goalsPath)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("load goals: %w", err)
	}
	e.goals = goals

	// Load noise state from store (spec D7, R2)
	noiseState, err := LoadNoiseState(e.statePath, e.clock.Now().UTC())
	if err != nil {
		cancel()
		return nil, fmt.Errorf("load noise state: %w", err)
	}
	e.noiseState = noiseState

	hb := ReadHeartbeatInfo(home)
	e.lastHeartbeat = &HeartbeatState{
		Enabled: hb.Enabled,
		Cadence: hb.Cadence,
		Quiet:   hb.Quiet,
	}

	return e, nil
}

func cleanAbandonedTempFiles(dir string) {
	patterns := []string{"jobs.json-*.tmp", "events.jsonl-*.tmp", "goals.json-*.tmp", "state.json-*.tmp"}
	for _, p := range patterns {
		matches, err := filepath.Glob(filepath.Join(dir, p))
		if err == nil {
			for _, m := range matches {
				_ = os.Remove(m)
			}
		}
	}
}

// Start boots the engine, processes offline replay (V-Pb, F1-6), arms pending timers,
// and starts the command loop and single firing worker goroutines.
func (e *Engine) Start(ctx context.Context) error {
	// Spec m14: clean up abandoned temporary files from crashed writers at boot
	cleanAbandonedTempFiles(filepath.Join(e.home, "fasti"))

	now := e.clock.Now().UTC()

	// Boot sweep (spec D5 terminal-state law): remove unsettled model-proposed rows whose cards disappeared on restart.
	e.sweepUnsettledProposalsLocked()

	// Process offline replay per V-Pb and D7 downtime collapse
	var missedJobs []Job
	for i := range e.jobs {
		job := &e.jobs[i]
		if !job.Enabled || job.Paused {
			continue
		}

		if job.NextDue == "" {
			res, err := e.calculateNextDue(job, now)
			if err == nil {
				job.NextDue = res.Due.Format(time.RFC3339)
			}
			continue
		}

		dueTime, err := time.Parse(time.RFC3339, job.NextDue)
		if err != nil {
			continue
		}

		if !dueTime.After(now) {
			// Missed while offline!
			if now.Sub(dueTime) > e.cfg.CatchupWindow {
				// Past catchup_window: stale discard (D7)
				_, _ = e.journal.Append(JournalEvent{
					Type:      "fasti_fired",
					Status:    "skipped",
					Reason:    "stale_discard",
					JobID:     job.ID,
					JobName:   job.Name,
					DueAt:     job.NextDue,
					SessionID: job.Session,
				})
			} else {
				// Replay EXACTLY ONCE with slipped_while_offline (D7, V-Pb)
				// Boot precedence (spec D7): if boot lands inside an active defers quiet window,
				// the downtime offline-replay WAITS for the window end and merges with the window's dues
				// into the single quiet_collapse firing (most-recent prompt wins). Downtime never bypasses quiet hours.
				qw, qm := EffectiveQuiet(job, e.cfg.QuietHours)
				if qw != "" && IsInQuietWindow(qw, now, e.loc) && qm == "defers" {
					wEnd := QuietWindowEnd(qw, now, e.loc)
					// Journal law: every wake leaves a trace — a deferred
					// offline replay is recorded before it waits (D7).
					_, _ = e.journal.Append(JournalEvent{
						Type:      "fasti_fired",
						Status:    "deferred",
						Reason:    "quiet_hours",
						JobID:     job.ID,
						JobName:   job.Name,
						Prompt:    job.Prompt,
						SessionID: job.Session,
						DueAt:     job.NextDue,
					})
					e.quietCollapses[job.ID] = quietCollapseEntry{
						jobID:     job.ID,
						prompt:    job.Prompt,
						windowEnd: wEnd,
					}
					delay := wEnd.Sub(now)
					if delay < 0 {
						delay = 0
					}
					jobID := job.ID
					e.quietCollapseTimers[jobID] = e.clock.AfterFunc(delay, func() {
						e.sendCommand(cmdQuietCollapseDue{jobID: jobID})
					})
				} else {
					missedJobs = append(missedJobs, *job)
				}
			}

			// Recompute next future due strictly after now
			res, err := e.calculateNextDue(job, now)
			if err == nil {
				job.NextDue = res.Due.Format(time.RFC3339)
			}
		}
	}

	// Persist updated jobs state after offline catchup calculation
	if err := SaveJobs(e.jobsPath, e.jobs); err != nil {
		return fmt.Errorf("save jobs on start: %w", err)
	}

	// Goals boot duties (spec D10, F2-6):
	// 1. Regenerate journals from goals.json (missing or stale rendering is a cache miss, not state)
	if err := RegenerateGoalJournals(e.home, e.goals); err != nil {
		return fmt.Errorf("regenerate goal journals on start: %w", err)
	}

	// 2. Run stale-30d sweep (spec D10, F2-5, F2-6)
	e.sweepStaleGoalsLocked(now)

	// 3. Re-arm next_due for active goals per D7 collapse rules (due passed offline fires EXACTLY once)
	var missedGoals []Goal
	for i := range e.goals {
		goal := &e.goals[i]
		if goal.Status != "active" {
			continue
		}

		if goal.NextDue == "" {
			goal.NextDue = now.Format(time.RFC3339)
			missedGoals = append(missedGoals, *goal)
			continue
		}

		dueTime, err := time.Parse(time.RFC3339, goal.NextDue)
		if err != nil {
			continue
		}

		if !dueTime.After(now) {
			if now.Sub(dueTime) > e.cfg.CatchupWindow {
				// Past catchup_window: stale discard (D7)
				_, _ = e.journal.Append(JournalEvent{
					Type:      "fasti_fired",
					Kind:      "goal",
					Status:    "skipped",
					Reason:    "stale_discard",
					GoalID:    goal.ID,
					DueAt:     goal.NextDue,
					SessionID: goal.SessionID,
				})
			} else {
				// Replay EXACTLY ONCE with slipped_while_offline (D7, V-Pb, F2-6)
				missedGoals = append(missedGoals, *goal)
			}

			nextDue := now.Add(goal.StepAfter)
			goal.NextDue = nextDue.Format(time.RFC3339)
		}
	}

	if err := SaveGoals(e.goalsPath, e.goals); err != nil {
		return fmt.Errorf("save goals on start: %w", err)
	}

	// Arm future timers (jobs, goals) BEFORE starting runLoop (Spec B4: no map writes outside loop)
	e.armStartupTimersLocked()

	// Arm heartbeat executor before starting runLoop (spec D6, R8)
	e.armHeartbeatLocked()

	// Start worker goroutine
	e.workerWg.Add(1)
	go e.runWorker() //nolint:contextcheck // worker uses engine-scoped lifecycle ctx (e.ctx)

	// Start command loop goroutine
	e.loopWg.Add(1)
	go e.runLoop()

	// Dispatch missed offline firings via the command mailbox (Spec B4/D3: loop owns all dispatch)
	for _, mj := range missedJobs {
		qw, qm := EffectiveQuiet(&mj, e.cfg.QuietHours)
		task := firingTask{
			jobID:       mj.ID,
			sessionID:   mj.Session,
			prompt:      mj.Prompt,
			dueAt:       now,
			reason:      "slipped_while_offline",
			quietWindow: qw,
			quietMode:   qm,
		}
		e.sendCommand(cmdDispatchTask{task: task})
	}

	// Dispatch missed offline goal firings via the command mailbox (Spec D10, F2-6)
	for _, mg := range missedGoals {
		prompt := BuildGoalStepPrompt(&mg)
		task := firingTask{
			jobID:       mg.ID,
			goalID:      mg.ID,
			isGoal:      true,
			sessionID:   mg.SessionID,
			prompt:      prompt,
			dueAt:       now,
			reason:      "slipped_while_offline",
			quietWindow: e.cfg.QuietHours,
			quietMode:   "announce_only",
		}
		e.sendCommand(cmdDispatchTask{task: task})
	}

	return nil
}

// Stop halts the engine, cancelling active timers and waiting for worker completion.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		// Cancel context to abort any in-flight worker turn
		e.cancelCtx()

		// Spec B4: Stop() sends shutdown command through the mailbox, lets the loop stop its own timers and reply
		respCh := make(chan struct{})
		select {
		case e.cmdCh <- cmdShutdown{resp: respCh}:
			select {
			case <-respCh:
			case <-time.After(10 * time.Second): // 10s shutdown deadline per Spec D13
			}
		case <-time.After(10 * time.Second):
		}

		// Wait for command loop to exit first
		e.loopWg.Wait()

		// Now safely close workerCh since command loop is stopped
		close(e.workerCh)

		// Wait for worker to exit
		e.workerWg.Wait()

		e.stopped = true
	})
}

// Close implements io.Closer by stopping the engine.
func (e *Engine) Close() error {
	e.Stop()
	return nil
}

// Journal returns the active run journal.
func (e *Engine) Journal() *Journal {
	return e.journal
}

// runWorker is the single firing worker goroutine (spec D3/R3 global one-turn gate).
func (e *Engine) runWorker() {
	defer e.workerWg.Done()
	for {
		select {
		case <-e.ctx.Done():
			return
		case task, ok := <-e.workerCh:
			if !ok {
				return
			}

			// If it's an operational notice task:
			if task.isNotice {
				if e.announcer != nil {
					_ = e.announcer.Announce(e.ctx, task.prompt)
				}
				e.workerDoneCh <- firingResult{task: task}
				continue
			}

			turnCtx := WithTaskKind(e.ctx, task.kind())
			var replyText string
			var err error
			if e.textRunner != nil {
				replyText, err = e.textRunner(turnCtx, task.sessionID, task.prompt)
			} else if e.runner != nil {
				err = e.runner(turnCtx, task.sessionID, task.prompt)
			}

			if err != nil {
				e.workerDoneCh <- firingResult{task: task, err: err}
				continue
			}

			trimmedReply := strings.TrimSpace(replyText)
			if trimmedReply == "" || (task.isHeartbeat && IsHeartbeatOK(trimmedReply)) {
				e.workerDoneCh <- firingResult{task: task, replyText: replyText}
				continue
			}

			// Non-empty candidate announcement: route through policy
			decCh := make(chan announceDecision, 1)
			e.sendCommand(cmdCheckAnnounce{
				task: task,
				text: replyText,
				resp: decCh,
			})

			var decision announceDecision
			select {
			case decision = <-decCh:
			case <-e.ctx.Done():
				return
			}

			if decision.noticeText != "" && e.announcer != nil {
				_ = e.announcer.Announce(e.ctx, decision.noticeText)
			}
			if decision.allowed && e.announcer != nil {
				_ = e.announcer.Announce(e.ctx, replyText)
			}

			e.workerDoneCh <- firingResult{
				task:             task,
				replyText:        replyText,
				announceDecision: &decision,
			}
		}
	}
}

// runLoop is the command loop goroutine (spec D3/R3).
func (e *Engine) runLoop() {
	defer e.loopWg.Done()

	for {
		// Prioritize completing worker tasks before servicing new commands
		select {
		case res := <-e.workerDoneCh:
			e.handleWorkerDone(res)
			continue
		default:
		}

		select {
		case <-e.stopCh:
			return

		case res := <-e.workerDoneCh:
			e.handleWorkerDone(res)

		case rawCmd := <-e.cmdCh:
			e.processCommand(rawCmd)
		}
	}
}

func (e *Engine) processCommand(rawCmd any) {
	switch cmd := rawCmd.(type) {
	case cmdAddJob:
		e.handleAddJob(cmd)
	case cmdRemoveJob:
		e.handleRemoveJob(cmd)
	case cmdPauseJob:
		e.handlePauseJob(cmd)
	case cmdResumeJob:
		e.handleResumeJob(cmd)
	case cmdRunJob:
		e.handleRunJob(cmd)
	case cmdEditJob:
		e.handleEditJob(cmd)
	case cmdGetJob:
		e.handleGetJob(cmd)
	case cmdListJobs:
		e.handleListJobs(cmd)
	case cmdJobDue:
		e.handleJobDue(cmd)
	case cmdJobRetry:
		e.handleJobRetry(cmd)
	case cmdSync:
		e.handleSync(cmd)
	case cmdDispatchTask:
		if cmd.task.isGoal {
			e.dispatchGoalTaskOrDefer(cmd.task)
		} else {
			e.dispatchOrDefer(cmd.task)
		}
	default:
		e.processExtendedCommand(rawCmd)
	case cmdShutdown:
		// Spec B4: loop stops its own timers and replies
		e.stopAllTimersLocked()

		// Spec D13: the loop stays alive until the cancelled worker's completion event is received
		if e.workerBusy {
			select {
			case res := <-e.workerDoneCh:
				e.handleWorkerDone(res)
			case <-time.After(10 * time.Second):
			}
		}

		close(cmd.resp)
		close(e.stopCh)
		return
	}
}

// processExtendedCommand dispatches the goals/quiet/heartbeat/noise command
// set (F2/F3). Split out of processCommand to keep both switches readable.
func (e *Engine) processExtendedCommand(rawCmd any) {
	switch cmd := rawCmd.(type) {
	case cmdAddGoal:
		e.handleAddGoal(cmd)
	case cmdPauseGoal:
		e.handlePauseGoal(cmd)
	case cmdResumeGoal:
		e.handleResumeGoal(cmd)
	case cmdCancelGoal:
		e.handleCancelGoal(cmd)
	case cmdSetGoalBudget:
		e.handleSetGoalBudget(cmd)
	case cmdForceCloseGoal:
		e.handleForceCloseGoal(cmd)
	case cmdGetGoal:
		e.handleGetGoal(cmd)
	case cmdListGoals:
		e.handleListGoals(cmd)
	case cmdGoalStep:
		e.handleGoalStep(cmd)
	case cmdGoalClose:
		e.handleGoalClose(cmd)
	case cmdGoalDue:
		e.handleGoalDue(cmd)
	case cmdGoalRetry:
		e.handleGoalRetry(cmd)
	case cmdCheckAnnounce:
		e.handleCheckAnnounce(cmd)
	case cmdQuietCollapseDue:
		e.handleQuietCollapseDue(cmd)
	case cmdHeartbeatDue:
		e.handleHeartbeatDue(cmd)
	case cmdHeartbeatIdleTick:
		e.handleHeartbeatIdleTick(cmd)
	case cmdHeartbeatRetry:
		e.handleHeartbeatRetry(cmd)
	case cmdHeartbeatCollapseDue:
		e.handleHeartbeatCollapseDue(cmd)
	case cmdNoiseSnapshot:
		e.handleNoiseSnapshot(cmd)
	case cmdCheckEditDedup:
		e.handleCheckEditDedup(cmd)
	case cmdRecordEditDedup:
		e.handleRecordEditDedup(cmd)
	case cmdProposeJob:
		e.handleProposeJob(cmd)
	case cmdSettleProposal:
		e.handleSettleProposal(cmd)
	}
}

// armStartupTimersLocked arms timers for every active job and goal before the
// loop starts (Spec B4: no map writes outside loop).
func (e *Engine) armStartupTimersLocked() {
	for i := range e.jobs {
		job := &e.jobs[i]
		if job.Enabled && !job.Paused && job.NextDue != "" {
			dueTime, err := time.Parse(time.RFC3339, job.NextDue)
			if err == nil {
				e.armTimerLocked(job, dueTime, false, time.Time{})
			}
		}
	}
	for i := range e.goals {
		goal := &e.goals[i]
		if goal.Status == "active" && goal.NextDue != "" {
			dueTime, err := time.Parse(time.RFC3339, goal.NextDue)
			if err == nil {
				e.armGoalTimerLocked(goal, dueTime)
			}
		}
	}
}

// stopAllTimersLocked stops every live timer owned by the loop (shutdown path).
func (e *Engine) stopAllTimersLocked() {
	for _, t := range e.activeTimers {
		if t != nil {
			t.Stop()
		}
	}
	for _, t := range e.retryTimers {
		if t != nil {
			t.Stop()
		}
	}
	for _, t := range e.goalTimers {
		if t != nil {
			t.Stop()
		}
	}
	for _, t := range e.goalRetryTimers {
		if t != nil {
			t.Stop()
		}
	}
	for _, t := range e.quietCollapseTimers {
		if t != nil {
			t.Stop()
		}
	}
	for _, t := range []loop.Timer{
		e.heartbeatTimer,
		e.heartbeatIdleTimer,
		e.heartbeatRetryTimer,
		e.heartbeatCollapseTimer,
	} {
		if t != nil {
			t.Stop()
		}
	}
}

func (e *Engine) handleSync(cmd cmdSync) {
	for e.workerBusy {
		select {
		case res := <-e.workerDoneCh:
			e.handleWorkerDone(res)
		case rawCmd := <-e.cmdCh:
			e.processCommand(rawCmd)
		case <-e.stopCh:
			return
		}
	}
	cmd.resp <- struct{}{}
}

func (e *Engine) handleAddJob(cmd cmdAddJob) {
	job := cmd.job
	now := e.clock.Now().UTC()

	if job.ID == "" {
		job.ID = NewJobID(now)
	}
	if err := job.Validate(); err != nil {
		cmd.resp <- addJobResp{err: err}
		return
	}

	for _, existing := range e.jobs {
		if existing.ID == job.ID {
			cmd.resp <- addJobResp{err: fmt.Errorf("job id %q already exists", job.ID)}
			return
		}
	}

	// User-tier add creates job enabled (spec D9)
	job.Enabled = true
	job.Paused = false

	res, err := e.calculateNextDue(&job, now)
	if err != nil {
		cmd.resp <- addJobResp{err: fmt.Errorf("calculate initial next due: %w", err)}
		return
	}
	job.NextDue = res.Due.Format(time.RFC3339)

	e.jobs = append(e.jobs, job)
	if err := SaveJobs(e.jobsPath, e.jobs); err != nil {
		e.jobs = e.jobs[:len(e.jobs)-1]
		cmd.resp <- addJobResp{err: fmt.Errorf("save jobs: %w", err)}
		return
	}

	_, _ = e.journal.Append(JournalEvent{
		Type:      "fasti_job_created",
		JobID:     job.ID,
		JobName:   job.Name,
		Prompt:    job.Prompt,
		SessionID: job.Session,
		DueAt:     job.NextDue,
	})

	e.armTimerLocked(&job, res.Due, res.IsDstGap, res.SkippedDue)
	cmd.resp <- addJobResp{job: job}
}

func (e *Engine) handleRemoveJob(cmd cmdRemoveJob) {
	idx := -1
	for i, j := range e.jobs {
		if j.ID == cmd.id {
			idx = i
			break
		}
	}
	if idx == -1 {
		cmd.resp <- ErrJobNotFound
		return
	}

	removed := e.jobs[idx]
	if t, ok := e.activeTimers[cmd.id]; ok && t != nil {
		t.Stop()
		delete(e.activeTimers, cmd.id)
	}
	if t, ok := e.retryTimers[cmd.id]; ok && t != nil {
		t.Stop()
		delete(e.retryTimers, cmd.id)
	}

	e.jobs = append(e.jobs[:idx], e.jobs[idx+1:]...)
	if err := SaveJobs(e.jobsPath, e.jobs); err != nil {
		cmd.resp <- fmt.Errorf("save jobs: %w", err)
		return
	}

	_, _ = e.journal.Append(JournalEvent{
		Type:    "fasti_job_removed",
		JobID:   removed.ID,
		JobName: removed.Name,
	})

	cmd.resp <- nil
}

func (e *Engine) handlePauseJob(cmd cmdPauseJob) {
	job := e.findJob(cmd.id)
	if job == nil {
		cmd.resp <- ErrJobNotFound
		return
	}

	if job.Paused {
		// Replay idempotence: no-op success ack (spec D8)
		cmd.resp <- nil
		return
	}

	// Spec D5 / M11: Paused and Enabled are distinct; pause sets Paused only
	job.Paused = true
	if t, ok := e.activeTimers[cmd.id]; ok && t != nil {
		t.Stop()
		delete(e.activeTimers, cmd.id)
	}
	if t, ok := e.retryTimers[cmd.id]; ok && t != nil {
		t.Stop()
		delete(e.retryTimers, cmd.id)
	}

	if err := SaveJobs(e.jobsPath, e.jobs); err != nil {
		cmd.resp <- fmt.Errorf("save jobs: %w", err)
		return
	}

	_, _ = e.journal.Append(JournalEvent{
		Type:    "fasti_job_paused",
		Reason:  "user_pause",
		JobID:   job.ID,
		JobName: job.Name,
	})

	cmd.resp <- nil
}

func (e *Engine) handleResumeJob(cmd cmdResumeJob) {
	job := e.findJob(cmd.id)
	if job == nil {
		cmd.resp <- ErrJobNotFound
		return
	}

	if !job.Paused && job.Enabled {
		// Replay idempotence
		cmd.resp <- nil
		return
	}

	job.Paused = false
	job.Enabled = true

	now := e.clock.Now().UTC()
	res, err := e.calculateNextDue(job, now)
	if err != nil {
		cmd.resp <- fmt.Errorf("calculate next due: %w", err)
		return
	}

	job.NextDue = res.Due.Format(time.RFC3339)
	if err := SaveJobs(e.jobsPath, e.jobs); err != nil {
		cmd.resp <- fmt.Errorf("save jobs: %w", err)
		return
	}

	_, _ = e.journal.Append(JournalEvent{
		Type:    "fasti_job_resumed",
		JobID:   job.ID,
		JobName: job.Name,
		DueAt:   job.NextDue,
	})

	e.armTimerLocked(job, res.Due, res.IsDstGap, res.SkippedDue)
	cmd.resp <- nil
}

// handleEditJob applies a declarative prompt and/or schedule edit to one job
// row — atomic under the single-writer law (spec D8 PATCH verbs, D3 "edit").
//
// Adoption law (D9): a prompt edit through the user tier rewrites created_by
// to user:<surface> with audit event fasti_job_adopted. The engine never
// learns which client spoke (the v1 user tier is REST-only — D8: every
// surface, CLI and socket included, reaches the engine through it), so the
// stamp is "user:rest". Prompt edits on a live job (enabled, not paused) are
// refused: the edit path is pause → edit → resume (D9 "immutable while
// enabled" plus D5's Paused/Enabled distinction from M11).
func (e *Engine) handleEditJob(cmd cmdEditJob) {
	job := e.findJob(cmd.id)
	if job == nil {
		cmd.resp <- editJobResp{err: ErrJobNotFound}
		return
	}
	if cmd.prompt == nil && cmd.schedule == nil {
		// Declarative no-op: same row back (D8 replay idempotence).
		cmd.resp <- editJobResp{job: *job}
		return
	}

	snapshot := *job
	promptEdited := false
	if cmd.prompt != nil {
		if job.Enabled && !job.Paused {
			cmd.resp <- editJobResp{err: ErrPromptWhileEnabled}
			return
		}
		job.Prompt = *cmd.prompt
		job.CreatedBy = "user:rest"
		promptEdited = true
	}
	if cmd.schedule != nil {
		job.Schedule = *cmd.schedule
	}
	if err := job.Validate(); err != nil {
		*job = snapshot
		cmd.resp <- editJobResp{err: err}
		return
	}

	var res NextDueResult
	scheduleEdited := cmd.schedule != nil
	if scheduleEdited {
		var err error
		res, err = e.calculateNextDue(job, e.clock.Now().UTC())
		if err != nil {
			*job = snapshot
			cmd.resp <- editJobResp{err: fmt.Errorf("calculate next due: %w", err)}
			return
		}
		job.NextDue = res.Due.Format(time.RFC3339)
	}

	if err := SaveJobs(e.jobsPath, e.jobs); err != nil {
		*job = snapshot
		cmd.resp <- editJobResp{err: fmt.Errorf("save jobs: %w", err)}
		return
	}

	if scheduleEdited {
		if t, ok := e.activeTimers[cmd.id]; ok && t != nil {
			t.Stop()
			delete(e.activeTimers, cmd.id)
		}
		if t, ok := e.retryTimers[cmd.id]; ok && t != nil {
			t.Stop()
			delete(e.retryTimers, cmd.id)
		}
		if job.Enabled && !job.Paused {
			e.armTimerLocked(job, res.Due, res.IsDstGap, res.SkippedDue)
		}
	}

	if promptEdited {
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_job_adopted",
			JobID:     job.ID,
			JobName:   job.Name,
			Prompt:    job.Prompt,
			SessionID: job.Session,
			DueAt:     job.NextDue,
		})
	}
	cmd.resp <- editJobResp{job: *job}
}

func (e *Engine) handleRunJob(cmd cmdRunJob) {
	job := e.findJob(cmd.id)
	if job == nil {
		cmd.resp <- ErrJobNotFound
		return
	}

	now := e.clock.Now().UTC()
	qw, qm := EffectiveQuiet(job, e.cfg.QuietHours)
	task := firingTask{
		jobID:       job.ID,
		sessionID:   job.Session,
		prompt:      job.Prompt,
		dueAt:       now,
		reason:      "manual_run",
		quietWindow: qw,
		quietMode:   qm,
	}

	e.dispatchOrDefer(task)
	cmd.resp <- nil
}

func (e *Engine) handleGetJob(cmd cmdGetJob) {
	job := e.findJob(cmd.id)
	if job == nil {
		cmd.resp <- getJobResp{err: ErrJobNotFound}
		return
	}
	cmd.resp <- getJobResp{job: *job}
}

func (e *Engine) handleListJobs(cmd cmdListJobs) {
	out := make([]Job, len(e.jobs))
	copy(out, e.jobs)
	cmd.resp <- out
}

func (e *Engine) handleJobDue(cmd cmdJobDue) {
	job := e.findJob(cmd.jobID)
	if job == nil || !job.Enabled || job.Paused {
		return
	}

	now := e.clock.Now().UTC()

	// Handle spring-forward gap: skip with dst_gap event (Spec D4, B3)
	if cmd.isGap {
		// Spec B3: skip event names the skipped wall due (cmd.skippedDue)
		// One-event-per-armed-due rule collapses all gap minutes to this skip event.
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Status:    "skipped",
			Reason:    "dst_gap",
			JobID:     job.ID,
			JobName:   job.Name,
			DueAt:     cmd.skippedDue.Format(time.RFC3339),
			SessionID: job.Session,
		})

		// Check if the gap-end time is itself a legitimate due occurrence (Spec B3)
		ps, err := ParseSchedule(job.Schedule)
		if err == nil && ps.Matches(cmd.due, e.loc) {
			// Spec B3: gap end matches schedule (e.g. 03:00 for */5) — arm that next real due without consuming it
			task := firingTask{
				jobID:     job.ID,
				sessionID: job.Session,
				prompt:    job.Prompt,
				dueAt:     cmd.due,
			}
			e.dispatchOrDefer(task)
			return
		}

		// Gap end does not match schedule (e.g. daily 02:30 at 03:00) — advance to next valid occurrence
		res, err := e.calculateNextDue(job, cmd.due)
		if err != nil {
			// Spec M10: auto-pause on recalculation failure
			job.Paused = true
			job.BudgetCounters.TotalFailures++
			_, _ = e.journal.Append(JournalEvent{
				Type:      "fasti_fired",
				Status:    "failed",
				Reason:    "schedule_recompute_failed",
				JobID:     job.ID,
				JobName:   job.Name,
				Prompt:    job.Prompt,
				SessionID: job.Session,
				DueAt:     job.NextDue,
			})
		} else {
			job.NextDue = res.Due.Format(time.RFC3339)
			e.armTimerLocked(job, res.Due, res.IsDstGap, res.SkippedDue)
		}
		e.saveAndPublishJobLocked(job)
		return
	}

	// Forward steps / downtime collapse (Spec B1, D4, D7)
	if now.Sub(cmd.due) > e.cfg.CatchupWindow {
		// Spec D7: dues older than catchup_window are discarded, not replayed
		job.BudgetCounters.TotalSkips++
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Status:    "skipped",
			Reason:    "stale_discard",
			JobID:     job.ID,
			JobName:   job.Name,
			Prompt:    job.Prompt,
			SessionID: job.Session,
			DueAt:     cmd.due.Format(time.RFC3339),
		})

		res, err := e.calculateNextDue(job, now)
		if err != nil {
			// Spec M10: auto-pause on recalculation failure
			job.Paused = true
			job.BudgetCounters.TotalFailures++
			_, _ = e.journal.Append(JournalEvent{
				Type:      "fasti_fired",
				Status:    "failed",
				Reason:    "schedule_recompute_failed",
				JobID:     job.ID,
				JobName:   job.Name,
				Prompt:    job.Prompt,
				SessionID: job.Session,
				DueAt:     job.NextDue,
			})
		} else {
			job.NextDue = res.Due.Format(time.RFC3339)
			e.armTimerLocked(job, res.Due, res.IsDstGap, res.SkippedDue)
		}
		e.saveAndPublishJobLocked(job)
		return
	}

	qw, qm := EffectiveQuiet(job, e.cfg.QuietHours)
	if qw != "" && IsInQuietWindow(qw, cmd.due, e.loc) && qm == "defers" {
		// Suppressed in defers mode: reschedule to window end, collapse dues into one firing (spec D7)
		wEnd := QuietWindowEnd(qw, cmd.due, e.loc)
		if e.quietCollapseTimers[job.ID] == nil {
			delay := wEnd.Sub(now)
			if delay < 0 {
				delay = 0
			}
			jobID := job.ID
			e.quietCollapseTimers[jobID] = e.clock.AfterFunc(delay, func() {
				e.sendCommand(cmdQuietCollapseDue{jobID: jobID})
			})
		}
		// Most recent prompt wins
		e.quietCollapses[job.ID] = quietCollapseEntry{
			jobID:     job.ID,
			prompt:    job.Prompt,
			windowEnd: wEnd,
		}

		// Journal law: a suppressed due is recorded even though it collapses
		// into a later single firing (D7).
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Status:    "deferred",
			Reason:    "quiet_hours",
			JobID:     job.ID,
			JobName:   job.Name,
			Prompt:    job.Prompt,
			SessionID: job.Session,
			DueAt:     cmd.due.Format(time.RFC3339),
		})

		// Advance schedule
		res, err := e.calculateNextDue(job, cmd.due)
		if err != nil {
			job.Paused = true
			job.BudgetCounters.TotalFailures++
			_, _ = e.journal.Append(JournalEvent{
				Type:      "fasti_fired",
				Status:    "failed",
				Reason:    "schedule_recompute_failed",
				JobID:     job.ID,
				JobName:   job.Name,
				Prompt:    job.Prompt,
				SessionID: job.Session,
				DueAt:     job.NextDue,
			})
		} else {
			// If next due is at or before windowEnd, advance after windowEnd to avoid double firing
			if !res.Due.After(wEnd) {
				resAfter, errAfter := e.calculateNextDue(job, wEnd)
				if errAfter == nil {
					res = resAfter
				}
			}
			job.NextDue = res.Due.Format(time.RFC3339)
			e.armTimerLocked(job, res.Due, res.IsDstGap, res.SkippedDue)
		}
		e.saveAndPublishJobLocked(job)
		return
	}

	reason := ""
	// Check if forward jump crossed dues while daemon ran (Spec B1, D4)
	ps, err := ParseSchedule(job.Schedule)
	if err == nil {
		if ps.interval > 0 && now.Sub(cmd.due) >= ps.interval {
			// More than one full period elapsed: collapse per D7
			reason = "slipped_while_offline"
		} else if ps.cron != nil {
			nextAfter, err := ps.Next(cmd.due, e.loc)
			if err == nil && !nextAfter.Due.After(now) {
				// Dues crossed while daemon ran: collapse per D7
				reason = "slipped_while_offline"
			}
		}
	}

	task := firingTask{
		jobID:       job.ID,
		sessionID:   job.Session,
		prompt:      job.Prompt,
		dueAt:       cmd.due,
		reason:      reason,
		quietWindow: qw,
		quietMode:   qm,
	}

	e.dispatchOrDefer(task)
}

func (e *Engine) handleJobRetry(cmd cmdJobRetry) {
	delete(e.retryTimers, cmd.jobID)

	job := e.findJob(cmd.jobID)
	if job == nil || !job.Enabled || job.Paused {
		return
	}

	qw, qm := EffectiveQuiet(job, e.cfg.QuietHours)
	task := firingTask{
		jobID:       job.ID,
		sessionID:   job.Session,
		prompt:      job.Prompt,
		dueAt:       cmd.originalDue,
		quietWindow: qw,
		quietMode:   qm,
	}

	if e.workerBusy {
		// Second contention: defer to next scheduled due time without incrementing skips (spec D3)
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Status:    "skipped",
			Reason:    "engine_busy",
			JobID:     job.ID,
			JobName:   job.Name,
			DueAt:     cmd.originalDue.Format(time.RFC3339),
			SessionID: job.Session,
		})

		res, err := e.calculateNextDue(job, e.clock.Now().UTC())
		if err != nil {
			// Spec M10: auto-pause on recalculation failure
			job.Paused = true
			job.BudgetCounters.TotalFailures++
			_, _ = e.journal.Append(JournalEvent{
				Type:      "fasti_fired",
				Status:    "failed",
				Reason:    "schedule_recompute_failed",
				JobID:     job.ID,
				JobName:   job.Name,
				Prompt:    job.Prompt,
				SessionID: job.Session,
				DueAt:     job.NextDue,
			})
		} else {
			job.NextDue = res.Due.Format(time.RFC3339)
			e.armTimerLocked(job, res.Due, res.IsDstGap, res.SkippedDue)
		}
		e.saveAndPublishJobLocked(job)
		return
	}

	e.workerBusy = true
	e.currentFiring = &task
	select {
	case e.workerCh <- task:
	case <-e.stopCh:
		return
	}
}

func (e *Engine) dispatchOrDefer(task firingTask) {
	job := e.findJob(task.jobID)

	if e.workerBusy {
		// Gate contention detected AT DISPATCH by command loop (spec D3/R3)
		if job != nil {
			job.BudgetCounters.TotalDefers++
			e.saveAndPublishJobLocked(job)
		}

		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Status:    "deferred",
			Reason:    "engine_busy",
			JobID:     task.jobID,
			DueAt:     task.dueAt.Format(time.RFC3339),
			SessionID: task.sessionID,
		})

		// Arm retry on loop timers at +60s (spec D3)
		retryTimer := e.clock.AfterFunc(60*time.Second, func() {
			e.sendCommand(cmdJobRetry{jobID: task.jobID, originalDue: task.dueAt})
		})
		e.retryTimers[task.jobID] = retryTimer
		return
	}

	e.workerBusy = true
	e.currentFiring = &task
	select {
	case e.workerCh <- task:
	case <-e.stopCh:
		return
	}
}

func (e *Engine) handleWorkerDone(res firingResult) {
	if res.task.isNotice {
		e.workerBusy = false
		e.currentFiring = nil
		return
	}

	if res.task.isHeartbeat {
		e.handleHeartbeatWorkerDone(res)
		return
	}

	if res.task.isGoal {
		e.handleGoalWorkerDone(res)
		return
	}

	e.workerBusy = false
	e.currentFiring = nil

	job := e.findJob(res.task.jobID)
	if job == nil {
		return
	}

	now := e.clock.Now().UTC()

	if res.err != nil {
		if errors.Is(res.err, context.Canceled) || e.ctx.Err() != nil {
			// Spec D13: worker cancelled mid-turn during shutdown
			_, _ = e.journal.Append(JournalEvent{
				Type:      "fasti_fired",
				Status:    "skipped",
				Reason:    "shutdown",
				JobID:     job.ID,
				JobName:   job.Name,
				Prompt:    res.task.prompt,
				SessionID: res.task.sessionID,
				DueAt:     res.task.dueAt.Format(time.RFC3339),
			})
			return
		}

		if isTurnInFlight(res.err) {
			// Session busy: record skip event (spec D3/R3)
			job.ConsecutiveSkips++
			job.BudgetCounters.TotalSkips++

			_, _ = e.journal.Append(JournalEvent{
				Type:      "fasti_fired",
				Status:    "skipped",
				Reason:    "session_busy",
				JobID:     job.ID,
				JobName:   job.Name,
				Prompt:    res.task.prompt,
				SessionID: res.task.sessionID,
				DueAt:     res.task.dueAt.Format(time.RFC3339),
			})

			// On 5th consecutive skip, self-pause job (spec D3/R3, M11)
			if job.ConsecutiveSkips >= 5 {
				// Spec D3: "pauses itself" — set Paused only; Enabled stays true (spec D5, M11)
				job.Paused = true

				_, _ = e.journal.Append(JournalEvent{
					Type:      "fasti_job_paused",
					Reason:    "consecutive_skips",
					JobID:     job.ID,
					JobName:   job.Name,
					Prompt:    job.Prompt,
					SessionID: job.Session,
				})

				// Emit matching operational notice alongside (spec D3/D7)
				noticeText := AutoPauseNotice(job)
				_, _ = e.journal.Append(JournalEvent{
					Type:      "fasti_notice",
					Kind:      "auto_pause",
					Reason:    "notice",
					JobID:     job.ID,
					JobName:   job.Name,
					Prompt:    noticeText,
					SessionID: job.Session,
				})
				e.noiseState.RecordNotice("auto_pause", now)
				_ = SaveNoiseState(e.statePath, e.noiseState)
				e.dispatchNoticeTask(firingTask{
					isNotice:   true,
					noticeKind: "auto_pause",
					prompt:     noticeText,
				})

				e.saveAndPublishJobLocked(job)
				return // Do not re-arm paused job
			}
		} else {
			// Non-concurrency error
			job.BudgetCounters.TotalFailures++
			_, _ = e.journal.Append(JournalEvent{
				Type:      "fasti_fired",
				Status:    "failed",
				Reason:    res.err.Error(),
				JobID:     job.ID,
				JobName:   job.Name,
				Prompt:    res.task.prompt,
				SessionID: res.task.sessionID,
				DueAt:     res.task.dueAt.Format(time.RFC3339),
			})
		}
	} else {
		// Firing successful
		job.ConsecutiveSkips = 0
		job.LastFire = now.Format(time.RFC3339)
		job.BudgetCounters.TotalRuns++

		status := "fired"
		reason := res.task.reason
		if res.announceDecision != nil && !res.announceDecision.allowed {
			status = "silenced"
			reason = res.announceDecision.reason
		} else if reason == "" {
			reason = "normal"
		}

		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Status:    status,
			Reason:    reason,
			JobID:     job.ID,
			JobName:   job.Name,
			Prompt:    res.task.prompt,
			SessionID: res.task.sessionID,
			FiredAt:   job.LastFire,
		})
	}

	// Re-arm for next due time
	// Spec M5: Anchor to the DUE that just fired instead of completion time
	var nextRes NextDueResult
	var err error

	if job.Schedule.Kind == "interval" {
		ps, pErr := ParseSchedule(job.Schedule)
		if pErr != nil {
			err = pErr
		} else {
			nextDue := res.task.dueAt.Add(ps.interval)
			for !nextDue.After(now) {
				nextDue = nextDue.Add(ps.interval)
			}
			if job.LastFire != "" {
				if lf, lErr := time.Parse(time.RFC3339, job.LastFire); lErr == nil {
					for !nextDue.After(lf) {
						nextDue = nextDue.Add(ps.interval)
					}
				}
			}
			nextRes = NextDueResult{Due: nextDue, IsDstGap: false}
		}
	} else {
		nextRes, err = e.calculateNextDue(job, now)
	}

	if err != nil {
		// Spec M10: auto-pause job and journal fasti_fired{status:failed,reason:schedule_recompute_failed}
		job.Paused = true
		job.BudgetCounters.TotalFailures++
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Status:    "failed",
			Reason:    "schedule_recompute_failed",
			JobID:     job.ID,
			JobName:   job.Name,
			Prompt:    job.Prompt,
			SessionID: job.Session,
			DueAt:     job.NextDue,
		})
	} else {
		job.NextDue = nextRes.Due.Format(time.RFC3339)
		e.armTimerLocked(job, nextRes.Due, nextRes.IsDstGap, nextRes.SkippedDue)
	}

	e.saveAndPublishJobLocked(job)
}

func (e *Engine) armTimerLocked(job *Job, due time.Time, isGap bool, skippedDue time.Time) {
	if t, ok := e.activeTimers[job.ID]; ok && t != nil {
		t.Stop()
		delete(e.activeTimers, job.ID)
	}

	now := e.clock.Now().UTC()
	delay := due.Sub(now)
	if delay < 0 {
		delay = 0
	}

	jobID := job.ID
	timer := e.clock.AfterFunc(delay, func() {
		e.sendCommand(cmdJobDue{jobID: jobID, due: due, isGap: isGap, skippedDue: skippedDue})
	})
	e.activeTimers[job.ID] = timer
}

func (e *Engine) sendCommand(cmd any) {
	select {
	case e.cmdCh <- cmd:
	case <-e.stopCh:
	}
}

func (e *Engine) calculateNextDue(job *Job, from time.Time) (NextDueResult, error) {
	ps, err := ParseSchedule(job.Schedule)
	if err != nil {
		return NextDueResult{}, err
	}

	// Spec D4: "a backward NTP step cannot re-arm a due time already fired (last_fire is the floor)"
	var lastFire time.Time
	hasLastFire := false
	if job.LastFire != "" {
		if lf, err := time.Parse(time.RFC3339, job.LastFire); err == nil {
			lastFire = lf
			hasLastFire = true
		}
	}

	if ps.interval > 0 {
		// For interval: anchor from last_fire instead of now if last_fire exists (Spec B1)
		interval := ps.interval
		base := from
		if hasLastFire {
			base = lastFire
		}
		cand := base.Add(interval)
		for !cand.After(from) || (hasLastFire && !cand.After(lastFire)) {
			cand = cand.Add(interval)
		}
		return NextDueResult{
			Due:      cand,
			IsDstGap: false,
		}, nil
	}

	// For cron: keep searching until candidate > last_fire (Spec B1)
	searchFrom := from
	if hasLastFire && !searchFrom.After(lastFire) {
		searchFrom = lastFire
	}

	res, err := ps.Next(searchFrom, e.loc)
	if err != nil {
		return NextDueResult{}, err
	}

	// Ensure computed next-due is never <= last_fire
	if hasLastFire {
		for err == nil && !res.Due.After(lastFire) {
			res, err = ps.Next(res.Due, e.loc)
		}
		if err != nil {
			return NextDueResult{}, err
		}
	}

	return res, nil
}

func (e *Engine) findJob(id string) *Job {
	for i := range e.jobs {
		if e.jobs[i].ID == id {
			return &e.jobs[i]
		}
	}
	return nil
}

func (e *Engine) findGoal(id string) *Goal {
	for i := range e.goals {
		if e.goals[i].ID == id {
			return &e.goals[i]
		}
	}
	return nil
}

func (e *Engine) saveAndPublishJobLocked(job *Job) {
	_ = SaveJobs(e.jobsPath, e.jobs)
	if job != nil {
		e.Publish(Event{Kind: "job", Op: "upsert", Row: *job})
	}
}

func (e *Engine) saveAndPublishGoalLocked(goal *Goal) {
	_ = SaveGoals(e.goalsPath, e.goals)
	e.saveJournalAndPublishGoalLocked(goal)
}

func (e *Engine) saveJournalAndPublishGoalLocked(goal *Goal) {
	if goal == nil {
		return
	}
	_ = SaveGoalJournal(e.home, goal)
	e.Publish(Event{Kind: "goal", Op: "upsert", Row: *goal})
}

func (e *Engine) readAndPublishHeartbeatLocked() HeartbeatInfo {
	info := ReadHeartbeatInfo(e.home)
	cur := HeartbeatState{
		Enabled: info.Enabled,
		Cadence: info.Cadence,
		Quiet:   info.Quiet,
	}
	if e.lastHeartbeat == nil || *e.lastHeartbeat != cur {
		e.lastHeartbeat = &cur
		e.Publish(Event{Kind: "heartbeat", Op: "upsert", Row: cur})
	}
	return info
}

func (e *Engine) sweepStaleGoalsLocked(now time.Time) {
	for i := range e.goals {
		goal := &e.goals[i]
		if goal.Status == "stale" {
			lastTouch := goal.UpdatedAt
			if lastTouch == "" {
				lastTouch = goal.CreatedAt
			}
			touchTime, err := time.Parse(time.RFC3339, lastTouch)
			if err == nil && !now.Before(touchTime.Add(30*24*time.Hour)) {
				goal.Status = "failed"
				goal.NextDue = ""
				goal.UpdatedAt = now.Format(time.RFC3339)
				e.saveAndPublishGoalLocked(goal)
				_, _ = e.journal.Append(JournalEvent{
					Type:      "goal_failed",
					Kind:      "goal",
					GoalID:    goal.ID,
					SessionID: goal.SessionID,
					Reason:    "stale_30d_untouched",
				})
			}
		}
	}
}

func (e *Engine) armGoalTimerLocked(goal *Goal, due time.Time) {
	if t, ok := e.goalTimers[goal.ID]; ok && t != nil {
		t.Stop()
		delete(e.goalTimers, goal.ID)
	}

	now := e.clock.Now().UTC()
	delay := due.Sub(now)
	if delay < 0 {
		delay = 0
	}

	goalID := goal.ID
	timer := e.clock.AfterFunc(delay, func() {
		e.sendCommand(cmdGoalDue{goalID: goalID, due: due})
	})
	e.goalTimers[goal.ID] = timer
}

func (e *Engine) handleAddGoal(cmd cmdAddGoal) {
	spec := cmd.spec
	now := e.clock.Now().UTC()
	e.sweepStaleGoalsLocked(now)

	if err := ValidatePrompt(spec.Title); err != nil {
		cmd.resp <- addGoalResp{err: fmt.Errorf("invalid title: %w", err)}
		return
	}
	if len(spec.Requirements) == 0 {
		cmd.resp <- addGoalResp{err: errors.New("goal must have at least one requirement")}
		return
	}
	for i, r := range spec.Requirements {
		if err := ValidatePrompt(r); err != nil {
			cmd.resp <- addGoalResp{err: fmt.Errorf("invalid requirement %d: %w", i, err)}
			return
		}
	}

	id := strings.TrimSpace(spec.ID)
	if id == "" {
		id = NewJobID(now)
	}

	for _, existing := range e.goals {
		if existing.ID == id {
			cmd.resp <- addGoalResp{err: fmt.Errorf("goal id %q already exists", id)}
			return
		}
	}

	sess := strings.TrimSpace(spec.SessionID)
	if sess == "" {
		sess = "goal-" + id
	}

	// Refused if held by another non-terminal goal (active|paused|stale hold; done|failed release)
	for _, existing := range e.goals {
		if existing.SessionID == sess {
			if existing.Status == "active" || existing.Status == "paused" || existing.Status == "stale" {
				cmd.resp <- addGoalResp{err: fmt.Errorf("session %q is already held by non-terminal goal %q", sess, existing.ID)}
				return
			}
		}
	}

	tb := spec.TurnBudget
	if tb <= 0 {
		tb = 25
	}

	sa := spec.StepAfter
	if sa <= 0 {
		sa = 15 * time.Minute
	}

	var createdSeq int64
	if e.sessionLog != nil {
		hw, err := e.sessionLog.HighWater(sess)
		if err != nil {
			cmd.resp <- addGoalResp{err: fmt.Errorf("session high-water: %w", err)}
			return
		}
		createdSeq = hw
	}

	reqs := make([]Requirement, len(spec.Requirements))
	for i, r := range spec.Requirements {
		reqs[i] = Requirement{
			Text:     r,
			Done:     false,
			Evidence: []int64{},
		}
	}

	nowStr := now.Format(time.RFC3339)
	goal := Goal{
		ID:           id,
		Title:        spec.Title,
		Status:       "active",
		Requirements: reqs,
		TurnCount:    0,
		TurnBudget:   tb,
		StepAfter:    sa,
		NextDue:      nowStr, // Arms immediately (due now)
		SessionID:    sess,
		CreatedAt:    nowStr,
		CreatedSeq:   createdSeq,
		UpdatedAt:    nowStr,
	}

	e.goals = append(e.goals, goal)
	if err := SaveGoals(e.goalsPath, e.goals); err != nil {
		e.goals = e.goals[:len(e.goals)-1]
		cmd.resp <- addGoalResp{err: fmt.Errorf("save goals: %w", err)}
		return
	}

	_ = SaveGoalJournal(e.home, &goal)

	_, _ = e.journal.Append(JournalEvent{
		Type:      "goal_created",
		Kind:      "goal",
		GoalID:    goal.ID,
		SessionID: goal.SessionID,
		Prompt:    goal.Title,
		DueAt:     goal.NextDue,
	})

	e.armGoalTimerLocked(&goal, now)
	cmd.resp <- addGoalResp{goal: goal}
}

func (e *Engine) handlePauseGoal(cmd cmdPauseGoal) {
	goal := e.findGoal(cmd.id)
	if goal == nil {
		cmd.resp <- ErrGoalNotFound
		return
	}

	if goal.Status == "paused" {
		// Replay idempotence: no-op success ack (spec D8, F2-3)
		cmd.resp <- nil
		return
	}

	goal.Status = "paused"
	goal.NextDue = ""
	goal.UpdatedAt = e.clock.Now().UTC().Format(time.RFC3339)

	if t, ok := e.goalTimers[cmd.id]; ok && t != nil {
		t.Stop()
		delete(e.goalTimers, cmd.id)
	}
	if t, ok := e.goalRetryTimers[cmd.id]; ok && t != nil {
		t.Stop()
		delete(e.goalRetryTimers, cmd.id)
	}

	if err := SaveGoals(e.goalsPath, e.goals); err != nil {
		cmd.resp <- fmt.Errorf("save goals: %w", err)
		return
	}
	e.saveJournalAndPublishGoalLocked(goal)

	_, _ = e.journal.Append(JournalEvent{
		Type:      "goal_paused",
		Kind:      "goal",
		GoalID:    goal.ID,
		SessionID: goal.SessionID,
	})

	cmd.resp <- nil
}

func (e *Engine) handleResumeGoal(cmd cmdResumeGoal) {
	goal := e.findGoal(cmd.id)
	if goal == nil {
		cmd.resp <- ErrGoalNotFound
		return
	}

	if goal.Status == "active" {
		// Replay idempotence
		cmd.resp <- nil
		return
	}

	// done/failed released the session hold (spec D10): reopening without an
	// exclusivity recheck could put two active goals on one session. Terminal
	// is terminal; paused and stale goals still re-arm normally below.
	if goal.Status == "done" || goal.Status == "failed" {
		cmd.resp <- fmt.Errorf("goal %q is terminal (%s); resume does not re-open terminal goals", goal.ID, goal.Status)
		return
	}

	now := e.clock.Now().UTC()
	goal.Status = "active"
	goal.NextDue = now.Format(time.RFC3339)
	goal.UpdatedAt = now.Format(time.RFC3339)

	if err := SaveGoals(e.goalsPath, e.goals); err != nil {
		cmd.resp <- fmt.Errorf("save goals: %w", err)
		return
	}
	e.saveJournalAndPublishGoalLocked(goal)

	_, _ = e.journal.Append(JournalEvent{
		Type:      "goal_resumed",
		Kind:      "goal",
		GoalID:    goal.ID,
		SessionID: goal.SessionID,
		DueAt:     goal.NextDue,
	})

	e.armGoalTimerLocked(goal, now)
	cmd.resp <- nil
}

func (e *Engine) handleCancelGoal(cmd cmdCancelGoal) {
	goal := e.findGoal(cmd.id)
	if goal == nil {
		cmd.resp <- ErrGoalNotFound
		return
	}

	if goal.Status == "failed" {
		// Replay idempotence
		cmd.resp <- nil
		return
	}

	goal.Status = "failed"
	goal.NextDue = ""
	goal.UpdatedAt = e.clock.Now().UTC().Format(time.RFC3339)

	if t, ok := e.goalTimers[cmd.id]; ok && t != nil {
		t.Stop()
		delete(e.goalTimers, cmd.id)
	}
	if t, ok := e.goalRetryTimers[cmd.id]; ok && t != nil {
		t.Stop()
		delete(e.goalRetryTimers, cmd.id)
	}

	if err := SaveGoals(e.goalsPath, e.goals); err != nil {
		cmd.resp <- fmt.Errorf("save goals: %w", err)
		return
	}
	e.saveJournalAndPublishGoalLocked(goal)

	_, _ = e.journal.Append(JournalEvent{
		Type:      "goal_failed",
		Kind:      "goal",
		GoalID:    goal.ID,
		SessionID: goal.SessionID,
		Reason:    "cancelled",
	})

	cmd.resp <- nil
}

func (e *Engine) handleForceCloseGoal(cmd cmdForceCloseGoal) {
	goal := e.findGoal(cmd.id)
	if goal == nil {
		cmd.resp <- ErrGoalNotFound
		return
	}

	goal.Status = "done"
	goal.NextDue = ""
	goal.UpdatedAt = e.clock.Now().UTC().Format(time.RFC3339)

	if t, ok := e.goalTimers[cmd.id]; ok && t != nil {
		t.Stop()
		delete(e.goalTimers, cmd.id)
	}
	if t, ok := e.goalRetryTimers[cmd.id]; ok && t != nil {
		t.Stop()
		delete(e.goalRetryTimers, cmd.id)
	}

	if err := SaveGoals(e.goalsPath, e.goals); err != nil {
		cmd.resp <- fmt.Errorf("save goals: %w", err)
		return
	}
	e.saveJournalAndPublishGoalLocked(goal)

	_, _ = e.journal.Append(JournalEvent{
		Type:      "goal_closed",
		Kind:      "goal",
		GoalID:    goal.ID,
		SessionID: goal.SessionID,
		Mode:      "user_force",
	})

	cmd.resp <- nil
}

func (e *Engine) handleSetGoalBudget(cmd cmdSetGoalBudget) {
	goal := e.findGoal(cmd.id)
	if goal == nil {
		cmd.resp <- ErrGoalNotFound
		return
	}

	goal.TurnBudget = cmd.budget
	goal.UpdatedAt = e.clock.Now().UTC().Format(time.RFC3339)

	if goal.Status == "active" {
		allDone := true
		for _, req := range goal.Requirements {
			if !req.Done {
				allDone = false
				break
			}
		}
		if goal.TurnCount >= goal.TurnBudget && !allDone {
			goal.Status = "stale"
			goal.NextDue = ""
			if t, ok := e.goalTimers[cmd.id]; ok && t != nil {
				t.Stop()
				delete(e.goalTimers, cmd.id)
			}
			_, _ = e.journal.Append(JournalEvent{
				Type:      "goal_staled",
				Kind:      "goal",
				GoalID:    goal.ID,
				SessionID: goal.SessionID,
				Reason:    "budget_exhausted",
			})
		}
	}

	if err := SaveGoals(e.goalsPath, e.goals); err != nil {
		cmd.resp <- fmt.Errorf("save goals: %w", err)
		return
	}
	e.saveJournalAndPublishGoalLocked(goal)
	cmd.resp <- nil
}

func (e *Engine) handleGetGoal(cmd cmdGetGoal) {
	now := e.clock.Now().UTC()
	e.sweepStaleGoalsLocked(now)
	goal := e.findGoal(cmd.id)
	if goal == nil {
		cmd.resp <- getGoalResp{err: ErrGoalNotFound}
		return
	}
	cmd.resp <- getGoalResp{goal: *goal}
}

func (e *Engine) handleListGoals(cmd cmdListGoals) {
	now := e.clock.Now().UTC()
	e.sweepStaleGoalsLocked(now)
	out := make([]Goal, len(e.goals))
	copy(out, e.goals)
	cmd.resp <- out
}

func (e *Engine) handleGoalStep(cmd cmdGoalStep) {
	goal := e.findGoal(cmd.goalID)
	if goal == nil {
		cmd.resp <- ErrGoalNotFound
		return
	}

	if goal.Status != "active" {
		cmd.resp <- fmt.Errorf("goal %q is not active (status %s)", goal.ID, goal.Status)
		return
	}

	if cmd.satisfiedReq < -1 || cmd.satisfiedReq >= len(goal.Requirements) {
		cmd.resp <- fmt.Errorf("invalid satisfied requirement index %d", cmd.satisfiedReq)
		return
	}

	// Step-window law (spec D10): evidence may only be stamped from inside the
	// goal's own open step window. Registration (F4) makes an out-of-window call
	// impossible by construction; this is the machine-bound defence in depth.
	if cmd.satisfiedReq >= 0 && (e.activeStep == nil || e.activeStep.goalID != goal.ID) {
		cmd.resp <- fmt.Errorf("no open step window for goal %q: satisfied steps must come from the step turn", goal.ID)
		return
	}

	now := e.clock.Now().UTC()

	// Determine step window start (guaranteed open when satisfiedReq >= 0 above)
	var windowStart int64
	if e.activeStep != nil && e.activeStep.goalID == goal.ID {
		windowStart = e.activeStep.windowStart
	}

	callHW := windowStart
	if e.sessionLog != nil {
		hw, err := e.sessionLog.HighWater(goal.SessionID)
		if err != nil {
			cmd.resp <- fmt.Errorf("read high water: %w", err)
			return
		}
		callHW = hw
	}

	if cmd.satisfiedReq >= 0 {
		req := &goal.Requirements[cmd.satisfiedReq]
		req.Done = true

		// Attach evidence seqs window_start+1..HighWater(session)
		existingMap := make(map[int64]bool)
		for _, s := range req.Evidence {
			existingMap[s] = true
		}
		for s := windowStart + 1; s <= callHW; s++ {
			if !existingMap[s] {
				req.Evidence = append(req.Evidence, s)
				existingMap[s] = true
			}
		}
	}

	escapedProse := EscapeTagOpenings(cmd.progressProse)
	escapedHint := EscapeTagOpenings(cmd.nextHint)

	goal.ProgressProse = escapedProse
	goal.NextHint = escapedHint
	goal.UpdatedAt = now.Format(time.RFC3339)

	if err := SaveGoals(e.goalsPath, e.goals); err != nil {
		cmd.resp <- fmt.Errorf("save goals: %w", err)
		return
	}
	e.saveJournalAndPublishGoalLocked(goal)

	_, _ = e.journal.Append(JournalEvent{
		Type:      "goal_stepped",
		Kind:      "goal",
		GoalID:    goal.ID,
		SessionID: goal.SessionID,
		Prompt:    escapedProse,
		Details: map[string]any{
			"satisfied": cmd.satisfiedReq,
			"next_hint": escapedHint,
		},
	})

	cmd.resp <- nil
}

func (e *Engine) handleGoalClose(cmd cmdGoalClose) {
	goal := e.findGoal(cmd.goalID)
	if goal == nil {
		cmd.resp <- ErrGoalNotFound
		return
	}

	if goal.Status != "active" {
		cmd.resp <- fmt.Errorf("goal %q is not active (status %s)", goal.ID, goal.Status)
		return
	}

	// Agent path: closes ONLY when every requirement is done with non-empty evidence,
	// and every evidence seq passes HasSeq and post-dates created_seq (F2-4).
	var allSeqs []int64
	valid := len(goal.Requirements) > 0
	for _, req := range goal.Requirements {
		if !req.Done || len(req.Evidence) == 0 {
			valid = false
			break
		}
		for _, seq := range req.Evidence {
			if seq <= goal.CreatedSeq {
				valid = false
				break
			}
			if e.sessionLog != nil {
				has, err := e.sessionLog.HasSeq(goal.SessionID, seq)
				if err != nil || !has {
					valid = false
					break
				}
			}
			allSeqs = append(allSeqs, seq)
		}
		if !valid {
			break
		}
	}

	if !valid {
		_, _ = e.journal.Append(JournalEvent{
			Type:      "goal_close_refused",
			Kind:      "goal",
			GoalID:    goal.ID,
			SessionID: goal.SessionID,
			Reason:    "evidence",
		})
		cmd.resp <- ErrGoalClosedRefused
		return
	}

	now := e.clock.Now().UTC()
	goal.Status = "done"
	goal.NextDue = ""
	goal.UpdatedAt = now.Format(time.RFC3339)

	if t, ok := e.goalTimers[cmd.goalID]; ok && t != nil {
		t.Stop()
		delete(e.goalTimers, cmd.goalID)
	}
	if t, ok := e.goalRetryTimers[cmd.goalID]; ok && t != nil {
		t.Stop()
		delete(e.goalRetryTimers, cmd.goalID)
	}

	if err := SaveGoals(e.goalsPath, e.goals); err != nil {
		cmd.resp <- fmt.Errorf("save goals: %w", err)
		return
	}
	e.saveJournalAndPublishGoalLocked(goal)

	_, _ = e.journal.Append(JournalEvent{
		Type:         "goal_closed",
		Kind:         "goal",
		GoalID:       goal.ID,
		SessionID:    goal.SessionID,
		Mode:         "agent",
		EvidenceSeqs: allSeqs,
		Details:      map[string]any{"evidence_seqs": allSeqs},
	})

	cmd.resp <- nil
}

func (e *Engine) handleGoalDue(cmd cmdGoalDue) {
	delete(e.goalTimers, cmd.goalID)

	now := e.clock.Now().UTC()
	e.sweepStaleGoalsLocked(now)

	goal := e.findGoal(cmd.goalID)
	if goal == nil || goal.Status != "active" {
		return
	}

	// Forward steps / downtime collapse past catchup_window
	if now.Sub(cmd.due) > e.cfg.CatchupWindow {
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Kind:      "goal",
			Status:    "skipped",
			Reason:    "stale_discard",
			GoalID:    goal.ID,
			SessionID: goal.SessionID,
			DueAt:     cmd.due.Format(time.RFC3339),
		})

		nextDue := now.Add(goal.StepAfter)
		goal.NextDue = nextDue.Format(time.RFC3339)
		e.armGoalTimerLocked(goal, nextDue)
		e.saveAndPublishGoalLocked(goal)
		return
	}

	// Check if budget is already exhausted
	allDone := true
	for _, req := range goal.Requirements {
		if !req.Done {
			allDone = false
			break
		}
	}
	if goal.TurnCount >= goal.TurnBudget && !allDone {
		goal.Status = "stale"
		goal.NextDue = ""
		goal.UpdatedAt = now.Format(time.RFC3339)
		e.saveAndPublishGoalLocked(goal)
		_, _ = e.journal.Append(JournalEvent{
			Type:      "goal_staled",
			Kind:      "goal",
			GoalID:    goal.ID,
			SessionID: goal.SessionID,
			Reason:    "budget_exhausted",
		})
		return
	}

	reason := ""
	if now.Sub(cmd.due) >= goal.StepAfter {
		reason = "slipped_while_offline"
	}

	prompt := BuildGoalStepPrompt(goal)
	task := firingTask{
		jobID:     goal.ID,
		goalID:    goal.ID,
		isGoal:    true,
		sessionID: goal.SessionID,
		prompt:    prompt,
		dueAt:     cmd.due,
		reason:    reason,
	}

	e.dispatchGoalTaskOrDefer(task)
}

func (e *Engine) handleGoalRetry(cmd cmdGoalRetry) {
	delete(e.goalRetryTimers, cmd.goalID)

	goal := e.findGoal(cmd.goalID)
	if goal == nil || goal.Status != "active" {
		return
	}

	prompt := BuildGoalStepPrompt(goal)
	task := firingTask{
		jobID:     goal.ID,
		goalID:    goal.ID,
		isGoal:    true,
		sessionID: goal.SessionID,
		prompt:    prompt,
		dueAt:     cmd.originalDue,
	}

	if e.workerBusy {
		// Second contention: defer to next scheduled due time without incrementing skips (spec D3)
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Kind:      "goal",
			Status:    "skipped",
			Reason:    "engine_busy",
			GoalID:    goal.ID,
			DueAt:     cmd.originalDue.Format(time.RFC3339),
			SessionID: goal.SessionID,
		})

		nextDue := e.clock.Now().UTC().Add(goal.StepAfter)
		goal.NextDue = nextDue.Format(time.RFC3339)
		e.armGoalTimerLocked(goal, nextDue)
		e.saveAndPublishGoalLocked(goal)
		return
	}

	e.workerBusy = true
	e.currentFiring = &task

	var windowStart int64
	if e.sessionLog != nil {
		hw, err := e.sessionLog.HighWater(task.sessionID)
		if err == nil {
			windowStart = hw
		}
	}
	e.activeStep = &activeGoalStep{
		goalID:      task.goalID,
		sessionID:   task.sessionID,
		windowStart: windowStart,
	}

	select {
	case e.workerCh <- task:
	case <-e.stopCh:
		return
	}
}

func (e *Engine) dispatchGoalTaskOrDefer(task firingTask) {
	if e.workerBusy {
		// Gate contention detected AT DISPATCH by command loop (spec D3/R3, F2-5)
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Kind:      "goal",
			Status:    "deferred",
			Reason:    "engine_busy",
			GoalID:    task.goalID,
			DueAt:     task.dueAt.Format(time.RFC3339),
			SessionID: task.sessionID,
		})

		// Arm retry on loop timers at +60s (spec D3, F2-5)
		retryTimer := e.clock.AfterFunc(60*time.Second, func() {
			e.sendCommand(cmdGoalRetry{goalID: task.goalID, originalDue: task.dueAt})
		})
		e.goalRetryTimers[task.goalID] = retryTimer
		return
	}

	e.workerBusy = true
	e.currentFiring = &task

	var windowStart int64
	if e.sessionLog != nil {
		hw, err := e.sessionLog.HighWater(task.sessionID)
		if err == nil {
			windowStart = hw
		}
	}
	e.activeStep = &activeGoalStep{
		goalID:      task.goalID,
		sessionID:   task.sessionID,
		windowStart: windowStart,
	}

	select {
	case e.workerCh <- task:
	case <-e.stopCh:
		return
	}
}

func (e *Engine) handleGoalWorkerDone(res firingResult) {
	e.workerBusy = false
	e.currentFiring = nil

	if e.activeStep != nil && e.sessionLog != nil {
		hw, _ := e.sessionLog.HighWater(res.task.sessionID)
		e.activeStep.windowEnd = hw
	}

	goal := e.findGoal(res.task.goalID)
	if goal == nil {
		e.activeStep = nil
		return
	}

	now := e.clock.Now().UTC()
	e.sweepStaleGoalsLocked(now)

	if res.err != nil {
		if errors.Is(res.err, context.Canceled) || e.ctx.Err() != nil {
			// Spec D13: worker cancelled mid-turn during shutdown
			_, _ = e.journal.Append(JournalEvent{
				Type:      "fasti_fired",
				Kind:      "goal",
				Status:    "skipped",
				Reason:    "shutdown",
				GoalID:    goal.ID,
				Prompt:    res.task.prompt,
				SessionID: res.task.sessionID,
				DueAt:     res.task.dueAt.Format(time.RFC3339),
			})
			e.activeStep = nil
			return
		}

		if isTurnInFlight(res.err) {
			// Session busy: record skip event (spec D3/R3, F2-5)
			// Reschedules without charging budget!
			_, _ = e.journal.Append(JournalEvent{
				Type:      "fasti_fired",
				Kind:      "goal",
				Status:    "skipped",
				Reason:    "session_busy",
				GoalID:    goal.ID,
				Prompt:    res.task.prompt,
				SessionID: res.task.sessionID,
				DueAt:     res.task.dueAt.Format(time.RFC3339),
			})

			// Reschedule without charging budget (+60s retry timer)
			retryTimer := e.clock.AfterFunc(60*time.Second, func() {
				e.sendCommand(cmdGoalDue{goalID: goal.ID, due: e.clock.Now().UTC()})
			})
			e.goalRetryTimers[goal.ID] = retryTimer
			nextDue := now.Add(60 * time.Second)
			goal.NextDue = nextDue.Format(time.RFC3339)
			e.saveAndPublishGoalLocked(goal)
			e.activeStep = nil
			return
		}

		// Non-concurrency error
		goal.TurnCount++
		goal.UpdatedAt = now.Format(time.RFC3339)
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Kind:      "goal",
			Status:    "failed",
			Reason:    res.err.Error(),
			GoalID:    goal.ID,
			Prompt:    res.task.prompt,
			SessionID: res.task.sessionID,
			DueAt:     res.task.dueAt.Format(time.RFC3339),
		})
	} else {
		// Firing successful: turn actually ran
		goal.TurnCount++
		goal.UpdatedAt = now.Format(time.RFC3339)

		reason := res.task.reason
		if reason == "" {
			reason = "normal"
		}

		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Kind:      "goal",
			Status:    "fired",
			Reason:    reason,
			GoalID:    goal.ID,
			Prompt:    res.task.prompt,
			SessionID: res.task.sessionID,
			FiredAt:   now.Format(time.RFC3339),
		})
	}

	e.activeStep = nil

	// Check if status still active
	if goal.Status == "active" {
		allDone := true
		for _, req := range goal.Requirements {
			if !req.Done {
				allDone = false
				break
			}
		}

		// When turn_count reaches turn_budget WITHOUT all requirements done: status stale, event goal_staled exactly once (F2-5)
		if goal.TurnCount >= goal.TurnBudget && !allDone {
			goal.Status = "stale"
			goal.NextDue = ""
			goal.UpdatedAt = now.Format(time.RFC3339)
			e.saveAndPublishGoalLocked(goal)
			_, _ = e.journal.Append(JournalEvent{
				Type:      "goal_staled",
				Kind:      "goal",
				GoalID:    goal.ID,
				SessionID: goal.SessionID,
				Reason:    "budget_exhausted",
			})
			return
		}

		// On a completed step with status still active: arm next step step_after later (F2-5)
		nextDue := now.Add(goal.StepAfter)
		goal.NextDue = nextDue.Format(time.RFC3339)
		goal.UpdatedAt = now.Format(time.RFC3339)
		e.saveAndPublishGoalLocked(goal)
		e.armGoalTimerLocked(goal, nextDue)
	} else {
		e.saveAndPublishGoalLocked(goal)
	}
}

// isTurnInFlight checks if err represents a turn-in-flight conflict.
// F5 must propagate the ErrTurnInFlight sentinel; string matching is belt-and-braces only (Spec m13).
func isTurnInFlight(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTurnInFlight) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "turn in flight")
}

// Public API methods (send commands to the loop goroutine)

// Sync flushes pending work and waits for any active turn firing to complete (used by tests).
func (e *Engine) Sync(ctx context.Context) error {
	respCh := make(chan struct{}, 1)
	select {
	case e.cmdCh <- cmdSync{resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}

	select {
	case <-respCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}
}

// AddJob registers and persists a new recurring job.
func (e *Engine) AddJob(ctx context.Context, job Job) (Job, error) {
	respCh := make(chan addJobResp, 1)
	select {
	case e.cmdCh <- cmdAddJob{job: job, resp: respCh}:
	case <-ctx.Done():
		return Job{}, ctx.Err()
	case <-e.stopCh:
		return Job{}, errors.New("engine stopped")
	}

	select {
	case res := <-respCh:
		return res.job, res.err
	case <-ctx.Done():
		return Job{}, ctx.Err()
	case <-e.stopCh:
		return Job{}, errors.New("engine stopped")
	}
}

// RemoveJob deletes a job by its ID.
func (e *Engine) RemoveJob(ctx context.Context, id string) error {
	respCh := make(chan error, 1)
	select {
	case e.cmdCh <- cmdRemoveJob{id: id, resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}

	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}
}

// PauseJob pauses a job, stopping pending timers.
func (e *Engine) PauseJob(ctx context.Context, id string) error {
	respCh := make(chan error, 1)
	select {
	case e.cmdCh <- cmdPauseJob{id: id, resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}

	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}
}

// ResumeJob unpauses a job, recalculating next_due and arming the timer.
func (e *Engine) ResumeJob(ctx context.Context, id string) error {
	respCh := make(chan error, 1)
	select {
	case e.cmdCh <- cmdResumeJob{id: id, resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}

	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}
}

// RunJob triggers an immediate manual firing of a job.
func (e *Engine) RunJob(ctx context.Context, id string) error {
	respCh := make(chan error, 1)
	select {
	case e.cmdCh <- cmdRunJob{id: id, resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}

	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}
}

// EditJob applies a declarative edit to one job's prompt and/or schedule
// (spec D8 PATCH; nil pointer = leave that field alone). The edit is atomic
// under the single-writer law and preserves id, history, and last_fire —
// unlike a remove-and-re-add, which could lose the row across a crash.
// See handleEditJob for the D9 adoption law enforced here.
func (e *Engine) EditJob(ctx context.Context, id string, prompt *string, schedule *Schedule) (Job, error) {
	respCh := make(chan editJobResp, 1)
	select {
	case e.cmdCh <- cmdEditJob{id: id, prompt: prompt, schedule: schedule, resp: respCh}:
	case <-ctx.Done():
		return Job{}, ctx.Err()
	case <-e.stopCh:
		return Job{}, errors.New("engine stopped")
	}

	select {
	case res := <-respCh:
		return res.job, res.err
	case <-ctx.Done():
		return Job{}, ctx.Err()
	case <-e.stopCh:
		return Job{}, errors.New("engine stopped")
	}
}

// GetJob retrieves a job by ID.
func (e *Engine) GetJob(ctx context.Context, id string) (Job, error) {
	respCh := make(chan getJobResp, 1)
	select {
	case e.cmdCh <- cmdGetJob{id: id, resp: respCh}:
	case <-ctx.Done():
		return Job{}, ctx.Err()
	case <-e.stopCh:
		return Job{}, errors.New("engine stopped")
	}

	select {
	case res := <-respCh:
		return res.job, res.err
	case <-ctx.Done():
		return Job{}, ctx.Err()
	case <-e.stopCh:
		return Job{}, errors.New("engine stopped")
	}
}

// ListJobs returns a snapshot of all registered jobs.
func (e *Engine) ListJobs(ctx context.Context) ([]Job, error) {
	respCh := make(chan []Job, 1)
	select {
	case e.cmdCh <- cmdListJobs{resp: respCh}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.stopCh:
		return nil, errors.New("engine stopped")
	}

	select {
	case jobs := <-respCh:
		return jobs, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.stopCh:
		return nil, errors.New("engine stopped")
	}
}

// AddGoal registers and persists a new goal (spec D10, F2-3).
func (e *Engine) AddGoal(ctx context.Context, spec GoalSpec) (Goal, error) {
	respCh := make(chan addGoalResp, 1)
	select {
	case e.cmdCh <- cmdAddGoal{spec: spec, resp: respCh}:
	case <-ctx.Done():
		return Goal{}, ctx.Err()
	case <-e.stopCh:
		return Goal{}, errors.New("engine stopped")
	}

	select {
	case res := <-respCh:
		return res.goal, res.err
	case <-ctx.Done():
		return Goal{}, ctx.Err()
	case <-e.stopCh:
		return Goal{}, errors.New("engine stopped")
	}
}

// PauseGoal pauses a goal, freezing turn_count and dropping next_due (spec D10, F2-3).
func (e *Engine) PauseGoal(ctx context.Context, id string) error {
	respCh := make(chan error, 1)
	select {
	case e.cmdCh <- cmdPauseGoal{id: id, resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}

	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}
}

// ResumeGoal unpauses a goal, arming its next step immediately (spec D10, F2-3).
func (e *Engine) ResumeGoal(ctx context.Context, id string) error {
	respCh := make(chan error, 1)
	select {
	case e.cmdCh <- cmdResumeGoal{id: id, resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}

	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}
}

// CancelGoal marks a goal failed and releases its held session (spec D10, F2-3).
func (e *Engine) CancelGoal(ctx context.Context, id string) error {
	respCh := make(chan error, 1)
	select {
	case e.cmdCh <- cmdCancelGoal{id: id, resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}

	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}
}

// SetGoalBudget declaratively sets the turn budget for a goal (spec D10, F2-3).
func (e *Engine) SetGoalBudget(ctx context.Context, id string, budget int) error {
	respCh := make(chan error, 1)
	select {
	case e.cmdCh <- cmdSetGoalBudget{id: id, budget: budget, resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}

	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}
}

// ForceCloseGoal force-closes a goal with user authority regardless of evidence (spec D10, F2-3).
func (e *Engine) ForceCloseGoal(ctx context.Context, id string) error {
	respCh := make(chan error, 1)
	select {
	case e.cmdCh <- cmdForceCloseGoal{id: id, resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}

	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}
}

// GetGoal retrieves a goal by its ID (spec D10, F2-3).
func (e *Engine) GetGoal(ctx context.Context, id string) (Goal, error) {
	respCh := make(chan getGoalResp, 1)
	select {
	case e.cmdCh <- cmdGetGoal{id: id, resp: respCh}:
	case <-ctx.Done():
		return Goal{}, ctx.Err()
	case <-e.stopCh:
		return Goal{}, errors.New("engine stopped")
	}

	select {
	case res := <-respCh:
		return res.goal, res.err
	case <-ctx.Done():
		return Goal{}, ctx.Err()
	case <-e.stopCh:
		return Goal{}, errors.New("engine stopped")
	}
}

// ListGoals returns a snapshot of all registered goals (spec D10, F2-3).
func (e *Engine) ListGoals(ctx context.Context) ([]Goal, error) {
	respCh := make(chan []Goal, 1)
	select {
	case e.cmdCh <- cmdListGoals{resp: respCh}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.stopCh:
		return nil, errors.New("engine stopped")
	}

	select {
	case goals := <-respCh:
		return goals, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.stopCh:
		return nil, errors.New("engine stopped")
	}
}

// GoalStep records progress, next hint, and evidence for a satisfied requirement during a step turn (spec D10, F2-4).
func (e *Engine) GoalStep(ctx context.Context, goalID string, nextHint, progressProse string, satisfiedReq int) error {
	respCh := make(chan error, 1)
	select {
	case e.cmdCh <- cmdGoalStep{
		goalID:        goalID,
		nextHint:      nextHint,
		progressProse: progressProse,
		satisfiedReq:  satisfiedReq,
		resp:          respCh,
	}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}

	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}
}

// GoalClose closes a goal from within an agent turn, verifying that all requirements are met with valid evidence (spec D10, F2-4).
func (e *Engine) GoalClose(ctx context.Context, goalID string) error {
	respCh := make(chan error, 1)
	select {
	case e.cmdCh <- cmdGoalClose{goalID: goalID, resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}

	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}
}

func (e *Engine) dispatchNoticeTask(task firingTask) {
	if e.workerBusy {
		return
	}
	e.workerBusy = true
	e.currentFiring = &task
	select {
	case e.workerCh <- task:
	case <-e.stopCh:
	}
}

func (e *Engine) handleCheckAnnounce(cmd cmdCheckAnnounce) {
	now := e.clock.Now().UTC()
	e.noiseState.Prune(now)

	// 1. Quiet hours policy check (spec D7)
	if cmd.task.quietWindow != "" && IsInQuietWindow(cmd.task.quietWindow, now, e.loc) {
		_ = SaveNoiseState(e.statePath, e.noiseState)
		cmd.resp <- announceDecision{
			allowed: false,
			status:  "silenced",
			reason:  "quiet_hours",
		}
		return
	}

	// 2. Dedup policy check (spec D7)
	norm := NormalizeAnnouncement(cmd.text)
	if e.noiseState.CheckDedup(norm, now) {
		_ = SaveNoiseState(e.statePath, e.noiseState)
		cmd.resp <- announceDecision{
			allowed: false,
			status:  "silenced",
			reason:  "dedup",
		}
		return
	}

	// 3. Noise budget policy check (spec D7)
	if e.noiseState.AnnounceCount >= e.cfg.MaxAnnouncePerDay {
		triggeredDigest := e.noiseState.RecordOverflow(now)
		_ = SaveNoiseState(e.statePath, e.noiseState)

		noticeText := ""
		if triggeredDigest {
			noticeText = NoiseDigestNotice(5)
			_, _ = e.journal.Append(JournalEvent{
				Type:   "fasti_notice",
				Kind:   "noise_digest",
				Reason: "notice",
				Prompt: noticeText,
			})
			e.noiseState.RecordNotice("noise_digest", now)
			_ = SaveNoiseState(e.statePath, e.noiseState)
		}

		cmd.resp <- announceDecision{
			allowed:    false,
			status:     "silenced",
			reason:     "noise_budget",
			noticeText: noticeText,
		}
		return
	}

	// 4. Allowed
	e.noiseState.RecordAnnouncement(norm, now)
	_ = SaveNoiseState(e.statePath, e.noiseState)

	cmd.resp <- announceDecision{
		allowed: true,
		status:  "fired",
	}
}

func (e *Engine) handleQuietCollapseDue(cmd cmdQuietCollapseDue) {
	delete(e.quietCollapseTimers, cmd.jobID)
	entry, ok := e.quietCollapses[cmd.jobID]
	delete(e.quietCollapses, cmd.jobID)
	if !ok {
		return
	}

	job := e.findJob(cmd.jobID)
	if job == nil || !job.Enabled || job.Paused {
		return
	}

	qw, qm := EffectiveQuiet(job, e.cfg.QuietHours)
	task := firingTask{
		jobID:       job.ID,
		sessionID:   job.Session,
		prompt:      entry.prompt,
		dueAt:       entry.windowEnd,
		reason:      "quiet_collapse",
		quietWindow: qw,
		quietMode:   qm,
	}
	e.dispatchOrDefer(task)
}

func (e *Engine) handleHeartbeatDue(cmd cmdHeartbeatDue) {
	now := e.clock.Now().UTC()
	info := e.readAndPublishHeartbeatLocked()
	if !info.Enabled {
		e.armHeartbeatLocked()
		return
	}

	// Verify that now is actually due under current info cadence (in case cadence was edited between wakes, spec D6/R8)
	tl := now.In(e.loc)
	isDue := false
	switch info.Cadence {
	case "hourly":
		if tl.Minute() == 0 {
			isDue = true
		}
	case "daily":
		h, m := parseDailyAt(e.cfg.Heartbeat.DailyAt)
		if tl.Hour() == h && tl.Minute() == m {
			isDue = true
		}
	}

	if !isDue {
		e.armHeartbeatLocked()
		return
	}

	window := info.Quiet
	if window == "" {
		window = e.cfg.QuietHours
	}

	if window != "" && IsInQuietWindow(window, now, e.loc) {
		// Quiet at wake time: defers (spec D6/D7). Journal law: the heartbeat
		// wake itself is recorded before it collapses to the window end.
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Kind:      "heartbeat",
			Status:    "skipped",
			Reason:    "quiet_hours",
			DueAt:     cmd.due.Format(time.RFC3339),
			SessionID: "heartbeat",
		})
		wEnd := QuietWindowEnd(window, now, e.loc)
		if e.heartbeatCollapseTimer == nil {
			delay := wEnd.Sub(now)
			if delay < 0 {
				delay = 0
			}
			e.heartbeatCollapseTimer = e.clock.AfterFunc(delay, func() {
				e.sendCommand(cmdHeartbeatCollapseDue{})
			})
			e.heartbeatCollapse = &quietCollapseEntry{
				isHeartbeat: true,
				prompt:      BuildHeartbeatPrompt(info.Checklist),
				windowEnd:   wEnd,
			}
		} else {
			e.heartbeatCollapse.prompt = BuildHeartbeatPrompt(info.Checklist)
		}
		e.armHeartbeatAfterLocked(wEnd)
		return
	}

	task := firingTask{
		isHeartbeat: true,
		sessionID:   "heartbeat",
		prompt:      BuildHeartbeatPrompt(info.Checklist),
		dueAt:       cmd.due,
		reason:      "normal",
		quietWindow: window,
		quietMode:   "defers",
	}
	e.dispatchHeartbeatOrDefer(task)
	e.armHeartbeatLocked()
}

func (e *Engine) handleHeartbeatCollapseDue(cmd cmdHeartbeatCollapseDue) {
	e.heartbeatCollapseTimer = nil
	entry := e.heartbeatCollapse
	e.heartbeatCollapse = nil
	if entry == nil {
		return
	}

	info := e.readAndPublishHeartbeatLocked()
	window := info.Quiet
	if window == "" {
		window = e.cfg.QuietHours
	}

	task := firingTask{
		isHeartbeat: true,
		sessionID:   "heartbeat",
		prompt:      entry.prompt,
		dueAt:       entry.windowEnd,
		reason:      "quiet_collapse",
		quietWindow: window,
		quietMode:   "defers",
	}
	e.dispatchHeartbeatOrDefer(task)
}

func (e *Engine) handleHeartbeatIdleTick(cmd cmdHeartbeatIdleTick) {
	e.heartbeatIdleTimer = nil
	info := e.readAndPublishHeartbeatLocked()
	now := e.clock.Now().UTC()

	if !info.Enabled {
		if info.Cadence != "off" {
			e.heartbeatIdleTimer = e.clock.AfterFunc(time.Hour, func() {
				e.sendCommand(cmdHeartbeatIdleTick{})
			})
		}
		return
	}

	tl := now.In(e.loc)
	if info.Cadence == "hourly" && tl.Minute() == 0 && tl.Second() == 0 {
		e.handleHeartbeatDue(cmdHeartbeatDue{due: now})
		return
	}

	e.armHeartbeatLocked()
}

func (e *Engine) handleHeartbeatRetry(cmd cmdHeartbeatRetry) {
	e.heartbeatRetryTimer = nil
	info := e.readAndPublishHeartbeatLocked()
	if !info.Enabled {
		return
	}

	window := info.Quiet
	if window == "" {
		window = e.cfg.QuietHours
	}

	task := firingTask{
		isHeartbeat: true,
		sessionID:   "heartbeat",
		prompt:      BuildHeartbeatPrompt(info.Checklist),
		dueAt:       cmd.originalDue,
		quietWindow: window,
		quietMode:   "defers",
	}

	if e.workerBusy {
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Kind:      "heartbeat",
			Status:    "skipped",
			Reason:    "engine_busy",
			DueAt:     cmd.originalDue.Format(time.RFC3339),
			SessionID: "heartbeat",
		})
		e.armHeartbeatLocked()
		return
	}

	e.workerBusy = true
	e.currentFiring = &task
	select {
	case e.workerCh <- task:
	case <-e.stopCh:
		return
	}
}

func (e *Engine) dispatchHeartbeatOrDefer(task firingTask) {
	if e.workerBusy {
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Kind:      "heartbeat",
			Status:    "deferred",
			Reason:    "engine_busy",
			DueAt:     task.dueAt.Format(time.RFC3339),
			SessionID: task.sessionID,
		})

		e.heartbeatRetryTimer = e.clock.AfterFunc(60*time.Second, func() {
			e.sendCommand(cmdHeartbeatRetry{originalDue: task.dueAt})
		})
		return
	}

	e.workerBusy = true
	e.currentFiring = &task
	select {
	case e.workerCh <- task:
	case <-e.stopCh:
		return
	}
}

func (e *Engine) handleHeartbeatWorkerDone(res firingResult) {
	e.workerBusy = false
	e.currentFiring = nil
	now := e.clock.Now().UTC()

	if res.err != nil {
		if errors.Is(res.err, context.Canceled) || e.ctx.Err() != nil {
			_, _ = e.journal.Append(JournalEvent{
				Type:      "fasti_fired",
				Kind:      "heartbeat",
				Status:    "skipped",
				Reason:    "shutdown",
				Prompt:    res.task.prompt,
				SessionID: "heartbeat",
				DueAt:     res.task.dueAt.Format(time.RFC3339),
			})
			return
		}
		if isTurnInFlight(res.err) {
			_, _ = e.journal.Append(JournalEvent{
				Type:      "fasti_fired",
				Kind:      "heartbeat",
				Status:    "skipped",
				Reason:    "session_busy",
				Prompt:    res.task.prompt,
				SessionID: "heartbeat",
				DueAt:     res.task.dueAt.Format(time.RFC3339),
			})
		} else {
			_, _ = e.journal.Append(JournalEvent{
				Type:      "fasti_fired",
				Kind:      "heartbeat",
				Status:    "failed",
				Reason:    res.err.Error(),
				Prompt:    res.task.prompt,
				SessionID: "heartbeat",
				DueAt:     res.task.dueAt.Format(time.RFC3339),
			})
		}
		e.armHeartbeatLocked()
		return
	}

	if res.announceDecision != nil && !res.announceDecision.allowed {
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Kind:      "heartbeat",
			Status:    "silenced",
			Reason:    res.announceDecision.reason,
			Prompt:    res.task.prompt,
			SessionID: "heartbeat",
			DueAt:     res.task.dueAt.Format(time.RFC3339),
			FiredAt:   now.Format(time.RFC3339),
		})
	} else {
		reason := res.task.reason
		if reason == "" {
			reason = "normal"
		}
		_, _ = e.journal.Append(JournalEvent{
			Type:      "fasti_fired",
			Kind:      "heartbeat",
			Status:    "fired",
			Reason:    reason,
			Prompt:    res.task.prompt,
			SessionID: "heartbeat",
			DueAt:     res.task.dueAt.Format(time.RFC3339),
			FiredAt:   now.Format(time.RFC3339),
		})
	}

	e.armHeartbeatLocked()
}

func (e *Engine) armHeartbeatLocked() {
	e.armHeartbeatAfterLocked(e.clock.Now().UTC())
}

func (e *Engine) armHeartbeatAfterLocked(from time.Time) {
	if e.heartbeatTimer != nil {
		e.heartbeatTimer.Stop()
		e.heartbeatTimer = nil
	}
	if e.heartbeatIdleTimer != nil {
		e.heartbeatIdleTimer.Stop()
		e.heartbeatIdleTimer = nil
	}

	info := e.readAndPublishHeartbeatLocked()
	now := e.clock.Now().UTC()

	if !info.Enabled {
		if info.Cadence != "off" {
			e.heartbeatIdleTimer = e.clock.AfterFunc(time.Hour, func() {
				e.sendCommand(cmdHeartbeatIdleTick{})
			})
		}
		return
	}

	wakeTime, ok := CalculateNextHeartbeatWake(info, from, e.loc, e.cfg.Heartbeat.DailyAt)
	if !ok {
		return
	}

	delay := wakeTime.Sub(now)
	if delay < 0 {
		delay = 0
	}
	e.heartbeatTimer = e.clock.AfterFunc(delay, func() {
		e.sendCommand(cmdHeartbeatDue{due: wakeTime})
	})
}

func (e *Engine) handleNoiseSnapshot(cmd cmdNoiseSnapshot) {
	e.noiseState.Prune(e.clock.Now().UTC())
	cmd.resp <- e.noiseState.Snapshot(e.cfg.MaxAnnouncePerDay)
}

// NoiseSnapshot returns a read-only snapshot of current noise counters (spec D7/D8).
func (e *Engine) NoiseSnapshot() NoiseSnapshot {
	respCh := make(chan NoiseSnapshot, 1)
	select {
	case e.cmdCh <- cmdNoiseSnapshot{resp: respCh}:
	case <-e.stopCh:
		if e.noiseState != nil {
			return e.noiseState.Snapshot(e.cfg.MaxAnnouncePerDay)
		}
		return NoiseSnapshot{}
	}

	select {
	case snap := <-respCh:
		return snap
	case <-e.stopCh:
		if e.noiseState != nil {
			return e.noiseState.Snapshot(e.cfg.MaxAnnouncePerDay)
		}
		return NoiseSnapshot{}
	}
}

func (e *Engine) handleCheckEditDedup(cmd cmdCheckEditDedup) {
	if e.noiseState == nil {
		cmd.resp <- false
		return
	}
	cmd.resp <- e.noiseState.CheckEditDedup(cmd.id, cmd.hash)
}

func (e *Engine) handleRecordEditDedup(cmd cmdRecordEditDedup) {
	if e.noiseState != nil {
		e.noiseState.RecordEditDedup(cmd.id, cmd.hash)
		_ = SaveNoiseState(e.statePath, e.noiseState)
	}
	cmd.resp <- nil
}

// CheckEditDedup reports whether a payload hash has already been applied to target ID (spec D8).
func (e *Engine) CheckEditDedup(ctx context.Context, id, hash string) (bool, error) {
	respCh := make(chan bool, 1)
	select {
	case e.cmdCh <- cmdCheckEditDedup{id: id, hash: hash, resp: respCh}:
	case <-ctx.Done():
		return false, ctx.Err()
	case <-e.stopCh:
		return false, errors.New("engine stopped")
	}

	select {
	case replayed := <-respCh:
		return replayed, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-e.stopCh:
		return false, errors.New("engine stopped")
	}
}

// RecordEditDedup records a payload hash for a successfully applied edit to target ID (spec D8).
func (e *Engine) RecordEditDedup(ctx context.Context, id, hash string) error {
	respCh := make(chan error, 1)
	select {
	case e.cmdCh <- cmdRecordEditDedup{id: id, hash: hash, resp: respCh}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}

	select {
	case err := <-respCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopCh:
		return errors.New("engine stopped")
	}
}
