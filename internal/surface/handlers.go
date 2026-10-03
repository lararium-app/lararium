package surface

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

type SessionInfo struct {
	ID       string  `json:"id"`
	Created  string  `json:"created"`
	Kind     string  `json:"kind"`
	Title    *string `json:"title,omitempty"`
	Parent   *string `json:"parent,omitempty"`
	ModelPin *string `json:"model_pin,omitempty"`
}

type SessionSource interface {
	List() ([]SessionInfo, error)
	Create(title string, modelPin string) (SessionInfo, error)
	Events(id string, afterSeq int64, limit int) (events []json.RawMessage, inFlight bool, err error)
}

type TurnHub interface {
	InFlight(sessionID string) bool
	Cancel(sessionID string) bool
}

type Server struct {
	Cfg      ServeConfig
	Store    *TokenStore
	Sessions SessionSource
	Hub      TurnHub
}

func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"version": "0.1.0",
	})
}

func (s *Server) listSessionsHandler(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.Sessions.List()
	if err != nil {
		s.error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sessions)
}

func (s *Server) createSessionHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title    string `json:"title,omitempty"`
		ModelPin string `json:"model_pin,omitempty"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	session, err := s.Sessions.Create(req.Title, req.ModelPin)
	if err != nil {
		s.error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(session)
}

func (s *Server) eventsHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	id = strings.TrimSuffix(id, "/events")

	if !ValidSessionID(id) {
		s.error(w, "not found", http.StatusNotFound)
		return
	}

	afterSeq := int64(0)
	if v := r.URL.Query().Get("after_seq"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil || parsed < 0 {
			s.error(w, "invalid after_seq", http.StatusBadRequest)
			return
		}
		afterSeq = parsed
	}

	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed <= 0 {
			s.error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = parsed
	}

	if limit > 1000 {
		limit = 1000
	}

	events, inFlight, err := s.Sessions.Events(id, afterSeq, limit)
	if err != nil {
		if errors.Is(err, ErrNoSession) {
			s.error(w, "not found", http.StatusNotFound)
			return
		}
		s.error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"events":    events,
		"in_flight": inFlight,
	})
}

func (s *Server) cancelHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	id = strings.TrimSuffix(id, "/cancel")

	if !ValidSessionID(id) {
		s.error(w, "not found", http.StatusNotFound)
		return
	}

	if s.Hub.Cancel(id) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"cancelled": true})
		return
	}

	s.error(w, "no turn in flight", http.StatusNotFound)
}

func (s *Server) error(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func bodyLimitMiddleware(next http.Handler) http.Handler {
	const maxBody = 1 << 20 // 1 MiB
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBody {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			json.NewEncoder(w).Encode(map[string]string{"error": "body too large"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}
