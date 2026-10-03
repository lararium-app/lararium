package surface

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
	"github.com/lararium-app/lararium/internal/penatus"
	"github.com/lararium-app/lararium/internal/router"
)

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
	mu                   sync.Mutex
	running              map[string]*turn
}

// NewHub builds the turn hub. hearthHome is the persona root; session
// logs live under <hearthHome>/sessions/<id> exactly where repl.go puts
// them, so REPL and API see the same logs.
func NewHub(cfg ServeConfig, hearthHome string, maxTokens int, compactionTriggerPct int, tools []loop.Tool, rt *router.Router, ap *ApprovalHub) *Hub {
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

// Cancel aborts the running turn for the session, if any.
func (h *Hub) Cancel(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.running[sessionID]
	if !ok {
		return false
	}
	t.cancel()
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

// sessionDir is where repl.go keeps every session log: <home>/sessions/<id>
// (main included — it is NOT the hearth root).
func (h *Hub) sessionDir(sessionID string) string {
	return filepath.Join(h.hearthHome, "sessions", sessionID)
}

// turnStream is the SSE writer for one turn: serialized frames, a
// heartbeat, and an exactly-once terminal event (spec §5: every stream
// ends in exactly one turn_done or error).
type turnStream struct {
	w       http.ResponseWriter
	flusher http.Flusher

	writeMu      sync.Mutex
	started      bool
	terminalOnce sync.Once

	hbStop     chan struct{}
	hbDone     chan struct{}
	stopHBOnce sync.Once

	denyAll       func()
	disconnectOnc sync.Once
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
	writeJSONBody(w, http.StatusInternalServerError, `{"error":"turn failed"}`)
}

// disconnected fires the disconnect policy once: pending approvals die
// with the listener (spec §5).
func (ts *turnStream) disconnected() {
	ts.disconnectOnc.Do(ts.denyAll)
}

// end stops the heartbeat and waits for the writer goroutine to drain.
func (ts *turnStream) end() {
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
		h.ap.DenyAllFor(sessionID, "disconnected")
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

// runTurn executes one engine loop and streams its events; when it
// returns, exactly one terminal event has left the stream.
func (h *Hub) runTurn(engineCtx, reqCtx context.Context, w http.ResponseWriter, ts *turnStream, t *turn, sessionID, text string) {
	approver := func(name, argsSummary string) (bool, string) {
		// Spec §5: an approval can only be pending while a client is
		// listening. No listener at creation time → denied immediately.
		if reqCtx.Err() != nil {
			return false, "disconnected"
		}
		id, decisionCh := h.ap.Register(sessionID, name, argsSummary, func(approvalID string) {
			ts.writeEvent("approval_request", payloadJSON(approvalPayload{
				ApprovalID:  approvalID,
				Name:        name,
				ArgsSummary: argsSummary,
			}))
		})
		select {
		case decision := <-decisionCh:
			return decision, id
		case <-reqCtx.Done():
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

	sessDir := h.sessionDir(sessionID)
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		ts.failPreStream(w)
		return
	}

	log, err := penatus.OpenLog(sessDir)
	if err != nil {
		ts.failPreStream(w)
		return
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
	}

	// engineCtx is intentionally not r.Context(): the turn persists after
	// client disconnect (spec §5 P3), bounded by turn_timeout only.
	_, err = sess.RunTurnTools(engineCtx, text, onDelta, h.tools, approver)
	if err != nil {
		ts.fail()
		return
	}

	// seq + token usage come from the assistant msg event loop just
	// appended (loop exposes usage there; we never touch loop).
	seq, inTok, outTok := lastAssistantUsage(log)

	ts.terminal("turn_done", payloadJSON(turnDonePayload{
		Seq:       seq,
		InTokens:  inTok,
		OutTokens: outTok,
	}))
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
