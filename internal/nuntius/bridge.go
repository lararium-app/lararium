package nuntius

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lararium-app/lararium/internal/penatus"
)

// Engine runs one Telegram-initiated turn on a session and reports its
// outcome. The hearthd wiring implements it over the same loop engine,
// session logs, key registry, and ApprovalHub as the web surface —
// nuntius never holds a second state machine (spec §1, N6).
type Engine interface {
	RunTurn(ctx context.Context, req TurnReq) TurnResult
}

// TurnReq is one engine turn initiated from Telegram.
type TurnReq struct {
	SessionID string
	Text      string
	UpdateID  int64 // A3: tags both msg events
	// OnDelta receives the assistant text so far (full text to date,
	// not an increment) — the Stream's input contract.
	OnDelta func(textSoFar string)
}

// TurnResult is the engine's terminal report. Err classifies the
// abort (nil means the turn completed); Text is the assistant text
// actually streamed, which the bridge renders verbatim (§6: the
// persisted msg event, when present, must equal it — V4).
type TurnResult struct {
	Text string
	Err  error
}

// Sessions is the session-store view the bridge needs for /new and
// /sessions (the wiring adapts surface.PenatusSource).
type Sessions interface {
	List() ([]SessionRef, error)
	Create(title string, modelPin string) (SessionRef, error)
}

// SessionRef is one session as the bridge sees it.
type SessionRef struct {
	ID    string
	Title string
}

// WireDeps are the hub hooks the bridge needs, injected as plain
// functions so this package never imports internal/surface (the
// wiring in cmd/hearthd adapts the real hub).
type WireDeps struct {
	SetFanout         func(pending func(id, sessionID, name, argsSummary string), terminal func(id, sessionID, state, reason, source string))
	SetBridgeLive     func(live func() bool)
	SetBridgeTimeout  func(d time.Duration)
	MarkUndeliverable func(approvalID string)
	// ResolveApproval settles a card tap through the hub's frozen
	// state machine, attributing source "telegram". Returns the hub's
	// status: 200 resolved, 404 unknown (session or id), 410 already
	// resolved (§7.2: mirrors the HTTP codes).
	ResolveApproval func(sessionID, approvalID string, allow bool) int
	// SessionOf maps an approval id to its session server-side
	// (§7.2: the callback payload never names a session).
	SessionOf func(approvalID string) (string, bool)
	// PendingApprovals counts pending approvals for /status.
	PendingApprovals func(sessionID string) int
	// CancelTurn is the same path as POST /v1/sessions/{id}/cancel
	// (§5): hub cancel, queue release.
	CancelTurn func(sessionID string) bool
	// InFlight reports the hub's per-session turn gate.
	InFlight func(sessionID string) bool
	// ModelRef is the active model for /status ("provider/model").
	ModelRef func() string
}

// Deps wires the bridge to the process it lives in (spec §2: nuntius
// runs inside hearthd, not as a separate daemon).
type Deps struct {
	Cfg  Config
	Home string // the hearth home; bridge state lives under <home>/nuntius
	// TG is the raw transport (the real BotAPI or the test fake). The
	// bridge wraps it in flood-gated sends itself (N9).
	TG     Transport
	Engine Engine
	Sess   Sessions
	// SessionDir locates a session's penatus log (update_id dedupe on
	// replay, V11(b); /status token counts).
	SessionDir func(sessionID string) string
	Wire       WireDeps
	// Log receives every bridge line. The wiring passes a logger
	// scrubbed of the bot token (N4); nil uses stderr.
	Log *log.Logger

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// Frozen static lines (§3/§5/§6/§2 — byte-exact, asserted in tests).
const (
	// MsgStillWorking is the reject-with-feedback line when both
	// slots are occupied (§6 backpressure).
	MsgStillWorking = "still working — /status"
	// MsgTurnRunning is the mid-turn refusal for session-switching
	// commands (§5 turn gate).
	MsgTurnRunning = "turn running — /cancel to stop it"
	// MsgCancelled is the /cancel reply with an empty queue (§5).
	MsgCancelled = "cancelled"
	// MsgNothingRunning is the /cancel reply with no turn (§5).
	MsgNothingRunning = "nothing running"
	// MsgTooLong is the oversized-message refusal (§5: 16 KiB cap).
	MsgTooLong = "message too long"
	// MsgShuttingDown is the shutdown refusal for queued messages (§2).
	MsgShuttingDown = "daemon shutting down"
	// MsgUnknownRef is the /sessions <ref> ambiguous/unknown answer.
	MsgUnknownRef = "unknown session"
	// MsgAmbiguousRef is the /sessions <ref> prefix-collision answer.
	MsgAmbiguousRef = "ambiguous session ref"
	// MsgNoSessions is the empty-list and store-error answer for
	// /sessions and the /new failure answer.
	MsgNoSessions = "no sessions"
	// MsgCancelledQueued is the /cancel reply when the queue is
	// non-empty (§6: cancelling releases the next message).
	MsgCancelledQueued = "cancelled — 1 queued message starting"
	// MsgGreeting is the /start greeting (§5); the pairing-required
	// line is appended only while unpaired.
	MsgGreeting = "Lararium hearth bridge.\n/new [title] · /sessions [ref] · /status · /cancel · /help"
	// MsgHelp is the static /help command list (§5).
	MsgHelp = "/start · /pair <code> · /new [title] · /sessions [ref] · /status · /cancel · /help"

	// ToastAllowed is the resolved-allow toast.
	ToastAllowed = "allowed ✓"
	// ToastDenied is the resolved-deny toast.
	ToastDenied = "denied ✗"
	// ToastAlready is the lost-race toast (already resolved).
	ToastAlready = "already resolved"
	// ToastNotPending is the unknown/stale-card toast.
	ToastNotPending = "not pending"
	// ToastPrivate is the non-owner callback toast (same words as
	// MsgPrivate — a toast, not a message).
	ToastPrivate = "this bot is private"

	// LogUnknownCommand is the rejected-command line (name, update).
	LogUnknownCommand = "nuntius: unknown command %q (update %d)"
	// LogForeignCallback is the unresolvable-card tap line.
	LogForeignCallback = "nuntius: callback for unknown approval %q (update %d)"
	// LogSessionFallback is the unknown-active-session line (§5).
	LogSessionFallback = "nuntius: active session %s unknown; falling back to main"
	// LogTokenRemedy is the 401 stop line (env var name).
	LogTokenRemedy = "nuntius: telegram rejected the bot token (401) — re-check %s and restart; polling stopped" //nolint:gosec // env var NAME in text, no secret
	// LogStartup is the one-line startup identity (hash prefix, V12).
	LogStartup = "nuntius: polling as bot token %s… (sha256 prefix)"
	// LogDormant is the enabled-without-token line.
	LogDormant = "nuntius: enabled but token missing — bridge dormant"
	// LogDisabled is the bridge-off line.
	LogDisabled = "nuntius: disabled — bridge dormant"
	// LogCardStale is the failed card-cleanup line (§7.5 cosmetic).
	LogCardStale = "nuntius: approval card %s cleanup failed; leaving stale card (cosmetic, §7.5)"
	// LogReplyFailed is the send-failure line (never a secret).
	LogReplyFailed          = "nuntius: reply failed: %v"
	LogShutdownNoticeFailed = "nuntius: shutdown notice failed: %v"
	LogInboxAppendFailed    = "nuntius: inbox append failed: %v (update will be redelivered)"
	LogOffsetFailed         = "nuntius: offset persist failed: %v"
	LogDoneFailed           = "nuntius: done-tombstone failed for update %d: %v"
	LogTurnFailed           = "nuntius: turn failed: %v"
	LogPollBackoff          = "nuntius: getUpdates failed (%d): %v; backing off %s"
	LogOwnersUnreadable     = "nuntius: owners.json unreadable: %v"
	LogReplayAborted        = "nuntius: replay aborted: %v"
	LogStateRefused         = "nuntius: state unreadable: %v — bridge not started (web surface unaffected)"
)

// maxInboundBytes is the §5 seam cap (Telegram itself caps at 4096).
const maxInboundBytes = 16 << 10

// nonOwnerGap is the frozen per-user refusal throttle (§3: one static
// refusal per user per 5 min).
const nonOwnerGap = 5 * time.Minute

// cardAttempts / cardBackoff: card delivery gets 3 attempts ≈ 8 s of
// backoff before MarkUndeliverable (§7.6).
const (
	cardAttempts = 3
	cardBackoff  = 3 * time.Second
)

// backoffSteps is the §2 poll-error ladder: 1s → 2s → 5s → 15s → 60s
// cap (jittered).
var backoffSteps = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 15 * time.Second, 60 * time.Second}

// approvalIDRe mirrors the hub's frozen id shape (§7.2): the callback
// parser refuses anything else before the hub is consulted.
var approvalIDRe = regexp.MustCompile(`^a_[0-9A-Za-z]{10,32}$`)

// Bridge is the Telegram supervisor: one goroutine owns the poll loop
// and every state mutation it touches; turns run on queue goroutines
// (§2 — polling never blocks on a model call).
type Bridge struct {
	deps Deps
	cfg  Config

	tg     Transport // flood-gated wrapper around deps.TG
	flood  *FloodControl
	inbox  *Inbox
	state  *StateFile
	pair   *PairStore
	muter  *Muter
	pairRL *Limiter
	privRL *Limiter
	log    *log.Logger

	cardsMu sync.Mutex
	cards   map[string]*card // approval id → live card

	custodyMu sync.Mutex
	custody   *custodyFeed // custody-card sibling feed (WireCustody)
	custodyW  CustodyWire

	queuesMu sync.Mutex
	queues   map[string]*turnQueue // chat id → FIFO

	startedMu sync.Mutex
	started   bool // hub presence probe reads this (A1)

	stopOnce sync.Once
	stop     chan struct{}
}

// card is a live approval card awaiting its terminal transition.
type card struct {
	chatID string
	msgID  int64
}

// New validates the config and opens the bridge-owned state files. A
// corrupt state.json or owners.json returns the N8 error — the caller
// must keep the bridge dormant while the web surface keeps serving
// (V15: the bridge refuses, the daemon does not die).
func New(deps Deps) (*Bridge, error) {
	cfg := deps.Cfg
	if _, err := cfg.Normalize(); err != nil {
		return nil, err
	}
	if deps.now == nil {
		deps.now = time.Now
	}
	if deps.sleep == nil {
		deps.sleep = sleepCtx
	}
	lg := deps.Log
	if lg == nil {
		lg = log.New(os.Stderr, "", log.LstdFlags)
	}

	dir := filepath.Join(deps.Home, "nuntius")
	inbox, err := OpenInbox(dir)
	if err != nil {
		return nil, err
	}
	sf, err := NewStateFile(dir)
	if err != nil {
		return nil, err
	}
	if _, err := sf.Load(); err != nil { // V15
		return nil, err
	}
	ps, err := NewPairStore(dir)
	if err != nil {
		return nil, err
	}
	if _, err := ps.Load(); err != nil { // V15
		return nil, err
	}

	b := &Bridge{
		deps:   deps,
		cfg:    cfg,
		flood:  NewFloodControl(deps.now, deps.sleep),
		inbox:  inbox,
		state:  sf,
		pair:   ps,
		muter:  NewMuter(cfg.PairFailWindow, cfg.PairMute, 5, deps.now),
		pairRL: NewLimiter(*cfg.PairReplyGap, deps.now),
		privRL: NewLimiter(nonOwnerGap, deps.now),
		log:    lg,
		cards:  map[string]*card{},
		queues: map[string]*turnQueue{},
		stop:   make(chan struct{}),
	}
	b.tg = &gated{T: deps.TG, fc: b.flood}
	return b, nil
}

// gated wraps a Transport so every send rides the flood buckets (N9):
// the per-chat bucket caps at ~1/s, a 429 pauses both send buckets
// for retry_after, and polling rides its own path — never blocked by
// a send pause (V9). The real BotAPI gates internally too; double
// gating is idempotent (token buckets, min-of pauses).
type gated struct {
	T  Transport
	fc *FloodControl
}

// GetUpdates passes polling through un-throttled (§1: only sends are gated).
func (g *gated) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	return g.T.GetUpdates(ctx, offset)
}

// SendMessage sends after the flood gate, recording a global pause on 429 (§1).
func (g *gated) SendMessage(ctx context.Context, chatID, text string, kb *Keyboard) (*Message, error) {
	if err := g.fc.Send(ctx, chatID); err != nil {
		return nil, err
	}
	msg, err := g.T.SendMessage(ctx, chatID, text, kb)
	g.pauseOn429(err, chatID)
	return msg, err
}

// EditMessageText edits after the flood gate, recording a global pause on 429 (§1).
func (g *gated) EditMessageText(ctx context.Context, chatID string, messageID int64, text string, kb *Keyboard) error {
	if err := g.fc.Send(ctx, chatID); err != nil {
		return err
	}
	err := g.T.EditMessageText(ctx, chatID, messageID, text, kb)
	g.pauseOn429(err, chatID)
	return err
}

// SendChatAction typings pass the gate too — they cost quota like sends.
func (g *gated) SendChatAction(ctx context.Context, chatID, action string) error {
	if err := g.fc.Send(ctx, chatID); err != nil {
		return err
	}
	return g.T.SendChatAction(ctx, chatID, action)
}

// AnswerCallback toasts bypass the gate: every callback must be answered (N10).
func (g *gated) AnswerCallback(ctx context.Context, callbackID, text string) error {
	return g.T.AnswerCallback(ctx, callbackID, text)
}

// DeleteMessageReplyMarkup strips a keyboard after the flood gate.
func (g *gated) DeleteMessageReplyMarkup(ctx context.Context, chatID string, messageID int64) error {
	if err := g.fc.Send(ctx, chatID); err != nil {
		return err
	}
	return g.T.DeleteMessageReplyMarkup(ctx, chatID, messageID)
}

func (g *gated) pauseOn429(err error, chatID string) {
	if Is429(err) {
		if d := RetryAfter(err); d > 0 {
			g.fc.PauseGlobal(d)
			g.fc.PauseChat(chatID, d)
		}
	}
}

// DormantLine reports the loud startup line for a bridge that will
// not run (§2: enabled AND token, or dormant with a line — never
// silent). "" means the bridge will run.
func DormantLine(cfg Config, getenv func(string) string) string {
	if !cfg.Enabled {
		return LogDisabled
	}
	if _, err := cfg.Token(getenv); err != nil {
		return LogDormant
	}
	return ""
}

// Wire installs the hub hooks (fan-out, presence probe, bridge
// timeout). Call once at startup before Start.
func (b *Bridge) Wire(w WireDeps) {
	if w.SetBridgeTimeout != nil {
		w.SetBridgeTimeout(b.cfg.ApprovalTimeout)
	}
	if w.SetBridgeLive != nil {
		w.SetBridgeLive(b.Live)
	}
	if w.SetFanout != nil {
		w.SetFanout(b.approvalPending, b.approvalTerminal)
	}
}

// Live reports bridge presence for the hub's approval probe (A1):
// live while the daemon polls.
func (b *Bridge) Live() bool {
	b.startedMu.Lock()
	defer b.startedMu.Unlock()
	return b.started
}

// Start launches the supervisor. The caller must have verified a
// token is present (spec §2); the startup line carries only the hash
// prefix (V12/N4).
func (b *Bridge) Start(ctx context.Context, token string) {
	b.startedMu.Lock()
	b.started = true
	b.startedMu.Unlock()

	b.log.Printf(LogStartup, TokenHashPrefix(token))
	go b.supervise(ctx)
}

// Stop ends polling, refuses queued-but-unstarted messages with the
// static shutdown line (best-effort, 5 s budget, §2). Pending
// approvals resolve via the hub's own shutdown path (denied:shutdown,
// §7.3) — the bridge adds nothing; in-flight turns finalize per the
// terminal-event rule as their engine calls return.
func (b *Bridge) Stop() {
	b.stopOnce.Do(func() { close(b.stop) })
	b.stopCustody()
	b.startedMu.Lock()
	b.started = false
	b.startedMu.Unlock()

	b.queuesMu.Lock()
	qs := make([]*turnQueue, 0, len(b.queues))
	for _, q := range b.queues {
		qs = append(qs, q)
	}
	b.queuesMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, q := range qs {
		for _, j := range q.close() {
			b.markDone(j.rec, nil)
			if _, err := b.tg.SendMessage(ctx, j.chatID, MsgShuttingDown, nil); err != nil {
				b.log.Printf(LogShutdownNoticeFailed, err)
			}
		}
	}
}

// stopping reports whether shutdown has begun (checked between
// updates in a batch; queued-but-unstarted messages are drained by
// Stop with the shutdown line).
func (b *Bridge) stopping() bool {
	select {
	case <-b.stop:
		return true
	default:
		return false
	}
}

// supervise owns the poll loop (§2).
func (b *Bridge) supervise(ctx context.Context) {
	st, err := b.state.Load()
	if err != nil {
		b.log.Printf(LogStateRefused, err)
		return
	}

	// §2 step 4: replay not-done inbox records in inbox order BEFORE
	// the first poll, so a replayed /sessions switch re-applies
	// before the messages that followed it.
	if err := b.replay(ctx); err != nil {
		b.log.Printf(LogReplayAborted, err)
		return
	}

	// The offset is a server-side watermark: the inbox is the source
	// of truth for what was durably accepted, so polling resumes at
	// max(state.json, inbox high-water) — an update durable in the
	// inbox but never acked must not re-enter (the dedupe absorbs it
	// either way; skipping is cheaper).
	offset := st.Offset
	if seen := b.inbox.LastSeenUpdateID(); seen > offset {
		offset = seen
	}

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.stop:
			return
		default:
		}

		// Freshness doctrine (§3): owners.json re-read every cycle —
		// pair/unpair take effect on the next poll, no signals.
		owners, err := b.pair.Load()
		if err != nil {
			b.log.Printf(LogOwnersUnreadable, err)
			owners = Owners{}
		}

		updates, err := b.tg.GetUpdates(ctx, offset+1)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrNotFound) {
				b.log.Printf(LogTokenRemedy, b.cfg.BotTokenEnv)
				return
			}
			if Is429(err) {
				b.flood.PauseGlobal(RetryAfter(err))
			}
			failures++
			d := backoff(failures)
			b.log.Printf(LogPollBackoff, failures, err, d)
			if err := b.deps.sleep(ctx, d); err != nil {
				return
			}
			continue
		}
		failures = 0
		if len(updates) == 0 {
			continue
		}

		// §2 step 1: durability first. The offset advances only over
		// updates durably in the inbox; an append failure leaves the
		// rest un-acked for redelivery.
		highest, err := b.accept(updates)
		if err != nil {
			b.log.Printf(LogInboxAppendFailed, err)
			if err2 := b.deps.sleep(ctx, time.Second); err2 != nil {
				return
			}
			continue
		}
		if highest > offset {
			st.Offset = highest
			if err := b.state.Save(st); err != nil {
				b.log.Printf(LogOffsetFailed, err)
				continue // do not execute past a watermark we cannot re-derive
			}
			offset = highest
		}
		b.handleBatch(ctx, updates, owners)
	}
}

// accept appends every update to its inbox record (§2 step 1,
// sanitized inside Append). Returns the highest update id durably
// present — freshly appended OR already seen (the dedupe case; a
// skipped duplicate must not strand the watermark).
func (b *Bridge) accept(updates []Update) (int64, error) {
	highest := int64(0)
	for _, u := range updates {
		rec, ok := recordFor(u)
		if !ok {
			// An update shape the spec has not decided: acknowledge
			// and ignore (P4), durably done with session null.
			rec = Record{UpdateID: u.UpdateID, Kind: KindTurn, Done: true}
		}
		if _, err := b.inbox.Append(rec); err != nil {
			return highest, err
		}
		if u.UpdateID > highest {
			highest = u.UpdateID
		}
	}
	return highest, nil
}

// recordFor turns an update into an inbox record. The payload is
// sanitized again inside Append (N3 belt: a /pair code never hits
// disk); an unknown callback shape gets no record — it drops.
func recordFor(u Update) (Record, bool) {
	env := u.Envelope()
	switch {
	case u.CallbackQuery != nil:
		if !strings.HasPrefix(env.Text, "ap:") && !strings.HasPrefix(env.Text, "cu:") {
			return Record{}, false
		}
		return Record{UpdateID: u.UpdateID, Kind: KindCallback, Payload: env.Text}, true
	case u.Message != nil:
		kind := KindTurn
		if CommandName(u.Message.Text) != "" {
			kind = KindCommand
		}
		return Record{UpdateID: u.UpdateID, Kind: kind, Payload: env.Text}, true
	}
	return Record{}, false
}

// handleBatch executes freshly accepted updates in arrival order
// (§2 step 2). The gate runs on the envelope only, before anything
// else (N2).
func (b *Bridge) handleBatch(ctx context.Context, updates []Update, owners Owners) {
	for _, u := range updates {
		if b.stopping() {
			return
		}
		rec, ok := recordFor(u)
		if !ok {
			continue // already durably done by accept
		}
		b.process(ctx, u, rec, owners)
	}
}

// replay re-processes not-done records in inbox order (§2 step 4).
// A record was admitted the first time it arrived, so replay rebuilds
// the owner envelope from owners.json + the persisted owner chat and
// re-runs the gate (fail-closed: unpaired or ownerless state stops
// the replay rather than guessing).
func (b *Bridge) replay(ctx context.Context) error {
	recs, err := b.inbox.Replay()
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return nil
	}
	owners, err := b.pair.Load()
	if err != nil {
		return err
	}
	st, err := b.state.Load()
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if b.stopping() {
			return nil
		}
		if len(owners.OwnerIDs) == 0 || st.OwnerChat == "" {
			// Cannot reconstruct who/where: leave the record not-done
			// and stop; pairing or repair re-enables replay.
			return fmt.Errorf("cannot replay update %d: unpaired or owner chat unknown", rec.UpdateID)
		}
		env := envelopeFromRecord(rec, owners.OwnerIDs[0], st.OwnerChat)
		switch Decide(env, owners) {
		case Admit:
			b.process(ctx, rebuildUpdate(rec, owners.OwnerIDs[0], st.OwnerChat), rec, owners)
		case PairAttempt:
			b.processPairReplay(ctx, rec)
		case Drop, ReplyPairingRequired, ReplyPrivate:
			b.markDone(rec, nil) // refused on replay: terminal, session null
		}
	}
	return nil
}

// process applies the gate and routes one fresh update (§3: the gate
// sees only the envelope).
func (b *Bridge) process(ctx context.Context, u Update, rec Record, owners Owners) {
	env := u.Envelope()
	switch Decide(env, owners) {
	case Drop:
		b.markDone(rec, nil)
		b.log.Printf("nuntius: dropped update %d (%s) from %s", env.UpdateID, env.ChatType, env.FromID)
	case ReplyPairingRequired:
		b.markDone(rec, nil)
		b.log.Printf("nuntius: refused unpaired update %d from %s (%s)", env.UpdateID, env.FromID, env.Text)
		if !env.IsCallback && b.pairRL.Allow(env.FromID) {
			b.replyText(ctx, env.ChatID, MsgPairingRequired)
		}
	case ReplyPrivate:
		b.markDone(rec, nil)
		b.log.Printf("nuntius: refused non-owner update %d from %s", env.UpdateID, env.FromID)
		if env.IsCallback {
			b.toast(ctx, callbackIDOf(u), ToastPrivate) // toast only, §3
		} else if b.privRL.Allow(env.FromID) {
			b.replyText(ctx, env.ChatID, MsgPrivate)
		}
	case PairAttempt:
		b.handlePair(ctx, u, rec)
	case Admit:
		b.route(ctx, u, rec)
	}
}

// handlePair evaluates /pair <code> while unpaired (§3). The code was
// never persisted, so a replay never reaches here with a live code:
// processPairReplay answers the redaction rule instead.
func (b *Bridge) handlePair(ctx context.Context, u Update, rec Record) {
	env := u.Envelope()
	userID := env.FromID
	chatID := env.ChatID
	res, err := b.pair.Redeem(userID, PairCode(env.Text), b.deps.now())
	if err != nil {
		b.log.Printf("nuntius: pair redeem failed: %v", err)
		b.markDone(rec, nil)
		return
	}
	switch res {
	case RedeemPaired:
		b.log.Printf(LogPairedOwner, userID)
		b.rememberOwnerChat(chatID) // replay routing needs the chat (§2)
		b.replyText(ctx, chatID, MsgPaired)
	case RedeemAlready:
		// Race between the gate's owners load and the redeem: §3's
		// already-paired owner path.
		b.replyText(ctx, chatID, MsgAlreadyPaired)
	case RedeemInvalid:
		b.log.Printf("nuntius: invalid pairing attempt from %s (name %q, update %d)", env.FromID, env.FirstName, env.UpdateID)
		muted := b.muter.Muted(userID)
		b.muter.RecordFail(userID)
		if !muted && b.pairRL.Allow(userID) {
			b.replyText(ctx, chatID, MsgInvalidCode)
		}
	}
	b.markDone(rec, nil)
}

// processPairReplay answers a replayed /pair record: the stored
// payload is redacted, so the code can never match — `invalid code`
// to the owner chat, failing closed is right (N3, V2).
func (b *Bridge) processPairReplay(ctx context.Context, rec Record) {
	b.muter.RecordFail(replayFailIdentity(rec))
	b.markDone(rec, nil)
	st, err := b.state.Load()
	if err == nil && st.OwnerChat != "" {
		b.replyText(ctx, st.OwnerChat, MsgInvalidCode)
	}
}

func replayFailIdentity(rec Record) string { return fmt.Sprintf("replay:%d", rec.UpdateID) }

// route sends an admitted update to its handler (§5).
func (b *Bridge) route(ctx context.Context, u Update, rec Record) {
	if u.CallbackQuery != nil {
		b.handleCallback(ctx, u, rec)
		return
	}
	m := u.Message
	chatID := m.Chat.ID
	if len(m.Text) > maxInboundBytes {
		b.replyText(ctx, chatID, MsgTooLong)
		b.markDone(rec, nil)
		return
	}
	if m.Text == "" {
		// Inbound media in v1: refuse with the static line (§11).
		b.replyText(ctx, chatID, MsgMediaUnsupported)
		b.markDone(rec, nil)
		return
	}
	if CommandName(m.Text) != "" {
		b.handleCommand(ctx, m, rec)
		return
	}
	b.submitTurn(ctx, rec, m)
}

// submitTurn applies the §6 backpressure gate and hands the message
// to the chat's FIFO.
func (b *Bridge) submitTurn(ctx context.Context, rec Record, m *Message) {
	q := b.queueFor(m.Chat.ID)
	j := job{rec: rec, chatID: m.Chat.ID, text: m.Text}
	if q.submit(j) {
		return
	}
	b.replyText(ctx, m.Chat.ID, MsgStillWorking)
	b.markDone(rec, nil) // rejected: fate known, terminal, session null
}

func (b *Bridge) queueFor(chatID string) *turnQueue {
	b.queuesMu.Lock()
	defer b.queuesMu.Unlock()
	q, ok := b.queues[chatID]
	if !ok {
		q = newTurnQueue(*b.cfg.QueueDepth, b.runTurnedJob)
		b.queues[chatID] = q
	}
	return q
}

// runTurnedJob executes one queued turn (queue goroutine).
func (b *Bridge) runTurnedJob(j job) {
	sessID := b.resolveSession(b.activeSession())

	// V11(b): a replayed update whose update_id already rides a
	// committed msg event in this session's log is marked done
	// without re-running — exactly one user+assistant pair.
	if b.updateCommitted(sessID, j.rec.UpdateID) {
		b.markDone(j.rec, &sessID)
		return
	}

	stream := NewStream(b.tg, j.chatID, b.cfg.EditInterval, b.deps.now)
	cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = b.tg.SendChatAction(cctx, j.chatID, "typing")
	cancel()

	res := b.deps.Engine.RunTurn(context.Background(), TurnReq{
		SessionID: sessID,
		Text:      j.text,
		UpdateID:  j.rec.UpdateID,
		OnDelta:   func(text string) { stream.Delta(context.Background(), text) },
	})

	if res.Err != nil {
		reason := "engine error"
		switch {
		case errors.Is(res.Err, context.DeadlineExceeded):
			reason = "turn timeout"
		case errors.Is(res.Err, context.Canceled):
			reason = "cancelled"
		}
		stream.TurnAborted(context.Background(), res.Text, reason)
		b.markDone(j.rec, &sessID)
		b.log.Printf(LogTurnFailed, res.Err)
		return
	}
	stream.TurnDone(context.Background(), res.Text)
	b.markDone(j.rec, &sessID)
}

// updateCommitted reports whether a session's log already carries a
// msg event tagged with this update_id (A3 dedupe, V11(b)).
func (b *Bridge) updateCommitted(sessionID string, updateID int64) bool {
	if b.deps.SessionDir == nil || updateID == 0 {
		return false
	}
	log, err := penatus.OpenLog(b.deps.SessionDir(sessionID))
	if err != nil {
		return false
	}
	for _, ev := range log.Live() {
		if ev.T != "msg" {
			continue
		}
		raw, ok := ev.Fields["update_id"]
		if !ok {
			continue
		}
		var id int64
		if _, err := fmt.Fscan(strings.NewReader(string(raw)), &id); err == nil && id == updateID {
			return true
		}
	}
	return false
}

// markDone appends the tombstone recording the session actually used
// (nil for updates that terminate outside any session, §2).
func (b *Bridge) markDone(rec Record, sessionID *string) {
	if err := b.inbox.MarkDone(rec.UpdateID, sessionID); err != nil {
		b.log.Printf(LogDoneFailed, rec.UpdateID, err)
	}
}

// activeSession reads the chat→session pointer (§5: one pointer in
// state.json, default main).
func (b *Bridge) activeSession() string {
	st, err := b.state.Load()
	if err != nil || st.ActiveSession == "" {
		return "main"
	}
	return st.ActiveSession
}

// resolveSession validates the pointer at execution: a pointer to a
// deleted/unknown session falls back to main with a log line, never a
// crash (§5).
func (b *Bridge) resolveSession(id string) string {
	if id == "" || id == "main" {
		return "main"
	}
	refs, err := b.deps.Sess.List()
	if err != nil {
		return id // store unreadable: the engine's own gate decides
	}
	for _, r := range refs {
		if r.ID == id {
			return id
		}
	}
	b.log.Printf(LogSessionFallback, id)
	return "main"
}

// setActiveSession persists the pointer (atomic replace + fsync at
// switch time; replay re-applies switches idempotently, §5).
func (b *Bridge) setActiveSession(id string) error {
	st, err := b.state.Load()
	if err != nil {
		st = State{}
	}
	st.ActiveSession = id
	return b.state.Save(st)
}

// rememberOwnerChat persists the chat pairing bound (replay routing
// and shutdown notices need it; v1 has exactly one chat).
func (b *Bridge) rememberOwnerChat(chatID string) {
	st, err := b.state.Load()
	if err != nil {
		st = State{}
	}
	if st.OwnerChat == chatID {
		return
	}
	st.OwnerChat = chatID
	if err := b.state.Save(st); err != nil {
		b.log.Printf("nuntius: owner chat persist failed: %v", err)
	}
}

// replyText is a plain bucket-gated send; errors are logged, never
// fatal — every message's fate is known via the inbox.
func (b *Bridge) replyText(ctx context.Context, chatID, text string) {
	if chatID == "" {
		return
	}
	if _, err := b.tg.SendMessage(ctx, chatID, text, nil); err != nil {
		b.log.Printf(LogReplyFailed, err)
	}
}

func (b *Bridge) toast(ctx context.Context, callbackID, text string) {
	if callbackID == "" {
		return
	}
	if err := b.tg.AnswerCallback(ctx, callbackID, text); err != nil {
		b.log.Printf("nuntius: toast failed: %v", err)
	}
}

func callbackIDOf(u Update) string {
	if u.CallbackQuery != nil {
		return u.CallbackQuery.ID
	}
	return ""
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// backoff maps the failure count onto the §2 ladder with ±10% jitter.
func backoff(failures int) time.Duration {
	idx := failures - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(backoffSteps) {
		idx = len(backoffSteps) - 1
	}
	d := backoffSteps[idx]
	jitter := time.Duration(rand.Int64N(int64(d)/5 + 1)) //nolint:gosec // poll-backoff jitter: no secret material (V14 fairness only)
	return d - d/10 + jitter
}

// lastTurnUsage reads the newest msg event's usage from a session log
// for /status (§5: token counts come from the session log, never from
// nuntius state). ok=false when the session has no committed turn.
func (b *Bridge) lastTurnUsage(sessionID string) (inTok, outTok int, ok bool) {
	if b.deps.SessionDir == nil {
		return 0, 0, false
	}
	log, err := penatus.OpenLog(b.deps.SessionDir(sessionID))
	if err != nil {
		return 0, 0, false
	}
	for _, ev := range log.Live() {
		if ev.T != "msg" {
			continue
		}
		role := strings.Trim(string(ev.Fields["role"]), `"`)
		if role != "assistant" {
			continue
		}
		var usage struct {
			In  int `json:"in"`
			Out int `json:"out"`
		}
		if err := jsonUnmarshal(ev.Fields["usage"], &usage); err != nil {
			continue
		}
		inTok, outTok, ok = usage.In, usage.Out, true
	}
	return inTok, outTok, ok
}
