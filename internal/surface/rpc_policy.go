package surface

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
)

// rpcPolicyRulesList implements policy.rules.list (port of policyGetHandler, policy.go).
func (s *Server) rpcPolicyRulesList(_ context.Context, conn *RPCConn, req *RPCRequest) {
	src := s.getPolicySource()
	if src == nil {
		_ = conn.SendError(*req.ID, "locked", "locked", nil)
		return
	}

	rules, err := src.ListRules()
	if err != nil {
		switch {
		case errors.Is(err, ErrPolicyLocked) || err.Error() == "locked":
			_ = conn.SendError(*req.ID, "locked", "locked", nil)
		case errors.Is(err, ErrPolicyCorrupt) || err.Error() == "policy_corrupt":
			_ = conn.SendError(*req.ID, "policy corrupt", "policy_corrupt", nil)
		default:
			log.Printf("surface: rpc policy.rules.list failed: %v", err)
			_ = conn.SendError(*req.ID, "internal error", "internal_error", nil)
		}
		return
	}

	if rules == nil {
		rules = []PolicyRule{}
	}

	_ = conn.SendResult(*req.ID, map[string]any{
		"egress_strict": src.EgressStrict(),
		"rules":         rules,
	})
}

// rpcPolicyRulesSet implements policy.rules.set (port of policyPutHandler, policy.go).
// Always calls PolicySource so the adapter's always=false and actor="web" invariants hold.
func (s *Server) rpcPolicyRulesSet(_ context.Context, conn *RPCConn, req *RPCRequest) {
	src := s.getPolicySource()
	if src == nil {
		_ = conn.SendError(*req.ID, "locked", "locked", nil)
		return
	}

	trimmed := bytes.TrimSpace(req.Params)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
		return
	}

	var params struct {
		Lane    string `json:"lane"`
		Pattern string `json:"pattern"`
		Verdict string `json:"verdict"`
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&params); err != nil {
		_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
		return
	}

	// Preserve v1's lane coercion: anything other than "egress" becomes "credential".
	lane := params.Lane
	if lane != "egress" {
		lane = "credential"
	}

	notes, err := src.AddRule(lane, params.Pattern, params.Verdict)
	if err != nil {
		switch {
		case errors.Is(err, ErrPolicyLocked) || err.Error() == "locked":
			_ = conn.SendError(*req.ID, "locked", "locked", nil)
		case errors.Is(err, ErrPolicyCorrupt) || err.Error() == "policy_corrupt":
			_ = conn.SendError(*req.ID, "policy corrupt", "policy_corrupt", nil)
		default:
			_ = conn.SendError(*req.ID, err.Error(), "invalid_rule", nil)
		}
		return
	}

	if notes == nil {
		notes = []string{}
	}

	_ = conn.SendResult(*req.ID, map[string]any{
		"ok":    true,
		"notes": notes,
	})
}

// rpcPolicyRulesRemove implements policy.rules.remove (port of policyDeleteHandler, policy.go).
// Preserves v1's gate order: pattern regex, then lane validation, then PolicySource.
func (s *Server) rpcPolicyRulesRemove(_ context.Context, conn *RPCConn, req *RPCRequest) {
	trimmed := bytes.TrimSpace(req.Params)
	var params struct {
		Pattern string `json:"pattern"`
		Lane    string `json:"lane,omitempty"`
	}
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || json.Unmarshal(trimmed, &params) != nil {
		_ = conn.SendError(*req.ID, "invalid request body", "invalid_request", nil)
		return
	}

	if !policyPatternGateRe.MatchString(params.Pattern) {
		_ = conn.SendError(*req.ID, "not found", "not_found", nil)
		return
	}

	if params.Lane != "" && params.Lane != "credential" && params.Lane != "egress" {
		_ = conn.SendError(*req.ID, "invalid lane", "invalid_lane", nil)
		return
	}

	src := s.getPolicySource()
	if src == nil {
		_ = conn.SendError(*req.ID, "locked", "locked", nil)
		return
	}

	removed, err := src.RemoveRule(params.Lane, params.Pattern)
	if err != nil {
		switch {
		case errors.Is(err, ErrPolicyLocked) || err.Error() == "locked":
			_ = conn.SendError(*req.ID, "locked", "locked", nil)
		case errors.Is(err, ErrPolicyCorrupt) || err.Error() == "policy_corrupt":
			_ = conn.SendError(*req.ID, "policy corrupt", "policy_corrupt", nil)
		default:
			_ = conn.SendError(*req.ID, err.Error(), "invalid_rule", nil)
		}
		return
	}

	_ = conn.SendResult(*req.ID, map[string]any{
		"ok":      true,
		"removed": removed,
	})
}
