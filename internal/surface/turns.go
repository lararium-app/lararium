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

type turn struct {
	ctx    context.Context
	cancel context.CancelFunc
}

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

func (h *Hub) InFlight(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.running[sessionID]
	return ok
}

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

// sessionDir is where repl.go keeps every session log: <home>/sessions/<id>
// (main included — it is NOT the hearth root).
func (h *Hub) sessionDir(sessionID string) string {
	return filepath.Join(h.hearthHome, "sessions", sessionID)
}

func (h *Hub) HandleMessage(w http.ResponseWriter, r *http.Request, sessionID string) {
	writeJSONError := func(msg string, code int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}

	// Body first: a bad request must fail as plain JSON before any SSE
	// bytes exist.
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError("invalid request body", http.StatusBadRequest)
		return
	}

	// Atomic gate: check-and-install before writing any response.
	engineCtx, cancelEngine := context.WithTimeout(context.Background(), h.cfg.TurnTimeout)
	reqCtx, cancelReq := context.WithCancel(r.Context())
	t := &turn{ctx: reqCtx, cancel: func() { cancelReq(); cancelEngine() }}
	h.mu.Lock()
	if _, ok := h.running[sessionID]; ok {
		h.mu.Unlock()
		cancelEngine()
		cancelReq()
		writeJSONError("turn in flight", http.StatusConflict)
		return
	}
	h.running[sessionID] = t
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.running, sessionID)
		h.mu.Unlock()
	}()
	// Safety: never leak the engine context if we return before the
	// engine finishes its normal path.
	deferLease := sync.OnceFunc(func() { cancelEngine() })
	defer deferLease()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError("streaming unsupported", http.StatusInternalServerError)
		return
	}

	var writeMu sync.Mutex
	streamStarted := false
	writeEvent := func(event, data string) {
		writeMu.Lock()
		defer writeMu.Unlock()
		streamStarted = true
		_, _ = w.Write([]byte("event: " + event + "\n"))
		_, _ = w.Write([]byte("data: " + data + "\n\n"))
		flusher.Flush()
	}

	// Exactly one terminal event ever closes the stream (spec §5).
	terminalOnce := sync.Once{}
	terminal := func(event, data string) {
		terminalOnce.Do(func() { writeEvent(event, data) })
	}
	failTerminal := func() {
		terminal("error", `{"error":"turn failed"}`)
	}

	// Heartbeat: `: ping` every 5 s; stopHB guarantees the goroutine is
	// DONE before any terminal write, so the ResponseWriter is never
	// touched after this handler returns.
	hbStop := make(chan struct{})
	hbDone := make(chan struct{})
	stopHB := sync.OnceFunc(func() { close(hbStop) })
	go func() {
		defer close(hbDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				writeMu.Lock()
				_, _ = w.Write([]byte(": ping\n\n"))
				flusher.Flush()
				writeMu.Unlock()
			case <-hbStop:
				return
			}
		}
	}()
	endStream := func() {
		stopHB()
		<-hbDone
	}
	defer endStream()

	// Disconnect policy: pending approvals die with the listener.
	disconnectOnce := sync.OnceFunc(func() {
		h.ap.DenyAllFor(sessionID, "disconnected")
	})
	go func() {
		<-reqCtx.Done()
		disconnectOnce()
	}()

	approver := func(name, argsSummary string) (bool, string) {
		// Spec §5: an approval can only be pending while a client is
		// listening. No listener at creation time → denied immediately.
		if reqCtx.Err() != nil {
			return false, "disconnected"
		}
		id, decisionCh := h.ap.Register(sessionID, name, argsSummary, func(approvalID string) {
			data, _ := json.Marshal(map[string]string{
				"approval_id":  approvalID,
				"name":         name,
				"args_summary": argsSummary,
			})
			writeEvent("approval_request", string(data))
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
			failTerminal()
			t.cancel() // aborts engine AND writer; no assistant event
			return
		}
		data, _ := json.Marshal(map[string]string{"text": delta})
		writeEvent("delta", string(data))
	}

	// loop's OnTool carries no call id; the approval string is unique per
	// call ("approved:a_…"/"denied:a_…"), so it doubles as the correlation
	// key the client matches tool_start against tool_done with.
	onTool := func(phase, name, approval string, ok bool) {
		switch phase {
		case "start":
			data, _ := json.Marshal(map[string]string{
				"call_id": approval,
				"name":    name,
			})
			writeEvent("tool_start", string(data))
		case "done":
			data, _ := json.Marshal(map[string]any{
				"call_id":  approval,
				"name":     name,
				"ok":       ok,
				"approval": approval,
			})
			writeEvent("tool_done", string(data))
		}
	}

	sessDir := h.sessionDir(sessionID)
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		if streamStarted {
			failTerminal()
		} else {
			writeJSONError("turn failed", http.StatusInternalServerError)
		}
		return
	}

	log, err := penatus.OpenLog(sessDir)
	if err != nil {
		if streamStarted {
			failTerminal()
		} else {
			writeJSONError("turn failed", http.StatusInternalServerError)
		}
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

	_, err = sess.RunTurnTools(engineCtx, req.Text, onDelta, h.tools, approver)
	endStream()

	if err != nil {
		failTerminal()
		return
	}

	// seq + token usage come from the assistant msg event loop just
	// appended (loop exposes usage there; we never touch loop).
	events := log.Live()
	var seq int64
	var inTok, outTok int
	for _, ev := range events {
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
		if b, _ := ev.MarshalJSON(); b != nil {
			_ = json.Unmarshal(b, &f)
		}
		if f.Role == "assistant" {
			seq = ev.Seq
			inTok = f.Usage.In
			outTok = f.Usage.Out
		}
	}

	data, _ := json.Marshal(map[string]any{
		"seq":        seq,
		"in_tokens":  inTok,
		"out_tokens": outTok,
	})
	terminal("turn_done", string(data))
}

func (h *Hub) HandleApproval(w http.ResponseWriter, r *http.Request, sessionID, approvalID string) {
	writeError := func(w http.ResponseWriter, msg string, code int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}

	var req struct {
		Decision string `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	allow := req.Decision == "allow"
	code := h.ap.Resolve(sessionID, approvalID, allow)

	switch code {
	case 200:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	case 404:
		writeError(w, "not found", http.StatusNotFound)
	case 410:
		writeError(w, "already resolved", http.StatusGone)
	default:
		writeError(w, "internal error", http.StatusInternalServerError)
	}
}
