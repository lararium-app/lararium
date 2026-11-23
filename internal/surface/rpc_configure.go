package surface

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
)

// rpcSessionsConfigure implements sessions.configure (RPC-SPEC §4a).
// Params: {id, model_pin?, private?, expected_revision?}. private (a bool)
// scopes the session to the caller (true) or shares it with every device
// again (false); only the session's creator may change it (SURFACE-SPEC §4b).
//
// Presence of model_pin is tracked explicitly via a raw key map so:
//   - field absent: model_pin is not modified (modelPin == nil passed to Configure)
//   - field present as null or "": model_pin is cleared
//   - field present as non-empty string: validated against s.Models before applying
func (s *Server) rpcSessionsConfigure(_ context.Context, conn *RPCConn, req *RPCRequest) {
	trimmed := bytes.TrimSpace(req.Params)
	var raw map[string]json.RawMessage
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || json.Unmarshal(trimmed, &raw) != nil || raw == nil {
		_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
		return
	}

	for k := range raw {
		switch k {
		case "id", "model_pin", "private", "expected_revision":
		default:
			_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
			return
		}
	}

	var id string
	if rawID, ok := raw["id"]; ok && !bytes.Equal(bytes.TrimSpace(rawID), []byte("null")) {
		if err := json.Unmarshal(rawID, &id); err != nil {
			_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
			return
		}
	}
	if !ValidSessionID(id) || !s.sessionVisibleTo(conn.caller, id) {
		_ = conn.SendError(*req.ID, "no such session", "no_such_session", nil)
		return
	}

	var privateArg *bool
	rawPrivate, hasPrivate := raw["private"]
	if hasPrivate {
		var v bool
		if err := json.Unmarshal(bytes.TrimSpace(rawPrivate), &v); err != nil || bytes.Equal(bytes.TrimSpace(rawPrivate), []byte("null")) {
			_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
			return
		}
		privateArg = &v
	}

	var pinArg *string
	rawPin, hasPin := raw["model_pin"]
	if hasPin {
		pinTrimmed := bytes.TrimSpace(rawPin)
		if bytes.Equal(pinTrimmed, []byte("null")) {
			empty := ""
			pinArg = &empty
		} else {
			var pinStr string
			if err := json.Unmarshal(pinTrimmed, &pinStr); err != nil {
				_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
				return
			}
			if pinStr != "" {
				if ok, reason := s.validateModelPin(pinStr); !ok {
					_ = conn.SendError(*req.ID, reason, reason, nil)
					return
				}
			}
			pinArg = &pinStr
		}
	}

	var expectedRev *int64
	if rawRev, ok := raw["expected_revision"]; ok && !bytes.Equal(bytes.TrimSpace(rawRev), []byte("null")) {
		var rev int64
		if err := json.Unmarshal(rawRev, &rev); err != nil || rev < 0 {
			_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
			return
		}
		expectedRev = &rev
	}

	if s.Sessions == nil {
		_ = conn.SendError(*req.ID, "internal error", "internal_error", nil)
		return
	}

	var sess SessionInfo
	var err error
	if src, ok := s.Sessions.(SessionAudienceSource); ok {
		sess, err = src.ConfigureWith(id, SessionConfigureRequest{
			ModelPin:         pinArg,
			Private:          privateArg,
			ExpectedRevision: expectedRev,
			Caller:           conn.caller,
		})
		if err == nil && privateArg != nil && *privateArg {
			// The new audience is on disk; now stop live delivery to
			// connections outside it.
			s.dropRPCSubsOutside(id, sess.audience)
		}
	} else if hasPrivate {
		err = errAudienceUnsupported
	} else {
		sess, err = s.Sessions.Configure(id, pinArg, expectedRev)
	}
	switch {
	case errors.Is(err, ErrNoSession):
		_ = conn.SendError(*req.ID, "no such session", "no_such_session", nil)
	case errors.Is(err, ErrNotCreator):
		_ = conn.SendError(*req.ID, "only the session's creator can change its audience", "not_creator", nil)
	case errors.Is(err, ErrSharedSession):
		_ = conn.SendError(*req.ID, "the main session is shared with every device", "session_shared", nil)
	case errors.Is(err, errAudienceUnsupported):
		_ = conn.SendError(*req.ID, "not implemented", "not_implemented", nil)
	case errors.Is(err, ErrRevisionConflict):
		_ = conn.SendError(*req.ID, "revision conflict", "revision_conflict", nil)
	case err != nil:
		log.Printf("surface: rpc sessions.configure failed: %v", err)
		_ = conn.SendError(*req.ID, "internal error", "internal_error", nil)
	default:
		applied := map[string]string{}
		if hasPin {
			applied["model_pin"] = "ok"
		}
		if hasPrivate {
			applied["private"] = "ok"
		}
		_ = conn.SendResult(*req.ID, map[string]any{
			"ok":      true,
			"applied": applied,
			"session": sess,
		})
	}
}
