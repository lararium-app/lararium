package surface

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"unicode"
)

// registerDefaultRPCMethods wires the built-in JSON-RPC methods onto the Server
// using RegisterRPCMethod (if not already registered by a caller or test).
func (s *Server) registerDefaultRPCMethods() {
	defaults := map[string]RPCMethodHandler{
		"sessions.list":              s.rpcSessionsList,
		"sessions.create":            s.rpcSessionsCreate,
		"sessions.configure":         s.rpcSessionsConfigure,
		"sessions.delete":            s.rpcSessionsDelete,
		"sessions.subscribe":         s.rpcSessionsSubscribe,
		"sessions.send":              s.rpcSessionsSend,
		"sessions.cancel":            s.rpcSessionsCancel,
		"sessions.approvals.resolve": s.rpcSessionsApprovalsResolve,
		"models.list":                s.rpcModelsList,
		"profile.describe":           s.rpcProfileDescribe,
		"profile.configure":          s.rpcProfileConfigure,
		"keys.list":                  s.rpcKeysList,
		"keys.set":                   s.rpcKeysSet,
		"keys.remove":                s.rpcKeysRemove,
		"policy.rules.list":          s.rpcPolicyRulesList,
		"policy.rules.set":           s.rpcPolicyRulesSet,
		"policy.rules.remove":        s.rpcPolicyRulesRemove,
		"custos.cards.subscribe":     s.rpcCustosCardsSubscribe,
		"custos.cards.resolve":       s.rpcCustosCardsResolve,
	}
	for name, h := range defaults {
		if _, ok := s.lookupRPCMethod(name); !ok {
			s.RegisterRPCMethod(name, h)
		}
	}
}

// rpcSessionSub is one connection's live subscription to one session.
//
// While any sessions.subscribe call on this (connection, session) pair sits
// between registering the subscription and writing its result frame
// (pending > 0), live events are appended to queued instead of written, so
// no session.event can overtake that result. The last call to write its
// result drains the queue in order while holding mu. Live writes also hold
// mu, so a live event can never overtake a queued one.
//
// Lock rule: mu is never held together with Server.rpcSubsMu, and nothing
// that calls into the session source or the turn hub runs under mu. The
// turn goroutine that emits an event therefore only ever waits on this
// connection's socket write (bounded by the write deadline), never on a
// catch-up read. The queue only grows while a window is open, which lasts one
// catch-up read plus one result write; the streamed text it can hold is also
// capped per turn by the turn's runaway-output limit.
//
// closed marks a subscription withdrawn because its session is no longer
// visible to this connection's caller (audience gate): nothing more is
// written, queued events are discarded instead of drained.
type rpcSessionSub struct {
	mu      sync.Mutex
	pending int
	queued  []json.RawMessage
	closed  bool
}

// close withdraws the subscription: later deliveries are dropped and any
// queued events are discarded. The caller must already have removed it from
// Server.rpcSubs, and must not hold Server.rpcSubsMu.
func (sub *rpcSessionSub) close() {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	sub.closed = true
	sub.queued = nil
}

// deliver writes one live event for sessionID to conn, or queues it while a
// subscribe call on this pair is still open.
func (sub *rpcSessionSub) deliver(conn *RPCConn, sessionID string, event json.RawMessage) {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.closed {
		return
	}
	if sub.pending > 0 {
		sub.queued = append(sub.queued, event)
		return
	}
	_ = conn.Notify("session.event", rpcSessionEventParams(sessionID, event))
}

func rpcSessionEventParams(sessionID string, event json.RawMessage) map[string]any {
	return map[string]any{
		"session_id": sessionID,
		"event":      event,
	}
}

// rpcSessionWindow opens one hold-back window on conn's EXISTING
// subscription for sessionID, without registering anything. RPC-SPEC §5
// ratified item 5: the response of the request that starts a turn
// (sessions.send) must never be overtaken by that turn's session.event
// frames on the sending connection. Returns nil when conn is not
// subscribed; every non-nil return must be paired with exactly one
// endSubscribe (called after the response/error frame is written).
func (s *Server) rpcSessionWindow(sessionID string, conn *RPCConn) *rpcSessionSub {
	s.rpcSubsMu.Lock()
	defer s.rpcSubsMu.Unlock()
	sub := s.rpcSubs[sessionID][conn]
	if sub == nil {
		return nil
	}
	sub.mu.Lock()
	sub.pending++
	sub.mu.Unlock()
	return sub
}

// beginRPCSessionSubscribe registers conn for live events on sessionID and
// opens one hold-back window on that subscription. Every call must be paired
// with exactly one endSubscribe on the returned subscription.
func (s *Server) beginRPCSessionSubscribe(sessionID string, conn *RPCConn) *rpcSessionSub {
	s.rpcSubsMu.Lock()
	if s.rpcSubs == nil {
		s.rpcSubs = make(map[string]map[*RPCConn]*rpcSessionSub)
	}
	conns := s.rpcSubs[sessionID]
	if conns == nil {
		conns = make(map[*RPCConn]*rpcSessionSub)
		s.rpcSubs[sessionID] = conns
	}
	sub := conns[conn]
	if sub == nil {
		// Created with its window already open while the map lock is held,
		// so no notifier can reach it with the window closed.
		sub = &rpcSessionSub{pending: 1}
		conns[conn] = sub
		s.rpcSubsMu.Unlock()
		return sub
	}
	s.rpcSubsMu.Unlock()

	// Existing subscription (a re-subscribe): open the window under the
	// subscription's own lock, after releasing the map lock. Removal from the
	// map only happens at connection teardown or when the session does not
	// exist, so a window opened on a just-removed entry is harmless.
	sub.mu.Lock()
	sub.pending++
	sub.mu.Unlock()
	return sub
}

// endSubscribe closes one hold-back window after that subscribe call has
// written its reply. When the last open window closes, queued events are
// written in emit order.
func (sub *rpcSessionSub) endSubscribe(conn *RPCConn, sessionID string) {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	sub.pending--
	if sub.pending > 0 {
		return
	}
	queued := sub.queued
	sub.queued = nil
	for _, ev := range queued {
		_ = conn.Notify("session.event", rpcSessionEventParams(sessionID, ev))
	}
}

func (s *Server) isRPCSessionSubscribed(sessionID string, conn *RPCConn) bool {
	s.rpcSubsMu.RLock()
	defer s.rpcSubsMu.RUnlock()
	if s.rpcSubs == nil {
		return false
	}
	_, ok := s.rpcSubs[sessionID][conn]
	return ok
}

// unsubscribeRPCSession removes one connection's subscription to one session only,
// leaving its subscriptions to other sessions intact (unlike unsubscribeAllRPCConn).
func (s *Server) unsubscribeRPCSession(sessionID string, conn *RPCConn) {
	s.rpcSubsMu.Lock()
	defer s.rpcSubsMu.Unlock()
	conns := s.rpcSubs[sessionID]
	if conns == nil {
		return
	}
	delete(conns, conn)
	if len(conns) == 0 {
		delete(s.rpcSubs, sessionID)
	}
}

// dropRPCSubsFor withdraws every sessions.subscribe registration on session
// id, whichever connection holds it. A deleted session keeps no live
// subscribers: without this a client's stale subscription would satisfy the
// sessions.send gate for an id that no longer exists. Each registration is
// closed after it leaves the map, never with rpcSubsMu held.
func (s *Server) dropRPCSubsFor(id string) {
	s.rpcSubsMu.Lock()
	conns := s.rpcSubs[id]
	delete(s.rpcSubs, id)
	s.rpcSubsMu.Unlock()
	for _, sub := range conns {
		sub.close()
	}
}

func (s *Server) unsubscribeAllRPCConn(conn *RPCConn) {
	s.rpcSubsMu.Lock()
	for sid, conns := range s.rpcSubs {
		delete(conns, conn)
		if len(conns) == 0 {
			delete(s.rpcSubs, sid)
		}
	}
	s.rpcSubsMu.Unlock()
	s.unsubscribeCustosRPCConn(conn)
}

func (s *Server) notifyRPCSessionEvent(sessionID string, event json.RawMessage) {
	type target struct {
		conn *RPCConn
		sub  *rpcSessionSub
	}
	s.rpcSubsMu.RLock()
	connsMap := s.rpcSubs[sessionID]
	if len(connsMap) == 0 {
		s.rpcSubsMu.RUnlock()
		return
	}
	targets := make([]target, 0, len(connsMap))
	for c, sub := range connsMap {
		targets = append(targets, target{conn: c, sub: sub})
	}
	s.rpcSubsMu.RUnlock()

	for _, tg := range targets {
		tg.sub.deliver(tg.conn, sessionID, event)
	}
}

// buildRPCSessionEventJSON merges the event discriminator ("type" and "t") into
// the JSON object serialized by turnStream so live session.event notifications
// carry the exact payload fields from turns.go alongside the event type discriminator.
func buildRPCSessionEventJSON(eventType, dataJSON string) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(dataJSON), &fields); err != nil || fields == nil {
		fields = make(map[string]json.RawMessage)
	}
	typeBytes := rawJSON(eventType)
	if _, hasType := fields["type"]; !hasType {
		fields["type"] = typeBytes
	}
	if _, hasT := fields["t"]; !hasT {
		fields["t"] = typeBytes
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return json.RawMessage(dataJSON)
	}
	return out
}

// rpcSessionsList implements sessions.list (port of listSessionsHandler, handlers.go:312).
func (s *Server) rpcSessionsList(_ context.Context, conn *RPCConn, req *RPCRequest) {
	if s.Sessions == nil {
		_ = conn.SendError(*req.ID, "internal error", "internal_error", nil)
		return
	}
	sessions, err := s.Sessions.List()
	if err != nil {
		log.Printf("surface: rpc sessions.list failed: %v", err)
		_ = conn.SendError(*req.ID, "internal error", "internal_error", nil)
		return
	}
	_ = conn.SendResult(*req.ID, map[string]any{
		"sessions": visibleSessions(sessions, conn.caller),
	})
}

// rpcSessionsCreate implements sessions.create (port of createSessionHandler, handlers.go:323).
func (s *Server) rpcSessionsCreate(_ context.Context, conn *RPCConn, req *RPCRequest) {
	var params struct {
		Title    string `json:"title,omitempty"`
		ModelPin string `json:"model_pin,omitempty"`
		Private  bool   `json:"private,omitempty"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
			return
		}
	}

	if params.ModelPin != "" {
		if ok, reason := s.validateModelPin(params.ModelPin); !ok {
			_ = conn.SendError(*req.ID, reason, reason, nil)
			return
		}
	}

	if s.Sessions == nil {
		_ = conn.SendError(*req.ID, "internal error", "internal_error", nil)
		return
	}
	session, err := s.createSession(conn.caller, params.Title, params.ModelPin, params.Private)
	switch {
	case errors.Is(err, errPrivateNeedsIdentity):
		_ = conn.SendError(*req.ID, "private session requires a caller identity", "private_requires_identity", nil)
		return
	case errors.Is(err, errAudienceUnsupported):
		_ = conn.SendError(*req.ID, "not implemented", "not_implemented", nil)
		return
	case err != nil:
		log.Printf("surface: rpc sessions.create failed: %v", err)
		_ = conn.SendError(*req.ID, "internal error", "internal_error", nil)
		return
	}

	_ = conn.SendResult(*req.ID, session)
}

// rpcSessionsDelete implements sessions.delete (port of deleteSessionHandler, handlers.go:358).
func (s *Server) rpcSessionsDelete(_ context.Context, conn *RPCConn, req *RPCRequest) {
	var params struct {
		ID string `json:"id"`
	}
	if len(req.Params) == 0 || json.Unmarshal(req.Params, &params) != nil {
		_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
		return
	}

	if !ValidSessionID(params.ID) || !s.sessionVisibleTo(conn.caller, params.ID) {
		_ = conn.SendError(*req.ID, "no such session", "no_such_session", nil)
		return
	}
	if s.Sessions == nil {
		_ = conn.SendError(*req.ID, "delete failed", "delete_failed", nil)
		return
	}
	err := s.deleteSessionExclusive(params.ID)
	switch {
	case errors.Is(err, errSessionBusy):
		_ = conn.SendError(*req.ID, "session busy", "session_busy", nil)
	case errors.Is(err, ErrNoSession):
		_ = conn.SendError(*req.ID, "no such session", "no_such_session", nil)
	case errors.Is(err, ErrProtectedSession):
		_ = conn.SendError(*req.ID, "session is protected", "session_is_protected", nil)
	case err != nil:
		log.Printf("surface: rpc sessions.delete failed: %v", err)
		_ = conn.SendError(*req.ID, "delete failed", "delete_failed", nil)
	default:
		_ = conn.SendResult(*req.ID, map[string]any{"ok": true})
	}
}

// rpcSessionsSubscribe implements sessions.subscribe (port of eventsHandler, handlers.go:381,
// plus live session.event notification subscription).
func (s *Server) rpcSessionsSubscribe(_ context.Context, conn *RPCConn, req *RPCRequest) {
	var raw map[string]json.RawMessage
	if len(req.Params) == 0 || json.Unmarshal(req.Params, &raw) != nil {
		_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
		return
	}

	var id string
	if rawID, ok := raw["id"]; ok {
		if err := json.Unmarshal(rawID, &id); err != nil {
			_ = conn.SendError(*req.ID, "not found", "not_found", nil)
			return
		}
	}
	if !ValidSessionID(id) {
		_ = conn.SendError(*req.ID, "not found", "not_found", nil)
		return
	}

	afterSeq := int64(0)
	if rawAfter, ok := raw["after_seq"]; ok && string(rawAfter) != "null" {
		var parsed int64
		if err := json.Unmarshal(rawAfter, &parsed); err != nil || parsed < 0 {
			_ = conn.SendError(*req.ID, "invalid after_seq", "invalid_after_seq", nil)
			return
		}
		afterSeq = parsed
	}

	limit := 200
	if rawLimit, ok := raw["limit"]; ok && string(rawLimit) != "null" {
		var parsed int
		if err := json.Unmarshal(rawLimit, &parsed); err != nil || parsed <= 0 {
			_ = conn.SendError(*req.ID, "invalid limit", "invalid_limit", nil)
			return
		}
		limit = parsed
	}
	if limit > 1000 {
		limit = 1000
	}

	if s.Sessions == nil {
		_ = conn.SendError(*req.ID, "internal error", "internal_error", nil)
		return
	}
	// Register the live subscription BEFORE the catch-up read: an event committed between an
	// Events() read and registering would otherwise be lost (past the catch-up cursor, not yet
	// forwarded live), which is realistic whenever another device already has a turn in flight on
	// this session. Registering opens a hold-back window: events emitted until this call has
	// written its reply are queued and written right after it, in emit order, so no
	// session.event overtakes the result (issue #310). Live events can overlap the catch-up
	// array (a turn already persisted there may still be streaming), but none is lost or arrives
	// first. ErrNoSession below unsubscribes again so a not-found reply leaves no orphan entry.
	sub := s.beginRPCSessionSubscribe(id, conn)
	defer sub.endSubscribe(conn, id)

	// Audience gate, checked AFTER registering: a concurrent scope-down
	// writes the header before dropping registrations outside the new
	// audience, so either that drop sees this registration or this read sees
	// the new audience. A hidden session answers like an unknown one.
	if !s.sessionVisibleTo(conn.caller, id) {
		s.unsubscribeRPCSession(id, conn)
		sub.close()
		_ = conn.SendError(*req.ID, "not found", "not_found", nil)
		return
	}

	events, inFlight, err := s.Sessions.Events(id, afterSeq, limit)
	if err != nil {
		if errors.Is(err, ErrNoSession) {
			s.unsubscribeRPCSession(id, conn)
			_ = conn.SendError(*req.ID, "not found", "not_found", nil)
			return
		}
		log.Printf("surface: rpc sessions.subscribe events failed: %v", err)
		_ = conn.SendError(*req.ID, "internal error", "internal_error", nil)
		return
	}
	if events == nil {
		events = []json.RawMessage{}
	}

	_ = conn.SendResult(*req.ID, map[string]any{
		"events":    events,
		"in_flight": inFlight,
	})
}

// rpcSessionsSend implements sessions.send (port of Hub.HandleMessage, turns.go:451,
// with RPC-SPEC §7.2 not_subscribed gate and Translation note #1 connection-lifetime disconnect semantics).
func (s *Server) rpcSessionsSend(connCtx context.Context, conn *RPCConn, req *RPCRequest) {
	var params struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if len(req.Params) == 0 || json.Unmarshal(req.Params, &params) != nil {
		_ = conn.SendError(*req.ID, "invalid request body", "invalid_request_body", nil)
		return
	}
	// One reply for a malformed, hidden or unknown id, ahead of every
	// parameter and subscription check, so those later replies cannot tell
	// a caller which ids exist (SURFACE-SPEC §4b). main is exempt from the
	// existence half: a fresh hearth has no main directory until its first
	// turn.
	if !ValidSessionID(params.ID) || !s.sessionVisibleTo(conn.caller, params.ID) ||
		(params.ID != "main" && !s.sessionExists(params.ID)) {
		_ = conn.SendError(*req.ID, "not found", "not_found", nil)
		return
	}
	if strings.TrimFunc(params.Text, unicode.IsSpace) == "" {
		_ = conn.SendError(*req.ID, "empty message text", "empty_message_text", nil)
		return
	}
	if !s.isRPCSessionSubscribed(params.ID, conn) {
		_ = conn.SendError(*req.ID, "not subscribed", "not_subscribed", nil)
		return
	}

	hub, ok := s.Hub.(*Hub)
	if !ok || hub == nil {
		if s.Hub != nil && s.Hub.InFlight(params.ID) {
			_ = conn.SendError(*req.ID, "turn in flight", "turn_in_flight", nil)
			return
		}
		_ = conn.SendError(*req.ID, "not implemented", "not_implemented", nil)
		return
	}

	sessionID := params.ID
	// Hold back this connection's session events until the response frame
	// below is written (RPC-SPEC §5 ratified item 5): a turn's turn_start
	// must never overtake the send response.
	if sub := s.rpcSessionWindow(sessionID, conn); sub != nil {
		defer sub.endSubscribe(conn, sessionID)
	}
	// A turn creates its session directory on demand, so a session deleted
	// since this connection subscribed must not be revived by a send. main
	// is exempt: the first turn on a fresh hearth creates it.
	var exists func() bool
	if sessionID != "main" {
		exists = func() bool { return s.sessionExists(sessionID) }
	}
	err := hub.StartRPCTurnGated(connCtx, sessionID, params.Text, func(eventType, dataJSON string) {
		ev := buildRPCSessionEventJSON(eventType, dataJSON)
		s.notifyRPCSessionEvent(sessionID, ev)
	}, exists)
	if errors.Is(err, ErrTurnInFlight) {
		_ = conn.SendError(*req.ID, "turn in flight", "turn_in_flight", nil)
		return
	}
	if errors.Is(err, ErrSessionGone) {
		_ = conn.SendError(*req.ID, "not found", "not_found", nil)
		return
	}
	if err != nil {
		_ = conn.SendError(*req.ID, "turn failed", "turn_failed", nil)
		return
	}

	_ = conn.SendResult(*req.ID, map[string]any{
		"accepted": true,
	})
}

// rpcSessionsCancel implements sessions.cancel (port of cancelHandler, handlers.go:431).
func (s *Server) rpcSessionsCancel(_ context.Context, conn *RPCConn, req *RPCRequest) {
	var params struct {
		ID string `json:"id"`
		// TurnID (#345): cancel only the turn bearing this id (learned
		// from the turn_start frame). Empty = legacy session-scoped cancel.
		TurnID string `json:"turn_id"`
	}
	if len(req.Params) == 0 || json.Unmarshal(req.Params, &params) != nil {
		_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
		return
	}
	if !ValidSessionID(params.ID) {
		_ = conn.SendError(*req.ID, "not found", "not_found", nil)
		return
	}
	if !s.sessionVisibleTo(conn.caller, params.ID) {
		_ = conn.SendError(*req.ID, "no turn in flight", "no_turn_in_flight", nil)
		return
	}
	if s.Hub != nil && s.Hub.CancelTurn(params.ID, params.TurnID) {
		_ = conn.SendResult(*req.ID, map[string]any{"ok": true})
		return
	}
	_ = conn.SendError(*req.ID, "no turn in flight", "no_turn_in_flight", nil)
}

// rpcSessionsApprovalsResolve implements sessions.approvals.resolve (port of Hub.HandleApproval, turns.go:837).
func (s *Server) rpcSessionsApprovalsResolve(_ context.Context, conn *RPCConn, req *RPCRequest) {
	var params struct {
		SessionID  string `json:"session_id"`
		ApprovalID string `json:"approval_id"`
		Decision   string `json:"decision"`
		Scope      string `json:"scope,omitempty"`
	}
	if len(req.Params) == 0 || json.Unmarshal(req.Params, &params) != nil {
		_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
		return
	}
	if !ValidSessionID(params.SessionID) || !ValidApprovalID(params.ApprovalID) ||
		!s.sessionVisibleTo(conn.caller, params.SessionID) {
		_ = conn.SendError(*req.ID, "not found", "not_found", nil)
		return
	}
	if params.Decision != "allow" && params.Decision != "deny" {
		_ = conn.SendError(*req.ID, "invalid decision", "invalid_request", nil)
		return
	}
	if params.Decision != "allow" && params.Scope != "" {
		_ = conn.SendError(*req.ID, "cannot combine scope with deny", "invalid_request", nil)
		return
	}
	if params.Scope != "" && params.Scope != "connector" {
		_ = conn.SendError(*req.ID, "invalid scope", "invalid_request", nil)
		return
	}

	var ap *ApprovalHub
	if hub, ok := s.Hub.(*Hub); ok && hub != nil {
		ap = hub.ap
	}
	if ap == nil {
		ap = s.approvalsViaRegistry()
	}
	if ap == nil {
		_ = conn.SendError(*req.ID, "not implemented", "not_implemented", nil)
		return
	}

	code, err := ap.ResolveScopeVia(params.SessionID, params.ApprovalID, params.Decision == "allow", "web", "", params.Scope)
	switch code {
	case 200:
		_ = conn.SendResult(*req.ID, map[string]any{"ok": true})
	case 400:
		msg := "invalid request"
		if err != nil {
			msg = err.Error()
		}
		_ = conn.SendError(*req.ID, msg, "invalid_request", nil)
	case 404:
		_ = conn.SendError(*req.ID, "not found", "not_found", nil)
	case 410:
		_ = conn.SendError(*req.ID, "already resolved", "already_resolved", nil)
	default:
		msg := "internal error"
		if err != nil {
			msg = err.Error()
		}
		_ = conn.SendError(*req.ID, msg, "internal_error", nil)
	}
}
