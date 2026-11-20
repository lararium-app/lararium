package surface

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"unicode"
)

// registerDefaultRPCMethods wires the built-in JSON-RPC methods onto the Server
// using RegisterRPCMethod (if not already registered by a caller or test).
func (s *Server) registerDefaultRPCMethods() {
	defaults := map[string]RPCMethodHandler{
		"sessions.list":              s.rpcSessionsList,
		"sessions.create":            s.rpcSessionsCreate,
		"sessions.delete":            s.rpcSessionsDelete,
		"sessions.subscribe":         s.rpcSessionsSubscribe,
		"sessions.send":              s.rpcSessionsSend,
		"sessions.cancel":            s.rpcSessionsCancel,
		"sessions.approvals.resolve": s.rpcSessionsApprovalsResolve,
	}
	for name, h := range defaults {
		if _, ok := s.lookupRPCMethod(name); !ok {
			s.RegisterRPCMethod(name, h)
		}
	}
}

func (s *Server) subscribeRPCSession(sessionID string, conn *RPCConn) {
	s.rpcSubsMu.Lock()
	defer s.rpcSubsMu.Unlock()
	if s.rpcSubs == nil {
		s.rpcSubs = make(map[string]map[*RPCConn]struct{})
	}
	if s.rpcSubs[sessionID] == nil {
		s.rpcSubs[sessionID] = make(map[*RPCConn]struct{})
	}
	s.rpcSubs[sessionID][conn] = struct{}{}
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

func (s *Server) unsubscribeAllRPCConn(conn *RPCConn) {
	s.rpcSubsMu.Lock()
	defer s.rpcSubsMu.Unlock()
	for sid, conns := range s.rpcSubs {
		delete(conns, conn)
		if len(conns) == 0 {
			delete(s.rpcSubs, sid)
		}
	}
}

func (s *Server) notifyRPCSessionEvent(sessionID string, event json.RawMessage) {
	s.rpcSubsMu.RLock()
	connsMap := s.rpcSubs[sessionID]
	if len(connsMap) == 0 {
		s.rpcSubsMu.RUnlock()
		return
	}
	conns := make([]*RPCConn, 0, len(connsMap))
	for c := range connsMap {
		conns = append(conns, c)
	}
	s.rpcSubsMu.RUnlock()

	params := map[string]any{
		"session_id": sessionID,
		"event":      event,
	}
	for _, c := range conns {
		_ = c.Notify("session.event", params)
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
	if sessions == nil {
		sessions = []SessionInfo{}
	}
	_ = conn.SendResult(*req.ID, map[string]any{
		"sessions": sessions,
	})
}

// rpcSessionsCreate implements sessions.create (port of createSessionHandler, handlers.go:323).
func (s *Server) rpcSessionsCreate(_ context.Context, conn *RPCConn, req *RPCRequest) {
	var params struct {
		Title    string `json:"title,omitempty"`
		ModelPin string `json:"model_pin,omitempty"`
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
	session, err := s.Sessions.Create(params.Title, params.ModelPin)
	if err != nil {
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

	if !ValidSessionID(params.ID) {
		_ = conn.SendError(*req.ID, "no such session", "no_such_session", nil)
		return
	}
	if s.Hub != nil && s.Hub.InFlight(params.ID) {
		_ = conn.SendError(*req.ID, "session busy", "session_busy", nil)
		return
	}
	if s.Sessions == nil {
		_ = conn.SendError(*req.ID, "delete failed", "delete_failed", nil)
		return
	}
	err := s.Sessions.Delete(params.ID)
	switch {
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
	events, inFlight, err := s.Sessions.Events(id, afterSeq, limit)
	if err != nil {
		if errors.Is(err, ErrNoSession) {
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

	// Register subscription before sending catch-up response so no subsequent live events are missed.
	s.subscribeRPCSession(id, conn)

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
	if !ValidSessionID(params.ID) {
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
	err := hub.StartRPCTurn(connCtx, sessionID, params.Text, func(eventType, dataJSON string) {
		ev := buildRPCSessionEventJSON(eventType, dataJSON)
		s.notifyRPCSessionEvent(sessionID, ev)
	})
	if errors.Is(err, ErrTurnInFlight) {
		_ = conn.SendError(*req.ID, "turn in flight", "turn_in_flight", nil)
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
	}
	if len(req.Params) == 0 || json.Unmarshal(req.Params, &params) != nil {
		_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
		return
	}
	if !ValidSessionID(params.ID) {
		_ = conn.SendError(*req.ID, "not found", "not_found", nil)
		return
	}
	if s.Hub != nil && s.Hub.Cancel(params.ID) {
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
	if !ValidSessionID(params.SessionID) || !ValidApprovalID(params.ApprovalID) {
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
