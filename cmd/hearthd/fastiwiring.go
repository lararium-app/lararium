package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lararium-app/lararium/internal/fasti"
	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/nuntius"
	"github.com/lararium-app/lararium/internal/router"
	"github.com/lararium-app/lararium/internal/surface"
)

var (
	errFastiAnnouncerUnset = errors.New("fasti: announcer bridge not configured")
	errFastiDisabled       = errors.New("fasti is disabled")
)

// SPEC-SILENCE: the spec says the card title includes the prompt "truncated sanely"
// without prescribing a rune limit; 80 runes keeps custody cards readable on mobile/Telegram.
const maxFastiCardPromptRunes = 80

// approvalHubProvider is implemented by *surface.Hub so startFasti can wire
// the approval card verdict hook without altering startFasti's signature.
type approvalHubProvider interface {
	ApprovalHub() *surface.ApprovalHub
}

// secretNeedlesProvider is implemented by *surface.Hub so startFasti can wire
// the lazy secret-needle source into the fasti engine (spec D9).
type secretNeedlesProvider interface {
	SecretNeedles() func() []string
}

type fastiCardRegistrar func(title, dest, argsSummary string) (string, error)

type fastiProposalBridge struct {
	mu           sync.Mutex
	eng          *fasti.Engine
	ap           *surface.ApprovalHub
	cardToJob    map[string]string
	registerCard fastiCardRegistrar
}

var (
	fastiBridgesMu sync.Mutex
	fastiBridges   = make(map[string]*fastiProposalBridge)
)

func getFastiBridge(home string) *fastiProposalBridge {
	key := filepath.Clean(home)
	fastiBridgesMu.Lock()
	defer fastiBridgesMu.Unlock()
	if b, ok := fastiBridges[key]; ok {
		return b
	}
	b := &fastiProposalBridge{
		cardToJob: make(map[string]string),
	}
	fastiBridges[key] = b
	return b
}

func (b *fastiProposalBridge) attach(eng *fasti.Engine, ap *surface.ApprovalHub) {
	b.mu.Lock()
	b.eng = eng
	b.cardToJob = make(map[string]string)
	if ap != nil {
		b.ap = ap
	}
	curAP := b.ap
	b.mu.Unlock()

	if curAP != nil {
		curAP.SetFastiVerdictHook(b.onVerdict)
	}
}

func (b *fastiProposalBridge) detach(eng *fasti.Engine) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if eng == nil || b.eng == eng {
		b.eng = nil
		b.cardToJob = make(map[string]string)
	}
}

func (b *fastiProposalBridge) setApprovalHub(ap *surface.ApprovalHub) {
	b.mu.Lock()
	b.ap = ap
	b.mu.Unlock()
	if ap != nil {
		ap.SetFastiVerdictHook(b.onVerdict)
	}
}

func (b *fastiProposalBridge) setCardRegistrar(fn fastiCardRegistrar) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.registerCard = fn
}

func (b *fastiProposalBridge) onVerdict(cardID string, allow, timedOut bool, _, via string) {
	b.mu.Lock()
	jobID, ok := b.cardToJob[cardID]
	if ok {
		delete(b.cardToJob, cardID)
	}
	eng := b.eng
	b.mu.Unlock()

	if !ok || eng == nil {
		return
	}

	outcome := fasti.ProposalDeny
	switch {
	case timedOut:
		outcome = fasti.ProposalTimeout
	case allow:
		outcome = fasti.ProposalAllow
	}
	_ = eng.SettleProposal(context.Background(), jobID, outcome, via)
}

type fastiJobToolArgs struct {
	Name     string               `json:"name"`
	Schedule json.RawMessage      `json:"schedule"`
	Prompt   string               `json:"prompt"`
	Session  string               `json:"session"`
	Announce string               `json:"announce"`
	Quiet    *fasti.QuietOverride `json:"quiet,omitempty"`
}

func parseFastiToolSchedule(raw json.RawMessage) (fasti.Schedule, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return fasti.Schedule{}, errors.New("schedule is required")
	}

	var sched fasti.Schedule
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal(raw, &sched); err != nil {
			return fasti.Schedule{}, fmt.Errorf("invalid schedule object: %w", err)
		}
	} else {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fasti.Schedule{}, fmt.Errorf("invalid schedule string: %w", err)
		}
		var err error
		sched, err = parseFastiScheduleString(s)
		if err != nil {
			return fasti.Schedule{}, err
		}
	}

	if _, err := fasti.ParseSchedule(sched); err != nil {
		return fasti.Schedule{}, err
	}
	return sched, nil
}

func parseFastiScheduleString(raw string) (fasti.Schedule, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fasti.Schedule{}, errors.New("schedule is required")
	}
	if strings.HasPrefix(s, "{") {
		var sched fasti.Schedule
		if err := json.Unmarshal([]byte(s), &sched); err != nil {
			return fasti.Schedule{}, fmt.Errorf("invalid schedule JSON: %w", err)
		}
		return sched, nil
	}
	if rest, ok := strings.CutPrefix(s, "every "); ok {
		return fasti.Schedule{Kind: "interval", Every: strings.TrimSpace(rest)}, nil
	}
	if rest, ok := strings.CutPrefix(s, "cron "); ok {
		return fasti.Schedule{Kind: "cron", Expr: strings.TrimSpace(rest)}, nil
	}
	if len(strings.Fields(s)) >= 5 {
		return fasti.Schedule{Kind: "cron", Expr: s}, nil
	}
	return fasti.Schedule{Kind: "interval", Every: s}, nil
}

func formatFastiSchedule(s fasti.Schedule) string {
	if s.Kind == "interval" {
		return "every " + s.Every
	}
	return "cron " + s.Expr
}

func truncateFastiCardPrompt(prompt string) string {
	clean := strings.Join(strings.Fields(prompt), " ")
	runes := []rune(clean)
	if len(runes) <= maxFastiCardPromptRunes {
		return clean
	}
	return string(runes[:maxFastiCardPromptRunes]) + "..."
}

func (b *fastiProposalBridge) propose(ctx context.Context, argsJSON string) (string, error) {
	b.mu.Lock()
	eng := b.eng
	ap := b.ap
	regFn := b.registerCard
	b.mu.Unlock()

	if eng == nil {
		return "", errFastiDisabled
	}

	var args fastiJobToolArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	sched, err := parseFastiToolSchedule(args.Schedule)
	if err != nil {
		return "", err
	}

	callerSID := loop.SessionIDFromContext(ctx)
	job := fasti.Job{
		Name:      strings.TrimSpace(args.Name),
		Schedule:  sched,
		Prompt:    args.Prompt,
		Session:   strings.TrimSpace(args.Session),
		Announce:  strings.TrimSpace(args.Announce),
		Quiet:     args.Quiet,
		CreatedBy: "model:" + callerSID,
	}

	// Spec D5: persist disabled row BEFORE registering the approval card.
	created, err := eng.ProposeJob(ctx, job)
	if err != nil {
		return "", err
	}

	cardID, err := b.registerProposalCard(created, ap, regFn)
	if err != nil {
		_ = eng.SettleProposal(ctx, created.ID, fasti.ProposalRollback, "rollback")
		return "", fmt.Errorf("register approval card: %w", err)
	}

	resp, err := json.Marshal(map[string]string{
		"id":      created.ID,
		"card_id": cardID,
		"status":  "pending approval",
	})
	if err != nil {
		return "", err
	}
	return string(resp), nil
}

func (b *fastiProposalBridge) registerProposalCard(
	job fasti.Job,
	ap *surface.ApprovalHub,
	regFn fastiCardRegistrar,
) (string, error) {
	schedStr := formatFastiSchedule(job.Schedule)
	promptSnippet := truncateFastiCardPrompt(job.Prompt)
	announcer := job.Announce
	if announcer == "" {
		announcer = "none"
	}
	title := fmt.Sprintf("fasti_job %s (%s, announce=%s): %s", job.Name, schedStr, announcer, promptSnippet)

	// SPEC-SILENCE: when job.Announce is empty, route dest to job.Session so the
	// custody card still carries a target session surface label.
	dest := job.Announce
	if dest == "" {
		dest = job.Session
	}
	argsSummary := fmt.Sprintf("id=%s schedule=%s prompt=%q announce=%s", job.ID, schedStr, promptSnippet, announcer)

	if regFn == nil {
		if ap == nil {
			return "", errors.New("approval hub unavailable")
		}
		regFn = func(cardTitle, cardDest, summary string) (string, error) {
			id, _ := ap.RegisterCustosNamed("", "", cardTitle, cardDest, summary, 0, nil)
			if id == "" {
				return "", errors.New("empty card id")
			}
			return id, nil
		}
	}

	cardID, err := regFn(title, dest, argsSummary)
	if err != nil {
		return "", err
	}
	if cardID == "" {
		return "", errors.New("empty card id")
	}

	b.mu.Lock()
	b.cardToJob[cardID] = job.ID
	b.mu.Unlock()
	return cardID, nil
}

// fastiJobTool returns the daemon-registered loop tool for proposing recurring fasti jobs (spec D5).
func fastiJobTool(home string) loop.Tool {
	bridge := getFastiBridge(home)
	return loop.Tool{
		Spec: router.ToolSpec{
			Name:        "fasti_job",
			Description: "Propose a recurring cron or interval job for human approval via a custody card. Never enables a job directly.",
			ParamsJSONSchema: `{"type":"object","properties":{
				"name":{"type":"string","description":"short human-readable job name"},
				"schedule":{"description":"schedule in fasti CLI grammar ('every <dur>', 'cron <5-field-expr> [tz]', or schedule object)"},
				"prompt":{"type":"string","description":"prompt executed when the job fires"},
				"session":{"type":"string","description":"bound session id, or 'new' for an isolated cron-<id> session"},
				"announce":{"type":"string","description":"optional announcement channel target"},
				"quiet":{"type":"object","properties":{"window":{"type":"string"},"mode":{"type":"string","enum":["suppresses_announces","defers"]}}}
			},"required":["name","schedule","prompt"]}`,
		},
		Trusted: true,
		Run:     bridge.propose,
	}
}

// headlessTurnDoor is the narrow turn-door interface satisfied by *surface.Hub (spec D2).
type headlessTurnDoor interface {
	RunHeadlessTurn(
		ctx context.Context,
		sessionID, text, channel string,
		updateID int64,
		onDelta func(delta string),
	) error
}

// fastiRealClock implements loop.Clock against the system wall clock for daemon serve mode.
type fastiRealClock struct{}

func (fastiRealClock) Now() time.Time {
	return time.Now().UTC()
}

func (fastiRealClock) AfterFunc(d time.Duration, f func()) loop.Timer {
	return time.AfterFunc(d, f)
}

// fastiAnnouncerHolder is a concurrent-safe indirection holder for fasti.Announcer
// so the fasti engine can be wired before ServeSocket while *nuntius.Bridge is
// populated once startNuntius returns (spec D7).
type fastiAnnouncerHolder struct {
	mu  sync.RWMutex
	ann fasti.Announcer
}

func newFastiAnnouncerHolder() *fastiAnnouncerHolder {
	return &fastiAnnouncerHolder{}
}

// Set attaches the active announcer (such as *nuntius.Bridge). Safe to call on a nil receiver.
func (h *fastiAnnouncerHolder) Set(ann fasti.Announcer) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if b, ok := ann.(*nuntius.Bridge); ok && b == nil {
		h.ann = nil
		return
	}
	h.ann = ann
}

// Announce delegates announcement delivery to the underlying bridge if configured.
func (h *fastiAnnouncerHolder) Announce(ctx context.Context, text string) error {
	if h == nil {
		return errFastiAnnouncerUnset
	}
	h.mu.RLock()
	ann := h.ann
	h.mu.RUnlock()
	if ann == nil {
		return errFastiAnnouncerUnset
	}
	return ann.Announce(ctx, text)
}

func mapTurnDoorErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, surface.ErrTurnInFlight) {
		return fmt.Errorf("%w: %w", fasti.ErrTurnInFlight, err)
	}
	return err
}

func newFastiRunner(door headlessTurnDoor) fasti.TurnRunner {
	return func(ctx context.Context, sessionID, prompt string) error {
		if door == nil {
			return nil
		}
		channel := fasti.TaskKindFromContext(ctx)
		return mapTurnDoorErr(door.RunHeadlessTurn(ctx, sessionID, prompt, channel, 0, nil))
	}
}

func newFastiTextRunner(door headlessTurnDoor) fasti.TextTurnRunner {
	return func(ctx context.Context, sessionID, prompt string) (string, error) {
		if door == nil {
			return "", nil
		}
		channel := fasti.TaskKindFromContext(ctx)
		var b strings.Builder
		err := door.RunHeadlessTurn(ctx, sessionID, prompt, channel, 0, func(delta string) {
			b.WriteString(delta)
		})
		if err != nil {
			return "", mapTurnDoorErr(err)
		}
		return strings.TrimSpace(b.String()), nil
	}
}

// startFasti constructs and starts the fasti proactivity engine when cfg.Enabled is true.
// When cfg.Enabled is false, no engine is constructed and all return values are nil.
func startFasti(
	ctx context.Context,
	cfg fasti.Config,
	home string,
	door headlessTurnDoor,
	clk loop.Clock,
	extraOpts ...fasti.EngineOption,
) (*fasti.Engine, *fastiAnnouncerHolder, func(), error) {
	bridge := getFastiBridge(home)
	if !cfg.Enabled {
		bridge.detach(nil)
		return nil, nil, nil, nil
	}
	userClk := clk
	if clk == nil {
		clk = fastiRealClock{}
	}
	holder := newFastiAnnouncerHolder()
	var secretNeedles func() []string
	if sp, ok := door.(secretNeedlesProvider); ok && sp != nil {
		secretNeedles = sp.SecretNeedles()
	}
	opts := []fasti.EngineOption{
		fasti.WithClock(clk),
		fasti.WithRunner(newFastiRunner(door)),
		fasti.WithTextRunner(newFastiTextRunner(door)),
		fasti.WithAnnouncer(holder),
		fasti.WithSecretNeedles(secretNeedles),
	}
	opts = append(opts, extraOpts...)

	eng, err := fasti.New(cfg, home, opts...)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := eng.Start(ctx); err != nil {
		eng.Stop()
		return nil, nil, nil, err
	}

	var ap *surface.ApprovalHub
	if p, ok := door.(approvalHubProvider); ok && p != nil {
		ap = p.ApprovalHub()
	}
	if ap != nil && userClk != nil {
		ap.SetClock(userClk)
	}
	bridge.attach(eng, ap)

	stopFn := func() {
		bridge.detach(eng)
		eng.Stop()
	}
	return eng, holder, stopFn, nil
}

func mustWireFasti(
	ctx context.Context,
	cfg fasti.Config,
	home string,
	srv *surface.Server,
	door headlessTurnDoor,
) (*fastiAnnouncerHolder, func()) {
	eng, holder, stopFasti, err := startFasti(ctx, cfg, home, door, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	srv.Fasti = eng
	return holder, stopFasti
}
