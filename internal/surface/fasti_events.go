package surface

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/lararium-app/lararium/internal/fasti"
)

// fastiPingInterval is the SSE keepalive interval (spec D8 / connectors_events clone).
// Made variable so test suites can advance without sleeping 5 seconds.
var fastiPingInterval = 5 * time.Second

// fastiEventsHandler serves the SSE stream for GET /v1/fasti/events (spec D8).
//
// Cloned from connectors_events.go:
// - 5s ping keepalive (": ping\n\n")
// - 64-slot buffer whose overflow disconnects the client (no resume, no Last-Event-ID)
// - Replays full bootstrap snapshot (jobs, goals, heartbeat) on initial connect
//
// Publish boundary note (spec D8 / F4a brief):
// To produce events without modifying engine.go, the Hub publish-on-write pattern operates
// through a goroutine-free rule: surface handlers publish after successful engine mutations
// (AddJob, Patch, Remove, Run, and goal verbs), and SSE streams replay a full bootstrap snapshot
// on connect. Heartbeat rows publish on heartbeat-status changes observed via state bootstrap only
// (acceptable for v1; F5 wires loop-side publishes).
func (s *Server) fastiEventsHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONBody(w, http.StatusInternalServerError, `{"error":"streaming unsupported"}`)
		return
	}

	// Subscribe BEFORE the headers go out (issue #233): the client returns
	// from Do() the instant the response header is flushed, so anything it
	// mutates next would publish into a stream nobody has subscribed to yet.
	eventCh, unsubscribe := s.Fasti.Subscribe()
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	// Replay the bootstrap snapshot; events that fired between subscribe and
	// replay are buffered in eventCh and replayed as idempotent row updates.
	s.replayFastiBootstrap(w, flusher, r)

	ticker := time.NewTicker(fastiPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case ev, ok := <-eventCh:
			if !ok {
				// 64-slot buffer overflow triggered disconnect per spec D8
				return
			}
			data, err := json.Marshal(ev)
			if err == nil {
				_, _ = fmt.Fprintf(w, "event: fasti_event\ndata: %s\n\n", data)
				flusher.Flush()
			}
		}
	}
}

// replayFastiBootstrap sends the current jobs, goals, and heartbeat snapshot on stream connect.
func (s *Server) replayFastiBootstrap(w http.ResponseWriter, flusher http.Flusher, r *http.Request) {
	ctx := r.Context()

	// Jobs bootstrap
	if jobs, err := s.Fasti.ListJobs(ctx); err == nil {
		for _, j := range jobs {
			emitFastiEvent(w, flusher, fasti.Event{
				Kind: "job",
				Op:   "upsert",
				Row:  j,
			})
		}
	}

	// Goals bootstrap
	if goals, err := s.Fasti.ListGoals(ctx); err == nil {
		for _, g := range goals {
			emitFastiEvent(w, flusher, fasti.Event{
				Kind: "goal",
				Op:   "upsert",
				Row:  g,
			})
		}
	}

	// Heartbeat bootstrap
	hb := s.Fasti.Heartbeat()
	emitFastiEvent(w, flusher, fasti.Event{
		Kind: "heartbeat",
		Op:   "upsert",
		Row: FastiHeartbeatState{
			Enabled: hb.Enabled,
			Cadence: hb.Cadence,
			Quiet:   hb.Quiet,
		},
	})
}

func emitFastiEvent(w http.ResponseWriter, flusher http.Flusher, ev fasti.Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: fasti_event\ndata: %s\n\n", data)
	flusher.Flush()
}
