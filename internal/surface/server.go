package surface

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// NewServer wires all routes behind the Host gate and the body limit.
// The inner mux owns dispatch; the returned handler is the full chain —
// never register the mux inside itself (that self-recurses on any
// unmatched path).
func NewServer(s *Server) http.Handler {
	mux := http.NewServeMux()

	// Health endpoint (no auth).
	mux.HandleFunc("/v1/health", s.healthHandler)

	// Everything under /v1/ requires a bearer token.
	mux.Handle("/v1/", BearerAuth(s.Store)(http.HandlerFunc(s.dispatch)))

	// static routes land in T9

	// Anything else: JSON 404 (spec: errors are always {"error": string}).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.error(w, "not found", http.StatusNotFound)
	})

	return HostGate(s.Cfg.AllowedHosts)(bodyLimitMiddleware(mux))
}

// dispatch routes authenticated /v1/ requests.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/sessions" {
		switch r.Method {
		case http.MethodGet:
			s.listSessionsHandler(w, r)
		case http.MethodPost:
			s.createSessionHandler(w, r)
		default:
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v1/sessions/") {
		s.dispatchSession(w, r)
		return
	}
	s.error(w, "not found", http.StatusNotFound)
}

// dispatchSession handles /v1/sessions/{id}/... subroutes.
func (s *Server) dispatchSession(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")

	switch {
	case rest == "":
		s.error(w, "not found", http.StatusNotFound)
	case strings.HasSuffix(rest, "/events") && r.Method == http.MethodGet:
		s.eventsHandler(w, r)
	case strings.HasSuffix(rest, "/cancel") && r.Method == http.MethodPost:
		s.cancelHandler(w, r)
	case strings.HasSuffix(rest, "/messages") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(rest, "/messages")
		if !ValidSessionID(id) {
			s.error(w, "not found", http.StatusNotFound)
			return
		}
		hub, ok := s.Hub.(*Hub)
		if !ok {
			s.error(w, "not implemented", http.StatusNotImplemented)
			return
		}
		hub.HandleMessage(w, r, id)
	case strings.Contains(rest, "/approvals/") && r.Method == http.MethodPost:
		parts := strings.SplitN(rest, "/approvals/", 2)
		if len(parts) != 2 || parts[1] == "" {
			s.error(w, "not found", http.StatusNotFound)
			return
		}
		if !ValidSessionID(parts[0]) || !ValidApprovalID(parts[1]) {
			s.error(w, "not found", http.StatusNotFound)
			return
		}
		hub, ok := s.Hub.(*Hub)
		if !ok {
			s.error(w, "not implemented", http.StatusNotImplemented)
			return
		}
		hub.HandleApproval(w, r, parts[0], parts[1])
	default:
		s.error(w, "not found", http.StatusNotFound)
	}
}

// ListenAndServe normalizes config, binds, and serves until failure.
// Timeouts are set on the http.Server (gosec G114) EXCEPT WriteTimeout,
// which is deliberately zero: SSE turn streams are long-lived by design
// and bounded instead by serve.turn_timeout inside the turn engine.
func (s *Server) ListenAndServe() error {
	if err := s.Cfg.Normalize(); err != nil {
		return err
	}

	host, _, err := net.SplitHostPort(s.Cfg.Listen)
	if err != nil {
		host = s.Cfg.Listen
	}

	if !isLoopbackHost(host) {
		log.Printf("WARNING: listening beyond loopback on %s", s.Cfg.Listen)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", s.Cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	srv := &http.Server{
		Handler:           NewServer(s),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0, // request bodies stream; capped by body limit
		IdleTimeout:       120 * time.Second,
	}
	return srv.Serve(ln)
}
