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

type ApprovalHub struct {
	mu        sync.Mutex
	bySession map[string]map[string]*approval
	timeout   time.Duration
}

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

func (h *ApprovalHub) DenyAllFor(sessionID, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	session, ok := h.bySession[sessionID]
	if !ok {
		return
	}

	for id, ap := range session {
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
			_ = id // keep for potential future use
		}
	}
}

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
