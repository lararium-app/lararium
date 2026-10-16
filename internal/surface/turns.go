package surface

import (
	"context"
	"encoding/json"
	"errors"
	stdlog "log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lararium-app/lararium/internal/keystore"
	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

// ErrTurnInFlight is returned when a turn is already running for the
// session (spec §5 concurrency gate).
var ErrTurnInFlight = errors.New("turn in flight")

// runawayCapBytes aborts a turn that generates this many streamed bytes
// (spec §5: cap enforced engine-side so a disconnected client cannot let
// a runaway loop burn tokens forever).
const runawayCapBytes = 4 << 20

// turn is a running engine loop for one session. Only the cancel func is
// stored: the contexts themselves live in the handler's scope.
type turn struct {
	cancel context.CancelFunc
}

// Hub runs HTTP-initiated turns through the frozen loop engine, one turn
// per session at a time (spec §5 concurrency gate).
type Hub struct {
	cfg                  ServeConfig
	hearthHome           string
	maxTokens            int
	compactionTriggerPct int
	tools                []loop.Tool
	router               *router.Router
	ap                   *ApprovalHub
	// keys is the live key registry handed to every Session (nil-safe).
	keys           *keystore.Registry
	mu             sync.Mutex
	running        map[string]*turn
	singleWriterMu sync.RWMutex
}

// TrySingleWriterLock attempts to acquire the hub's single-writer serialization point non-blockingly (PENATUS §4.5.3.3).
func (h *Hub) TrySingleWriterLock() (func(), bool) {
	if !h.singleWriterMu.TryLock() {
		return nil, false
	}
	return func() {
		h.singleWriterMu.Unlock()
	}, true
}

// SingleWriterLock acquires the hub's single-writer serialization point non-blockingly (PENATUS §4.5.3.1, §4.5.3.3).
func (h *Hub) SingleWriterLock() (func(), bool) {
	return h.TrySingleWriterLock()
}

// RLock acquires the hub's writer RLock (PENATUS §4.5.3.1, M9).
func (h *Hub) RLock() func() {
	h.singleWriterMu.RLock()
	return func() {
		h.singleWriterMu.RUnlock()
	}
}

// AttachKeys gives the hub the live key registry (KEYS-SPEC K6).
func (h *Hub) AttachKeys(r *keystore.Registry) { h.keys = r }

// Approvals returns the hub's approval coordinator (NUNTIUS-SPEC §7.1).
func (h *Hub) Approvals() *ApprovalHub { return h.ap }

// AuditApprovalFor maps an engine approval string to the frozen
// SURFACE-SPEC §6 audit object with the "telegram" fallback channel
// (NUNTIUS-SPEC §7.1).
func (h *Hub) AuditApprovalFor(sessionID, approval string) (json.RawMessage, bool) {
	return auditApprovalFor(h.ap, sessionID, approval, "telegram")
}

// NewHub builds the turn hub. hearthHome is the persona root; session
// logs live under <hearthHome>/sessions/<id> exactly where repl.go puts
// them, so REPL and API see the same logs.
func NewHub(cfg ServeConfig, hearthHome string, maxTokens int, compactionTriggerPct int, tools []loop.Tool, rt *router.Router, ap *ApprovalHub) *Hub {
	// Fill spec defaults here too: callers build the Hub before
	// ListenAndServe, and a zero TurnTimeout would arm an already-expired
	// context on every turn. Normalize only errors on a non-loopback
	// bind without allowed_hosts, which ListenAndServe reports properly.
	_ = cfg.Normalize()
	return &Hub{
		cfg:                  cfg,
		hearthHome:           hearthHome,
		maxTokens:            maxTokens,
		compactionTriggerPct: compactionTriggerPct,
		tools:                tools,
		router:               rt,
		ap:                   ap,
		running:              make(map[string]*turn),
	}
}

// InFlight reports whether a turn is running for the session (the
// in_flight flag of GET /v1/sessions/{id}/events, spec §5 catch-up).
func (h *Hub) InFlight(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.running[sessionID]
	return ok
}

// Shutdown resolves every pending approval denied:shutdown (source
// "shutdown", A2) and cancels every running turn (V12): a cancelled
// turn commits no partial assistant event, and pending approvals never
// outlive the process.
func (h *Hub) Shutdown() {
	h.ap.DenyAllAll("shutdown", "shutdown")
	h.mu.Lock()
	turns := make([]*turn, 0, len(h.running))
	for _, t := range h.running {
		turns = append(turns, t)
	}
	h.mu.Unlock()
	for _, t := range turns {
		t.cancel()
	}
}

// Cancel aborts the running turn for the session, if any. Per
// SURFACE-SPEC §5 (A5): every approval still pending on that turn
// resolves to denied (internal cause "cancelled") BEFORE the turn's own
// cancellation, so no approval can outlive its turn and a late decision
// POST answers 410. Like every cancelled turn no audit record is written
// — the tool never ran, and the absence of a tool_result IS the record.
func (h *Hub) Cancel(sessionID string) bool {
	// The turn handle is snapshotted under h.mu, but the fan-out and
	// cancellation run OUTSIDE it: CancelAllFor fires the bridge's
	// terminal hook, whose Telegram card edit can take ~15 s — holding
	// the hub lock across it would freeze every other session. A turn
	// that completes in the window makes both calls no-ops (cancel
	// funcs are idempotent, CancelAllFor sees nothing pending).
	h.mu.Lock()
	t, ok := h.running[sessionID]
	h.mu.Unlock()
	if !ok {
		return false
	}
	t.cancel()
	h.ap.CancelAllFor(sessionID, "web")
	return true
}

// SSE event payload shapes (spec §5). Concrete structs, not map[string]any:
// marshalling them cannot fail and the wire shape stays reviewable.
type deltaPayload struct {
	Text string `json:"text"`
}

type approvalPayload struct {
	ApprovalID  string `json:"approval_id"`
	Name        string `json:"name"`
	ArgsSummary string `json:"args_summary"`
}

type toolStartPayload struct {
	CallID string `json:"call_id"`
	Name   string `json:"name"`
}

type toolDonePayload struct {
	CallID   string `json:"call_id"`
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Approval string `json:"approval"`
}

type turnDonePayload struct {
	Seq       int64 `json:"seq"`
	InTokens  int   `json:"in_tokens"`
	OutTokens int   `json:"out_tokens"`
}

// payloadJSON marshals a payload struct built in this file; those types
// contain only strings/bools/ints, so Marshal cannot fail.
func payloadJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("surface: payload marshal: " + err.Error())
	}
	return string(b)
}

// SessionDir is where repl.go keeps every session log: <home>/sessions/<id>
// (main included — it is NOT the hearth root).
func (h *Hub) SessionDir(sessionID string) string {
	return filepath.Join(h.hearthHome, "sessions", sessionID)
}

// turnStream is the SSE writer for one turn: serialized frames, a
// heartbeat, and an exactly-once terminal event (spec §5: every stream
// ends in exactly one turn_done or error).
type turnStream struct {
	w       http.ResponseWriter
	flusher http.Flusher
	quiet   bool

	writeMu      sync.Mutex
	started      bool
	terminalOnce sync.Once

	hbStop     chan struct{}
	hbDone     chan struct{}
	stopHBOnce sync.Once

	denyAll       func()
	disconnectOnc sync.Once
}

// newQuietTurnStream builds a quiet turnStream for headless execution
// (NUNTIUS-SPEC §7.1): no heartbeat goroutine is started, writeEvent/terminal
// update bookkeeping without writing bytes or touching w/flusher, and denyAll
// fires if disconnected is called.
func newQuietTurnStream(denyAll func()) *turnStream {
	return &turnStream{
		quiet:   true,
		denyAll: denyAll,
	}
}

// newTurnStream starts the heartbeat (a `: ping` comment every 5 s so
// proxies keep the stream warm). end() guarantees the heartbeat goroutine
// is DONE before the handler returns, so the ResponseWriter is never
// touched after that point. denyAll fires once when the listener dies.
func newTurnStream(w http.ResponseWriter, flusher http.Flusher, denyAll func()) *turnStream {
	ts := &turnStream{w: w, flusher: flusher, hbStop: make(chan struct{}), hbDone: make(chan struct{}), denyAll: denyAll}
	go func() {
		defer close(ts.hbDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ts.writeMu.Lock()
				_, _ = w.Write([]byte(": ping\n\n"))
				flusher.Flush()
				ts.writeMu.Unlock()
			case <-ts.hbStop:
				return
			}
		}
	}()
	return ts
}

// writeEvent emits one SSE frame.
func (ts *turnStream) writeEvent(event, data string) {
	ts.writeMu.Lock()
	defer ts.writeMu.Unlock()
	ts.started = true
	if ts.quiet {
		return
	}
	_, _ = ts.w.Write([]byte("event: " + event + "\n"))
	_, _ = ts.w.Write([]byte("data: " + data + "\n\n"))
	ts.flusher.Flush()
}

// terminal closes the stream with exactly one terminal event.
func (ts *turnStream) terminal(event, data string) {
	ts.terminalOnce.Do(func() { ts.writeEvent(event, data) })
}

// fail is the generic terminal error frame.
func (ts *turnStream) fail() {
	ts.terminal("error", `{"error":"turn failed"}`)
}

// failPreStream answers a setup failure: plain JSON 500 while no SSE
// frame has gone out yet, terminal error frame once the stream exists.
func (ts *turnStream) failPreStream(w http.ResponseWriter) {
	ts.writeMu.Lock()
	started := ts.started
	ts.writeMu.Unlock()
	if started {
		ts.fail()
		return
	}
	if ts.quiet || w == nil {
		ts.fail()
		return
	}
	writeJSONBody(w, http.StatusInternalServerError, `{"error":"turn failed"}`)
}

// disconnected fires the disconnect policy once: pending approvals die
// with the listener (spec §5).
func (ts *turnStream) disconnected() {
	ts.disconnectOnc.Do(ts.denyAll)
}

// end stops the heartbeat and waits for the writer goroutine to drain.
func (ts *turnStream) end() {
	if ts.quiet {
		return
	}
	ts.stopHBOnce.Do(func() { close(ts.hbStop) })
	<-ts.hbDone
}

// HandleMessage serves POST /v1/sessions/{id}/messages: one streamed
// turn ending in exactly one terminal event (delta*/tool_*/
// approval_request/turn_done|error), per spec §5.
func (h *Hub) HandleMessage(w http.ResponseWriter, r *http.Request, sessionID string) {
	// Body first: a bad request must fail as plain JSON before any SSE
	// bytes exist.
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONBody(w, http.StatusBadRequest, `{"error":"invalid request body"}`)
		return
	}

	// Atomic gate: check-and-install before writing any response.
	// Two contexts (spec §5 disconnect policy): the ENGINE survives
	// client disconnect — the model call completes and persists (P3),
	// bounded only by turn_timeout, so it deliberately does NOT inherit
	// r.Context(). The WRITER/approvals side dies with the request.
	engineCtx, cancelEngine := context.WithTimeout(context.Background(), h.cfg.TurnTimeout)
	reqCtx, cancelReq := context.WithCancel(r.Context())
	t := &turn{cancel: func() { cancelReq(); cancelEngine() }}
	h.mu.Lock()
	if _, ok := h.running[sessionID]; ok {
		h.mu.Unlock()
		cancelEngine()
		cancelReq()
		writeJSONBody(w, http.StatusConflict, `{"error":"turn in flight"}`)
		return
	}
	h.running[sessionID] = t
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.running, sessionID)
		h.mu.Unlock()
		cancelEngine()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONBody(w, http.StatusInternalServerError, `{"error":"streaming unsupported"}`)
		return
	}

	ts := newTurnStream(w, flusher, func() {
		// A1 presence: the web channel dies; each pending approval
		// survives only if a live Telegram card covers it.
		h.ap.DisconnectFor(sessionID)
	})
	defer ts.end()

	// Disconnect policy: the request context dying means the listener
	// is gone — deny pending approvals.
	go func() {
		<-reqCtx.Done()
		ts.disconnected()
	}()

	h.runTurn(engineCtx, reqCtx, w, ts, t, sessionID, req.Text)
}

// RunHeadlessTurn runs one turn with no SSE listener (NUNTIUS-SPEC
// §7.1): events are dropped, persistence and approvals are unchanged.
// channel names the src channel for msg events and audit fallback
// ("telegram"). The running-map gate is shared with web turns: a
// session already in flight returns ErrTurnInFlight. ctx bounds the
// engine like turn_timeout does for web (use min of ctx and
// h.cfg.TurnTimeout).
func (h *Hub) RunHeadlessTurn(ctx context.Context, sessionID, text, channel string, updateID int64, onDelta func(string)) error {
	timeout := h.cfg.TurnTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	engineCtx, cancelEngine := context.WithTimeout(ctx, timeout)
	defer cancelEngine()

	reqCtx := engineCtx
	t := &turn{cancel: cancelEngine}

	h.mu.Lock()
	if _, ok := h.running[sessionID]; ok {
		h.mu.Unlock()
		return ErrTurnInFlight
	}
	h.running[sessionID] = t
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.running, sessionID)
		h.mu.Unlock()
	}()

	ts := newQuietTurnStream(func() {
		h.ap.DisconnectFor(sessionID)
	})
	defer ts.end()

	return h.executeTurn(engineCtx, reqCtx, nil, ts, t, sessionID, text, channel, channel, updateID, onDelta)
}

// runTurn executes one engine loop and streams its events; when it
// returns, exactly one terminal event has left the stream.
func (h *Hub) runTurn(engineCtx, reqCtx context.Context, w http.ResponseWriter, ts *turnStream, t *turn, sessionID, text string) {
	_ = h.executeTurn(engineCtx, reqCtx, w, ts, t, sessionID, text, "api", "web", 0, nil)
}

func (h *Hub) executeTurn(
	engineCtx, reqCtx context.Context,
	w http.ResponseWriter,
	ts *turnStream,
	t *turn,
	sessionID, text, srcChannel, turnFallback string,
	updateID int64,
	userOnDelta func(string),
) error {
	approver := func(name, argsSummary string) (bool, string) {
		// Spec §5 + NUNTIUS-SPEC A1: an approval needs at least one
		// live approver channel — this SSE listener or the polling
		// bridge. The hub probes presence and applies the deny rule.
		// The decision is awaited from ANY channel; reqCtx no longer
		// short-circuits it.
		webLive := !ts.quiet && reqCtx.Err() == nil
		id, decisionCh := h.ap.RegisterOn(sessionID, name, argsSummary, webLive, func(approvalID string) {
			ts.writeEvent("approval_request", payloadJSON(approvalPayload{
				ApprovalID:  approvalID,
				Name:        name,
				ArgsSummary: argsSummary,
			}))
		})
		select {
		case decision := <-decisionCh:
			return decision, id
		case <-engineCtx.Done():
			return false, id
		}
	}

	// Runaway cap: count streamed deltas; abort the engine (not just the
	// writer) at 4 MiB and persist nothing further.
	var genBytes int
	onDelta := func(delta string) {
		genBytes += len(delta)
		if genBytes > runawayCapBytes {
			ts.fail()
			t.cancel() // aborts engine AND writer; no assistant event
			return
		}
		ts.writeEvent("delta", payloadJSON(deltaPayload{Text: delta}))
		if userOnDelta != nil {
			userOnDelta(delta)
		}
	}

	// loop's OnTool carries no call id; the approval string is unique per
	// call ("approved:a_…"/"denied:a_…"), so it doubles as the correlation
	// key the client matches tool_start against tool_done with.
	onTool := func(phase, name, approval string, ok bool) {
		switch phase {
		case "start":
			ts.writeEvent("tool_start", payloadJSON(toolStartPayload{CallID: approval, Name: name}))
		case "done":
			ts.writeEvent("tool_done", payloadJSON(toolDonePayload{
				CallID:   approval,
				Name:     name,
				OK:       ok,
				Approval: approval,
			}))
		}
	}

	h.singleWriterMu.RLock()
	defer h.singleWriterMu.RUnlock()

	sessDir := h.SessionDir(sessionID)
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		ts.failPreStream(w)
		return err
	}

	log, err := penatus.OpenLog(sessDir)
	if err != nil {
		ts.failPreStream(w)
		return err
	}

	sys, _, _ := loop.SystemPrompt(h.hearthHome)

	sess := &loop.Session{
		MaxTokens:      h.maxTokens,
		OnTool:         onTool,
		Log:            log,
		Sys:            sys,
		Router:         h.router,
		CompactProfile: "compact",
		TriggerPct:     h.compactionTriggerPct,
		Keys:           h.keys,
		SrcChannel:     srcChannel,
		UpdateID:       updateID,
		AuditApproval: func(approval string) (json.RawMessage, bool) {
			return auditApprovalFor(h.ap, sessionID, approval, turnFallback)
		},
	}

	// engineCtx is intentionally not r.Context(): the turn persists after
	// client disconnect (spec §5 P3), bounded by turn_timeout only.
	_, err = sess.RunTurnTools(engineCtx, text, onDelta, h.tools, approver)
	if err != nil {
		stdlog.Printf("hearthd serve: turn on %s failed: %v", sessionID, err) //nolint:gosec // sessionID is regex-validated (spec §4), err is ours
		ts.fail()
		return err
	}

	// seq + token usage come from the assistant msg event loop just
	// appended (loop exposes usage there; we never touch loop).
	seq, inTok, outTok := lastAssistantUsage(log)

	ts.terminal("turn_done", payloadJSON(turnDonePayload{
		Seq:       seq,
		InTokens:  inTok,
		OutTokens: outTok,
	}))
	return nil
}

// lastAssistantUsage reads seq and token usage off the newest assistant
// msg event in the log (the one RunTurnTools just appended).
func lastAssistantUsage(log *penatus.Log) (seq int64, inTok, outTok int) {
	for _, ev := range log.Live() {
		if ev.T != "msg" {
			continue
		}
		var f struct {
			Role  string `json:"role"`
			Usage struct {
				In  int `json:"in"`
				Out int `json:"out"`
			} `json:"usage"`
		}
		b, merr := ev.MarshalJSON()
		if merr != nil {
			continue
		}
		if err := json.Unmarshal(b, &f); err != nil {
			continue
		}
		if f.Role == "assistant" {
			seq, inTok, outTok = ev.Seq, f.Usage.In, f.Usage.Out
		}
	}
	return seq, inTok, outTok
}

// auditApprovalFor maps the engine approval string ("auto" |
// "approved:<id>" | "denied:<id>") to the frozen SURFACE-SPEC §6 audit
// object. decision/reason/source come from the hub's terminal record
// (A2 attribution), never from the caller: explicit decisions carry
// reason "ok" with the deciding surface as source; timer/shutdown/hub
// causes carry their own. turnFallback names the turn's channel
// ("web" here, "telegram" for nuntius, "repl" for the REPL) and covers
// trusted tools (reason "not_required").
func auditApprovalFor(ap *ApprovalHub, sessionID, approval, turnFallback string) (json.RawMessage, bool) {
	decision, reason, source := "allowed", "not_required", turnFallback
	switch {
	case approval == "auto" || approval == "":
	case strings.HasPrefix(approval, "approved:"):
		reason, source = auditHubPair(ap, sessionID, strings.TrimPrefix(approval, "approved:"), turnFallback)
		if reason == "" { // approved outside a hub (REPL nick): explicit allow
			reason = "ok"
		}
	case strings.HasPrefix(approval, "denied:"):
		decision = "denied"
		reason, source = auditHubPair(ap, sessionID, strings.TrimPrefix(approval, "denied:"), turnFallback)
		if reason == "" { // no hub record: "no-approver" and friends
			reason = "disconnected"
		}
	}
	obj, err := json.Marshal(struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
		Source   string `json:"source"`
	}{decision, reason, source})
	if err != nil {
		return nil, false
	}
	return obj, true
}

// auditHubPair reads the hub's terminal attribution for an approval id
// and maps it onto the frozen reason enum (V7: explicit decisions —
// allow or deny — carry reason "ok"; the timer carries "timeout").
func auditHubPair(ap *ApprovalHub, sessionID, id, turnFallback string) (string, string) {
	hubReason := ap.Reason(sessionID, id)
	if hubReason == "" {
		return "", turnFallback
	}
	reason := hubReason
	switch hubReason {
	case "ok", "denied", "cancelled":
		reason = "ok"
	case "timed_out":
		reason = "timeout"
	}
	source := ap.Source(sessionID, id)
	if source == "" {
		source = turnFallback
	}
	return reason, source
}

// HandleApproval serves POST /v1/sessions/{id}/approvals/{aid}: resolves
// a pending approval (allow|deny). 200 resolved, 404 unknown, 410 already
// resolved (spec §5 approval state machine).
func (h *Hub) HandleApproval(w http.ResponseWriter, r *http.Request, sessionID, approvalID string) {
	var req struct {
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONBody(w, http.StatusBadRequest, `{"error":"invalid request body"}`)
		return
	}

	switch h.ap.Resolve(sessionID, approvalID, req.Decision == "allow") {
	case 200:
		writeJSONBody(w, http.StatusOK, `{"ok":true}`)
	case 404:
		writeJSONBody(w, http.StatusNotFound, `{"error":"not found"}`)
	case 410:
		writeJSONBody(w, http.StatusGone, `{"error":"already resolved"}`)
	default:
		writeJSONBody(w, http.StatusInternalServerError, `{"error":"internal error"}`)
	}
}
