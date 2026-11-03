package fasti

import (
	"sync"
)

// Event represents a single SSE rail broadcast event for the fasti proactivity feature (spec D8).
type Event struct {
	Kind string `json:"kind"` // "job", "goal", "heartbeat"
	Op   string `json:"op"`   // "upsert", "delete"
	Row  any    `json:"row"`  // full row
}

type hubSubscriber struct {
	ch     chan Event
	closed bool
}

// Hub coordinates fan-out of fasti SSE events (spec D8).
// Slow clients (>64 pending events) are disconnected immediately without blocking the publisher.
type Hub struct {
	mu   sync.Mutex
	next uint64
	subs map[uint64]*hubSubscriber
}

// NewHub constructs a new fasti Event Hub.
func NewHub() *Hub {
	return &Hub{
		subs: make(map[uint64]*hubSubscriber),
	}
}

// Subscribe registers a new subscriber channel with a 64-slot buffer.
// It returns the receive-only channel and a cleanup unregister function.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.next++
	id := h.next
	sub := &hubSubscriber{
		ch: make(chan Event, 64),
	}
	h.subs[id] = sub

	unsubscribe := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subs, id)
	}

	return sub.ch, unsubscribe
}

// Publish broadcasts an event to all active subscribers.
// If a subscriber channel buffer is full (>64 pending items), the channel is closed to trigger
// immediate client disconnection per spec D8, and the subscriber is removed.
// The publisher never blocks on slow clients.
func (h *Hub) Publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for id, sub := range h.subs {
		if sub.closed {
			delete(h.subs, id)
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			// Buffer overflow (>64 pending): close channel to disconnect slow client per spec D8
			sub.closed = true
			close(sub.ch)
			delete(h.subs, id)
		}
	}
}

var (
	engineHubsMu sync.RWMutex
	engineHubs   = make(map[*Engine]*Hub)
)

// EventHub returns the fasti Hub attached to this Engine instance.
// Engine methods declared in this file follow Go's same-package rule without modifying engine.go (slice F4a law).
func (e *Engine) EventHub() *Hub {
	engineHubsMu.RLock()
	h, ok := engineHubs[e]
	engineHubsMu.RUnlock()
	if ok {
		return h
	}

	engineHubsMu.Lock()
	defer engineHubsMu.Unlock()
	if h, ok = engineHubs[e]; ok {
		return h
	}
	h = NewHub()
	engineHubs[e] = h
	return h
}

// Subscribe creates a new subscription on the engine's event hub.
func (e *Engine) Subscribe() (<-chan Event, func()) {
	return e.EventHub().Subscribe()
}

// Publish emits a fasti event onto the engine's event hub.
// Publish boundary note (spec D8 / F4a brief):
// To avoid touching engine.go, Hub publish-on-write operates through a goroutine-free rule:
// surface handlers publish after successful engine operations (AddJob, Patch, Remove, Run, and goal verbs),
// and SSE connections replay a full bootstrap snapshot on connect. Heartbeat rows publish on
// heartbeat status changes observed via state bootstrap only (acceptable for v1; F5 wires loop-side publishes).
func (e *Engine) Publish(ev Event) {
	e.EventHub().Publish(ev)
}

// Home returns the engine's hearth home path.
func (e *Engine) Home() string {
	return e.home
}

// Heartbeat returns the parsed heartbeat info from <home>/HEARTBEAT.md.
func (e *Engine) Heartbeat() HeartbeatInfo {
	return ReadHeartbeatInfo(e.home)
}
