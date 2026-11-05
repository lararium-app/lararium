package surface

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lararium-app/lararium/internal/web"
)

// webSessionRow is the external JSON representation of a web session.
// NEVER carries cookie contents or secrets.
type webSessionRow struct {
	Origin     string `json:"origin"`
	Policy     string `json:"policy"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at"`
	LastGoal   string `json:"last_goal,omitempty"`
}

func (s *Server) dispatchWebSessions(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/v1/web/sessions" {
		switch r.Method {
		case http.MethodGet:
			s.webSessionsListHandler(w, r)
		default:
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	rest := strings.TrimPrefix(p, "/v1/web/sessions/")
	if rest == "" || strings.Contains(rest, "/") {
		s.error(w, "not found", http.StatusNotFound)
		return
	}
	origin := rest

	switch r.Method {
	case http.MethodPatch:
		s.webSessionsPatchHandler(w, r, origin)
	case http.MethodDelete:
		s.webSessionsDeleteHandler(w, r, origin)
	default:
		s.error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) webSessionsListHandler(w http.ResponseWriter, _ *http.Request) {
	_, idx, _ := s.getWebDeps()
	if idx == nil {
		writeJSONAny(w, http.StatusOK, []webSessionRow{})
		return
	}

	entries, err := idx.List()
	if err != nil {
		writeJSONAny(w, http.StatusInternalServerError, map[string]string{"error": "failed to list sessions"})
		return
	}

	rows := make([]webSessionRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, webSessionRow{
			Origin:     e.Origin,
			Policy:     e.Policy,
			CreatedAt:  e.CreatedAt,
			LastUsedAt: e.LastUsedAt,
			LastGoal:   e.LastGoal,
		})
	}
	writeJSONAny(w, http.StatusOK, rows)
}

func (s *Server) webSessionsPatchHandler(w http.ResponseWriter, r *http.Request, origin string) {
	_, idx, _ := s.getWebDeps()
	if idx == nil {
		s.error(w, "web wallet not configured", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "reason": "bad_request"})
		return
	}

	var req struct {
		Policy string `json:"policy"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": "invalid json", "reason": "bad_json"})
		return
	}

	if !web.ValidPolicy(req.Policy) {
		writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": "invalid policy", "reason": "invalid_policy"})
		return
	}

	updated, err := idx.SetPolicy(origin, req.Policy)
	if err != nil {
		if errors.Is(err, web.ErrSessionNotFound) {
			s.error(w, "session not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, web.ErrInvalidPolicy) {
			writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": "invalid policy", "reason": "invalid_policy"})
			return
		}
		s.error(w, "update policy failed", http.StatusInternalServerError)
		return
	}

	writeJSONAny(w, http.StatusOK, webSessionRow{
		Origin:     updated.Origin,
		Policy:     updated.Policy,
		CreatedAt:  updated.CreatedAt,
		LastUsedAt: updated.LastUsedAt,
		LastGoal:   updated.LastGoal,
	})
}

func (s *Server) webSessionsDeleteHandler(w http.ResponseWriter, _ *http.Request, origin string) {
	wallet, idx, act := s.getWebDeps()
	if idx == nil {
		s.error(w, "web wallet not configured", http.StatusServiceUnavailable)
		return
	}

	_, existsInIndex := idx.Get(origin)
	existsInWallet := wallet != nil && wallet.Has(origin)

	if !existsInIndex && !existsInWallet {
		s.error(w, "session not found", http.StatusNotFound)
		return
	}

	if wallet != nil {
		_ = wallet.Delete(origin)
	}
	_, _ = idx.Delete(origin)

	if act != nil {
		_ = act.Append(web.ActivityRecord{
			TS:      time.Now().UTC().Format(time.RFC3339),
			Origin:  origin,
			URL:     "",
			Kind:    web.KindSignOut,
			Verdict: web.VerdictOK,
		})
	}

	w.WriteHeader(http.StatusNoContent)
}
