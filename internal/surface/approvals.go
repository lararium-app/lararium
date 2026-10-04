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

// ChannelSet is the live approver-channel set at approval creation
// (NUNTIUS-SPEC §7.3, A6): the timeout is chosen at creation from this
// set and never re-attributed. Web wins ties.
type ChannelSet uint8

// Approver channels tracked at approval creation.
const (
	ChannelWeb      ChannelSet = 1 << iota // an SSE listener is attached
	ChannelTelegram                        // the nuntius bridge is polling
)

// ApprovalFanout receives hub transition events so the same approval
// reaches every approver surface (§7.1). Callbacks run OUTSIDE the
// hub mutex and may block only as long as the subscriber dares —
// terminal events fire once per approval, in transition order.
type ApprovalFanout interface {
	ApprovalPending(id, sessionID, name, argsSummary string)
	ApprovalTerminal(id, sessionID, state, reason, source string)
}

type approval struct {
	id          string
	ch          chan bool
	state       string
	sessionID   string
	name        string
	argsSummary string
	timer       *time.Timer
	reason      string
	source      string
	channels    ChannelSet // live at creation (A6 timer selection)
	webLive     bool       // SSE listener still attached (A1 presence)
	cardDead    bool       // Telegram card undeliverable (A7)
}

// ApprovalHub tracks pending tool approvals across sessions: one
// pending approval per (session, id), resolved by the HTTP approver or
// the Telegram bridge, auto-denied on timeout, or denied when no live
// approver channel remains (spec §5 approval state machine plus
// NUNTIUS-SPEC §7 amendments A1/A5/A6/A7: first-wins, mutex-guarded).
type ApprovalHub struct {
	mu            sync.Mutex
	bySession     map[string]map[string]*approval
	timeout       time.Duration
	bridgeTimeout time.Duration
	fanout        ApprovalFanout
	bridgeLive    func() bool
}

// NewApprovalHub builds the hub; timeout is serve.approval_timeout —
// a pending approval auto-denies (reason timed_out) once it elapses.
// The bridge timeout defaults to the same value until
// SetBridgeTimeout overrides it (A6).
func NewApprovalHub(timeout time.Duration) *ApprovalHub {
	return &ApprovalHub{
		bySession:     make(map[string]map[string]*approval),
		timeout:       timeout,
		bridgeTimeout: timeout,
	}
}

// SetBridgeTimeout sets nuntius.approval_timeout, bound at creation
// for Telegram-only approvals (A6).
func (h *ApprovalHub) SetBridgeTimeout(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bridgeTimeout = d
}

// SetFanout installs the outbound subscriber (nil detaches).
func (h *ApprovalHub) SetFanout(f ApprovalFanout) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fanout = f
}

// SetBridgeLive installs the bridge presence probe (A1): a bridge is
// live while the daemon polls. nil means no bridge — web-only behavior
// is then exactly the pre-nuntius spec.
func (h *ApprovalHub) SetBridgeLive(f func() bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bridgeLive = f
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

// Register creates a pending approval on the web channel and fires
// onEvent exactly once (the SSE approval_request frame). The decision
// channel receives true (allow) or false (deny/deny-reason) exactly
// once.
func (h *ApprovalHub) Register(sessionID, name, argsSummary string, onEvent func(approvalID string)) (id string, decision <-chan bool) {
	return h.RegisterOn(sessionID, name, argsSummary, true, onEvent)
}

// RegisterOn creates a pending approval with webLive stating whether
// an SSE listener is attached; the Telegram bit is added from the
// bridge presence probe (§7.3 presence rule). Timeout: web live →
// serve.approval_timeout (web wins ties, A6); Telegram-only →
// nuntius.approval_timeout. Neither channel live violates the presence
// rule: the approval resolves immediately denied:disconnected
// (S6/V6b), decision channel already fed.
func (h *ApprovalHub) RegisterOn(sessionID, name, argsSummary string, webLive bool, onEvent func(approvalID string)) (id string, decision <-chan bool) {
	var channels ChannelSet
	if webLive {
		channels |= ChannelWeb
	}
	h.mu.Lock()
	if h.bridgeLive != nil && h.bridgeLive() {
		channels |= ChannelTelegram
	}
	id = generateApprovalID()
	ch := make(chan bool, 1)
	ap := &approval{
		id:          id,
		ch:          ch,
		state:       "pending",
		sessionID:   sessionID,
		name:        name,
		argsSummary: argsSummary,
		webLive:     channels&ChannelWeb != 0,
	}
	if h.bySession[sessionID] == nil {
		h.bySession[sessionID] = make(map[string]*approval)
	}
	h.bySession[sessionID][id] = ap

	if channels == 0 {
		ap.state, ap.reason, ap.source = "denied", "disconnected", "hub"
		ch <- false
		h.mu.Unlock()
		h.fanTerminal(id, sessionID, "denied", "disconnected", "hub")
		return id, ch
	}
	ap.channels = channels

	timeout := h.bridgeTimeout
	if channels&ChannelWeb != 0 {
		timeout = h.timeout
	}
	ap.timer = time.AfterFunc(timeout, func() { h.resolveTimeout(id) })
	h.mu.Unlock()

	onEvent(id)
	h.fanPending(id, sessionID, name, argsSummary)
	return id, ch
}

// fan* fire outside the mutex; the fanout field is snapshotted under it.
func (h *ApprovalHub) fanPending(id, sessionID, name, argsSummary string) {
	h.mu.Lock()
	f := h.fanout
	h.mu.Unlock()
	if f != nil {
		f.ApprovalPending(id, sessionID, name, argsSummary)
	}
}

func (h *ApprovalHub) fanTerminal(id, sessionID, state, reason, source string) {
	h.mu.Lock()
	f := h.fanout
	h.mu.Unlock()
	if f != nil {
		f.ApprovalTerminal(id, sessionID, state, reason, source)
	}
}

// settle applies a terminal transition under the caller's lock and
// reports whether it changed state (first-wins).
func (ap *approval) settle(state, reason, source string) bool {
	if ap.state != "pending" {
		return false
	}
	ap.state, ap.reason, ap.source = state, reason, source
	allow := state == "approved"
	select {
	case ap.ch <- allow:
	default:
	}
	if ap.timer != nil {
		ap.timer.Stop()
		ap.timer = nil
	}
	return true
}

func (h *ApprovalHub) resolveTimeout(id string) {
	h.mu.Lock()
	ap, ok := h.findLocked(id)
	if !ok || !ap.settle("timed_out", "timed_out", "timer") {
		h.mu.Unlock()
		return
	}
	sid := ap.sessionID
	h.mu.Unlock()
	h.fanTerminal(id, sid, "timed_out", "timed_out", "timer")
}

// findLocked locates an approval by its globally unique id. Callers
// hold h.mu.
func (h *ApprovalHub) findLocked(id string) (*approval, bool) {
	for _, session := range h.bySession {
		if ap, ok := session[id]; ok {
			return ap, true
		}
	}
	return nil, false
}

// Resolve settles a pending approval from the web channel. Returns the
// HTTP status the caller should answer with: 200 resolved, 404 unknown
// session or id, 410 already resolved (denied/approved/timed_out).
func (h *ApprovalHub) Resolve(sessionID, id string, allow bool) int {
	return h.ResolveFrom(sessionID, id, allow, "web")
}

// ResolveFrom settles a pending approval attributing the decision to
// source ("web" | "telegram"). Same status codes as Resolve; a losing
// click answers 410 with zero state change (§7.2 race: first wins).
func (h *ApprovalHub) ResolveFrom(sessionID, id string, allow bool, source string) int {
	h.mu.Lock()
	session, ok := h.bySession[sessionID]
	if !ok {
		h.mu.Unlock()
		return 404
	}
	ap, ok := session[id]
	if !ok {
		h.mu.Unlock()
		return 404
	}
	state, reason := "denied", "denied"
	if allow {
		state, reason = "approved", "ok"
	}
	if !ap.settle(state, reason, source) {
		h.mu.Unlock()
		return 410
	}
	h.mu.Unlock()
	h.fanTerminal(id, sessionID, state, reason, source)
	return 200
}

// DenyAllFor denies every pending approval of a session unconditionally
// (reason carries the cause, e.g. "cancelled"; source "hub").
func (h *ApprovalHub) DenyAllFor(sessionID, reason string) {
	h.denyAllFor(sessionID, reason, "hub")
}

// CancelAllFor denies every pending approval of a cancelled turn (A5):
// an approval whose turn no longer exists is meaningless; source is the
// cancelling surface.
func (h *ApprovalHub) CancelAllFor(sessionID, source string) {
	h.denyAllFor(sessionID, "cancelled", source)
}

func (h *ApprovalHub) denyAllFor(sessionID, reason, source string) {
	h.mu.Lock()
	var fired [][3]string
	for _, ap := range h.bySession[sessionID] {
		if ap.settle("denied", reason, source) {
			fired = append(fired, [3]string{ap.id, ap.state, reason})
		}
	}
	h.mu.Unlock()
	for _, f := range fired {
		h.fanTerminal(f[0], sessionID, f[1], f[2], source)
	}
}

// DisconnectFor applies the web-listener disconnect (A1 presence,
// §7.6(c)): the web channel dies for the session's approvals; each
// pending approval survives only if a live bridge still holds a live
// card for it (bridge live AND card not dead). Approvals with no live
// approver resolve denied:disconnected immediately — never a freeze.
func (h *ApprovalHub) DisconnectFor(sessionID string) {
	h.mu.Lock()
	bridgeUp := h.bridgeLive != nil && h.bridgeLive()
	var fired [][3]string
	for _, ap := range h.bySession[sessionID] {
		if ap.state != "pending" {
			continue
		}
		ap.webLive = false
		if bridgeUp && !ap.cardDead {
			continue // the Telegram card is the live approver for this one
		}
		if ap.settle("denied", "disconnected", "web") {
			fired = append(fired, [3]string{ap.id, "denied", "disconnected"})
		}
	}
	h.mu.Unlock()
	for _, f := range fired {
		h.fanTerminal(f[0], sessionID, f[1], f[2], "web")
	}
}

// MarkUndeliverable is the inbound undeliverable signal from the
// in-process bridge (A7): if Telegram is the sole live approver
// channel for this approval it resolves immediately
// denied:undeliverable (source "hub"); otherwise the per-approval
// card_dead flag is set — the bridge stops counting as an approver
// FOR THIS APPROVAL (A1 refinement) and the timer keeps running.
func (h *ApprovalHub) MarkUndeliverable(id string) {
	h.mu.Lock()
	ap, ok := h.findLocked(id)
	if !ok || ap.state != "pending" {
		h.mu.Unlock()
		return
	}
	if ap.channels&ChannelWeb != 0 && ap.webLive {
		ap.cardDead = true
		h.mu.Unlock()
		return
	}
	if !ap.settle("denied", "undeliverable", "hub") {
		h.mu.Unlock()
		return
	}
	sid := ap.sessionID
	h.mu.Unlock()
	h.fanTerminal(id, sid, "denied", "undeliverable", "hub")
}

// DenyAllAll denies every pending approval across all sessions
// (shutdown path: reason "shutdown", source "shutdown").
func (h *ApprovalHub) DenyAllAll(reason, source string) {
	h.mu.Lock()
	var fired [][4]string
	for sid, session := range h.bySession {
		for id, ap := range session {
			if ap.settle("denied", reason, source) {
				fired = append(fired, [4]string{id, sid, ap.state, reason})
			}
		}
	}
	h.mu.Unlock()
	for _, f := range fired {
		h.fanTerminal(f[0], f[1], f[2], f[3], source)
	}
}

// PendingCount is the number of pending approvals for a session.
func (h *ApprovalHub) PendingCount(sessionID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	count := 0
	for _, ap := range h.bySession[sessionID] {
		if ap.state == "pending" {
			count++
		}
	}
	return count
}

// Reason is why an approval resolved (ok|denied|timed_out|disconnected|
// cancelled|shutdown|undeliverable).
func (h *ApprovalHub) Reason(sessionID, id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()

	ap, ok := h.bySession[sessionID][id]
	if !ok {
		return ""
	}
	return ap.reason
}

// Source is which surface or hub mechanism caused the terminal
// transition (web|telegram|timer|shutdown|hub) — audit attribution A2.
// Empty while pending.
func (h *ApprovalHub) Source(sessionID, id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()

	ap, ok := h.bySession[sessionID][id]
	if !ok {
		return ""
	}
	return ap.source
}

// SessionOf maps an approval id to its session (§7.2: Telegram
// callbacks never name a session; the server resolves it).
func (h *ApprovalHub) SessionOf(id string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	ap, ok := h.findLocked(id)
	if !ok {
		return "", false
	}
	return ap.sessionID, true
}
