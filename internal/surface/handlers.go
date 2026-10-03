package surface

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// SessionInfo is the wire shape of one session in GET /v1/sessions
// (spec §5: id, created, kind, optional title/parent/model_pin).
type SessionInfo struct {
	ID       string  `json:"id"`
	Created  string  `json:"created"`
	Kind     string  `json:"kind"`
	Title    *string `json:"title,omitempty"`
	Parent   *string `json:"parent,omitempty"`
	ModelPin *string `json:"model_pin,omitempty"`
}

// SessionSource is the storage view the handlers need: list, create,
// and replay session logs. PenatusSource implements it over the frozen
// Penatus file layer.
type SessionSource interface {
	List() ([]SessionInfo, error)
	Create(title string, modelPin string) (SessionInfo, error)
	Events(id string, afterSeq int64, limit int) (events []json.RawMessage, inFlight bool, err error)
}

// TurnHub is the live-turn view the handlers need: the in_flight flag
// for catch-up (spec §5) and POST /cancel.
type TurnHub interface {
	InFlight(sessionID string) bool
	Cancel(sessionID string) bool
}

// Server wires the HTTP surface: config, token store, session storage,
// and the turn hub. NewServer builds the handler chain.
type Server struct {
	Cfg      ServeConfig
	Store    *TokenStore
	Sessions SessionSource
	Hub      TurnHub
}

func (s *Server) healthHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSONBody(w, http.StatusOK, `{"ok":true,"version":"0.2.1"}`)
}

func (s *Server) listSessionsHandler(w http.ResponseWriter, _ *http.Request) {
	sessions, err := s.Sessions.List()
	if err != nil {
		s.error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSONAny(w, http.StatusOK, sessions)
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

	writeJSONAny(w, http.StatusCreated, session)
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

	writeJSONAny(w, http.StatusOK, map[string]any{
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
		writeJSONBody(w, http.StatusOK, `{"cancelled":true}`)
		return
	}

	s.error(w, "no turn in flight", http.StatusNotFound)
}

func (s *Server) error(w http.ResponseWriter, msg string, code int) {
	writeJSONAny(w, code, map[string]string{"error": msg})
}

// writeJSONAny marshals v first (a marshal failure is a server bug, not
// a client error) and answers with the bytes; a write failure means the
// client is gone and there is nobody left to tell.
func writeJSONAny(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func bodyLimitMiddleware(next http.Handler) http.Handler {
	const maxBody = 1 << 20 // 1 MiB
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBody {
			writeJSONBody(w, http.StatusRequestEntityTooLarge, `{"error":"body too large"}`)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}
