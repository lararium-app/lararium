package surface

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Connector status rail (CONNECTOR-LIFE-SPEC D5): GET /v1/mcp/connectors/events.
//
// Built on the custody-card stream pattern (custosEventsHandler): the same
// ": ping" heartbeat (custosHeartbeatInterval), a 64-slot per-client buffer
// whose overflow disconnects the client, and NO Last-Event-ID tracking — a
// reconnecting client bootstraps from GET /v1/mcp/connectors.
//
// Two event kinds:
//
//	event: connector_state  data: <FULL ConnectorCard row>
//	event: connector_gone   data: {"name": "<name>"}
//
// connector_state has UPSERT-BY-NAME semantics: the payload is always the
// complete row (never a delta), so a client replaces whatever it holds for
// that name. connector_gone removes the name.
//
// Fan-out: the door client mirrors every MCP-STATUS / MCP-TOOLS broadcast;
// each broadcast pokes this stream, which re-reads the mirror and emits a
// full row for every connector whose row changed (status change, tools
// change) and a synthesized connector_gone for every name that vanished.
// Surface-local changes (a pending add appearing/clearing) poke the same way.
// A stream emits nothing at connect time: the snapshot GET is the bootstrap.

type connectorRailEvent struct {
	row  *ConnectorCard
	gone string
}

func (s *Server) connectorEventsHandler(w http.ResponseWriter, r *http.Request) {
	door := s.requireConnectorDoor(w)
	if door == nil {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONBody(w, http.StatusInternalServerError, `{"error":"streaming unsupported"}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	events := make(chan connectorRailEvent, 64)
	overflowCh := make(chan struct{})
	var overflowOnce sync.Once
	push := func(ev connectorRailEvent) {
		select {
		case events <- ev:
		default:
			// Slow client (cap 64): close the stream; it resyncs via GET.
			overflowOnce.Do(func() { close(overflowCh) })
		}
	}

	// last holds the JSON of the row each name was last emitted with. It is
	// primed with the current inventory so the stream starts silent.
	var mu sync.Mutex
	last := map[string][]byte{}
	for _, row := range s.connectorRows() {
		if b, err := rowJSON(row); err == nil {
			last[row.Name] = b
		}
	}
	// diff re-reads the mirror and pushes changes. The mutex orders
	// concurrent pokes: every call reads the freshest state, so the last
	// writer always leaves the client converged.
	diff := func() {
		mu.Lock()
		defer mu.Unlock()
		rows := s.connectorRows()
		present := make(map[string]bool, len(rows))
		for i := range rows {
			row := rows[i]
			present[row.Name] = true
			b, err := rowJSON(row)
			if err != nil {
				continue
			}
			if prev, had := last[row.Name]; had && string(prev) == string(b) {
				continue
			}
			last[row.Name] = b
			push(connectorRailEvent{row: &row})
		}
		for name := range last {
			if !present[name] {
				delete(last, name)
				push(connectorRailEvent{gone: name})
			}
		}
	}

	cancelLocal := s.registerConnectorPoke(diff)
	defer cancelLocal()
	cancelDoor := door.SubscribeConnectors(diff)
	defer cancelDoor()

	ticker := time.NewTicker(custosHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-overflowCh:
			return
		case <-ticker.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case ev := <-events:
			if ev.row != nil {
				data, err := json.Marshal(ev.row)
				if err == nil {
					_, _ = fmt.Fprintf(w, "event: connector_state\ndata: %s\n\n", data)
					flusher.Flush()
				}
			} else {
				data, err := json.Marshal(map[string]string{"name": ev.gone})
				if err == nil {
					_, _ = fmt.Fprintf(w, "event: connector_gone\ndata: %s\n\n", data)
					flusher.Flush()
				}
			}
		}
	}
}
