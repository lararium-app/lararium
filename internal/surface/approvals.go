// Package surface is the HTTP/SSE surface of hearthd (docs/SURFACE-SPEC.md):
// loopback API on serve.listen, bearer-token auth, session CRUD, streamed
// turns through the frozen loop engine, and the tool-approval round-trip.
// It never forks storage or loop semantics — Penatus and loop stay frozen.
package surface

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"sync"
	"time"
)

const (
	approvalIDPrefix       = "a_"
	approvalIDEntropyBytes = 12
)

type approval struct {
	ch          chan bool
	state       string
	sessionID   string
	name        string
	argsSummary string
	timer       *time.Timer
	reason      string
}

// ApprovalHub tracks pending tool approvals across sessions: one
// pending approval per (session, id), resolved by the HTTP approver,
// auto-denied on timeout, or denied when the listener disconnects
// (spec §5 approval state machine: pending -> approved|denied|timed_out).
type ApprovalHub struct {
	mu        sync.Mutex
	bySession map[string]map[string]*approval
	timeout   time.Duration
}

// NewApprovalHub builds the hub; timeout is serve.approval_timeout —
// a pending approval auto-denies (reason timed_out) once it elapses.
func NewApprovalHub(timeout time.Duration) *ApprovalHub {
	return &ApprovalHub{
		bySession: make(map[string]map[string]*approval),
		timeout:   timeout,
	}
}

func generateApprovalID() string {
	b := make([]byte, approvalIDEntropyBytes)
	_, _ = rand.Read(b)
	encoded := base64.RawStdEncoding.EncodeToString(b)
	encoded = strings.ReplaceAll(encoded, "+", "0")
	encoded = strings.ReplaceAll(encoded, "/", "1")
	encoded = strings.ReplaceAll(encoded, "=", "2")
	if len(encoded) > 16 {
		encoded = encoded[:16]
	}
	return approvalIDPrefix + encoded
}

// Register creates a pending approval and fires onEvent exactly once
// (the SSE approval_request frame). The decision channel receives true
// (allow) or false (deny/deny-reason) exactly once.
func (h *ApprovalHub) Register(sessionID, name, argsSummary string, onEvent func(approvalID string)) (id string, decision <-chan bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	id = generateApprovalID()
	ch := make(chan bool, 1)

	ap := &approval{
		ch:          ch,
		state:       "pending",
		sessionID:   sessionID,
		name:        name,
		argsSummary: argsSummary,
	}

	if h.bySession[sessionID] == nil {
		h.bySession[sessionID] = make(map[string]*approval)
	}
	h.bySession[sessionID][id] = ap

	ap.timer = time.AfterFunc(h.timeout, func() {
		h.resolveTimeout(id)
	})

	onEvent(id)

	return id, ch
}

func (h *ApprovalHub) resolveTimeout(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, session := range h.bySession {
		if ap, ok := session[id]; ok {
			if ap.state == "pending" {
				ap.state = "timed_out"
				ap.reason = "timed_out"
				select {
				case ap.ch <- false:
				default:
				}
				if ap.timer != nil {
					ap.timer.Stop()
				}
			}
			return
		}
	}
}

// Resolve settles a pending approval. Returns the HTTP status the
// caller should answer with: 200 resolved, 404 unknown session or id,
// 410 already resolved (denied/approved/timed_out).
func (h *ApprovalHub) Resolve(sessionID, id string, allow bool) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	session, ok := h.bySession[sessionID]
	if !ok {
		return 404
	}

	ap, ok := session[id]
	if !ok {
		return 404
	}

	if ap.state != "pending" {
		return 410
	}

	if allow {
		ap.state = "approved"
		ap.reason = "ok"
	} else {
		ap.state = "denied"
		ap.reason = "denied"
	}

	select {
	case ap.ch <- allow:
	default:
	}

	if ap.timer != nil {
		ap.timer.Stop()
		ap.timer = nil
	}

	return 200
}

// DenyAllFor denies every pending approval of a session (disconnect
// policy: reason "disconnected").
func (h *ApprovalHub) DenyAllFor(sessionID, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	session, ok := h.bySession[sessionID]
	if !ok {
		return
	}

	for _, ap := range session {
		if ap.state == "pending" {
			ap.state = "denied"
			ap.reason = reason
			select {
			case ap.ch <- false:
			default:
			}
			if ap.timer != nil {
				ap.timer.Stop()
				ap.timer = nil
			}
		}
	}
}

// PendingCount is the number of pending approvals for a session.
func (h *ApprovalHub) PendingCount(sessionID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	session, ok := h.bySession[sessionID]
	if !ok {
		return 0
	}

	count := 0
	for _, ap := range session {
		if ap.state == "pending" {
			count++
		}
	}
	return count
}

// Reason is why an approval resolved (ok|denied|timed_out|disconnected).
func (h *ApprovalHub) Reason(sessionID, id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()

	session, ok := h.bySession[sessionID]
	if !ok {
		return ""
	}

	ap, ok := session[id]
	if !ok {
		return ""
	}

	return ap.reason
}
