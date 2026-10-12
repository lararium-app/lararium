package surface

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// PolicyRule represents a single policy rule on the wire per SURFACE-SPEC §4.
type PolicyRule struct {
	Lane    string `json:"lane"`
	Pattern string `json:"pattern"`
	Verdict string `json:"verdict"`
	Always  bool   `json:"always"`
	Source  string `json:"source"`
}

// Sentinel error values for PolicySource operations matching SURFACE-SPEC §4.
var (
	// ErrPolicyLocked indicates policy operations were attempted on a locked vault.
	ErrPolicyLocked = errors.New("locked")
	// ErrPolicyCorrupt indicates policy operations encountered a corrupt policy state.
	ErrPolicyCorrupt = errors.New("policy_corrupt")
)

// PolicySource is the interface connecting the web surface to the policy engine
// without importing internal/custos per the frozen import direction.
type PolicySource interface {
	ListRules() ([]PolicyRule, error)
	EgressStrict() bool
	AddRule(lane, pattern, verdict string) ([]string, error)
	RemoveRule(lane, pattern string) (bool, error)
}

// SetPolicySource attaches a PolicySource to the server.
// Safe for concurrent use and nil-safe.
func (s *Server) SetPolicySource(src PolicySource) {
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	s.policySrc = src
}

func (s *Server) getPolicySource() PolicySource {
	s.policyMu.RLock()
	defer s.policyMu.RUnlock()
	return s.policySrc
}

// policyPatternGateRe enforces ^[\x21-\x7e]{1,256}$ (printable ASCII, no control chars or spaces, max 256).
var policyPatternGateRe = regexp.MustCompile(`^[\x21-\x7e]{1,256}$`)

// dispatchPolicy routes /v1/policy/rules and /v1/policy/rules/{pattern}.
// Returns true if the path is under /v1/policy/rules.
//
//nolint:unparam // always-true by design matching dispatchCustos
func (s *Server) dispatchPolicy(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/v1/policy/rules" {
		switch r.Method {
		case http.MethodGet:
			s.policyGetHandler(w, r)
		case http.MethodPut:
			s.policyPutHandler(w, r)
		default:
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return true
	}

	if strings.HasPrefix(r.URL.Path, "/v1/policy/rules/") {
		if r.Method != http.MethodDelete {
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		s.policyDeleteHandler(w, r)
		return true
	}

	s.error(w, "not found", http.StatusNotFound)
	return true
}

func (s *Server) policyGetHandler(w http.ResponseWriter, _ *http.Request) {
	src := s.getPolicySource()
	if src == nil {
		s.error(w, "locked", http.StatusLocked)
		return
	}

	rules, err := src.ListRules()
	if err != nil {
		switch {
		case errors.Is(err, ErrPolicyLocked) || err.Error() == "locked":
			s.error(w, "locked", http.StatusLocked)
		case errors.Is(err, ErrPolicyCorrupt) || err.Error() == "policy_corrupt":
			s.error(w, "policy_corrupt", http.StatusServiceUnavailable)
		default:
			s.error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}

	if rules == nil {
		rules = []PolicyRule{}
	}

	writeJSONAny(w, http.StatusOK, map[string]any{
		"egress_strict": src.EgressStrict(),
		"rules":         rules,
	})
}

func (s *Server) policyPutHandler(w http.ResponseWriter, r *http.Request) {
	src := s.getPolicySource()
	if src == nil {
		s.error(w, "locked", http.StatusLocked)
		return
	}

	var req struct {
		Lane    string `json:"lane"`
		Pattern string `json:"pattern"`
		Verdict string `json:"verdict"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	lane := req.Lane
	if lane != "egress" {
		lane = "credential"
	}

	notes, err := src.AddRule(lane, req.Pattern, req.Verdict)
	if err != nil {
		switch {
		case errors.Is(err, ErrPolicyLocked) || err.Error() == "locked":
			s.error(w, "locked", http.StatusLocked)
		case errors.Is(err, ErrPolicyCorrupt) || err.Error() == "policy_corrupt":
			s.error(w, "policy_corrupt", http.StatusServiceUnavailable)
		default:
			s.error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}

	if notes == nil {
		notes = []string{}
	}

	writeJSONAny(w, http.StatusOK, map[string]any{
		"ok":    true,
		"notes": notes,
	})
}

func (s *Server) policyDeleteHandler(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimPrefix(r.URL.Path, "/v1/policy/rules/")
	if r.URL.RawPath != "" {
		raw = strings.TrimPrefix(r.URL.RawPath, "/v1/policy/rules/")
	}
	pattern, err := url.PathUnescape(raw)
	if err != nil || !policyPatternGateRe.MatchString(pattern) {
		s.error(w, "not found", http.StatusNotFound)
		return
	}

	lane := r.URL.Query().Get("lane")
	if lane != "" && lane != "credential" && lane != "egress" {
		s.error(w, "invalid lane", http.StatusBadRequest)
		return
	}

	src := s.getPolicySource()
	if src == nil {
		s.error(w, "locked", http.StatusLocked)
		return
	}

	removed, err := src.RemoveRule(lane, pattern)
	if err != nil {
		switch {
		case errors.Is(err, ErrPolicyLocked) || err.Error() == "locked":
			s.error(w, "locked", http.StatusLocked)
		case errors.Is(err, ErrPolicyCorrupt) || err.Error() == "policy_corrupt":
			s.error(w, "policy_corrupt", http.StatusServiceUnavailable)
		default:
			s.error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}

	writeJSONAny(w, http.StatusOK, map[string]any{
		"ok":      true,
		"removed": removed,
	})
}
