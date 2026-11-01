package surface

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type deviceRailEvent struct {
	row  *DeviceCard
	gone string
}

func (s *Server) registerDevicePoke(f func()) func() {
	s.devicePokeMu.Lock()
	defer s.devicePokeMu.Unlock()
	if s.devicePokes == nil {
		s.devicePokes = make(map[int64]func())
	}
	s.devicePokeSeq++
	id := s.devicePokeSeq
	s.devicePokes[id] = f
	return func() {
		s.devicePokeMu.Lock()
		defer s.devicePokeMu.Unlock()
		delete(s.devicePokes, id)
	}
}

func (s *Server) pokeDeviceRail() {
	s.devicePokeMu.RLock()
	var callbacks []func()
	for _, f := range s.devicePokes {
		callbacks = append(callbacks, f)
	}
	s.devicePokeMu.RUnlock()
	for _, f := range callbacks {
		f()
	}
}

// devicesEventsHandler serves the SSE stream for GET /v1/devices/events and per-device GET /v1/devices/<id>/events.
func (s *Server) devicesEventsHandler(w http.ResponseWriter, r *http.Request, filterDeviceID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONBody(w, http.StatusInternalServerError, `{"error":"streaming unsupported"}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	events := make(chan deviceRailEvent, 64)
	overflowCh := make(chan struct{})
	var overflowOnce sync.Once
	push := func(ev deviceRailEvent) {
		select {
		case events <- ev:
		default:
			overflowOnce.Do(func() { close(overflowCh) })
		}
	}

	var mu sync.Mutex
	last := map[string][]byte{}
	for _, row := range s.deviceRows() {
		if filterDeviceID != "" && row.DeviceID != filterDeviceID {
			continue
		}
		if b, err := json.Marshal(row); err == nil {
			last[row.DeviceID] = b
		}
	}

	diff := func() {
		mu.Lock()
		defer mu.Unlock()
		rows := s.deviceRows()
		present := make(map[string]bool, len(rows))
		for i := range rows {
			row := rows[i]
			if filterDeviceID != "" && row.DeviceID != filterDeviceID {
				continue
			}
			present[row.DeviceID] = true
			b, err := json.Marshal(row)
			if err != nil {
				continue
			}
			if prev, had := last[row.DeviceID]; had && string(prev) == string(b) {
				continue
			}
			last[row.DeviceID] = b
			push(deviceRailEvent{row: &row})
		}
		for id := range last {
			if !present[id] {
				delete(last, id)
				push(deviceRailEvent{gone: id})
			}
		}
	}

	cancelLocal := s.registerDevicePoke(diff)
	defer cancelLocal()

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
					_, _ = fmt.Fprintf(w, "event: device_state\ndata: %s\n\n", data)
					flusher.Flush()
				}
			} else {
				data, err := json.Marshal(map[string]string{"device_id": ev.gone})
				if err == nil {
					_, _ = fmt.Fprintf(w, "event: device_gone\ndata: %s\n\n", data)
					flusher.Flush()
				}
			}
		}
	}
}
