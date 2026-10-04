package surface

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/lararium-app/lararium/web"
)

// NewServer wires all routes behind the Host gate and the body limit.
// The inner mux owns dispatch; the returned handler is the full chain —
// never register the mux inside itself (that self-recurses on any
// unmatched path).
func NewServer(s *Server) http.Handler {
	mux := http.NewServeMux()

	// Static routes (no auth; Host gate still applies).
	RegisterStatic(mux, web.FS)

	// Health endpoint (no auth).
	mux.HandleFunc("/v1/health", s.healthHandler)

	// Everything under /v1/ requires a bearer token.
	mux.Handle("/v1/", BearerAuth(s.Store)(http.HandlerFunc(s.dispatch)))

	// Host gate first, then reject unclean paths before the mux can
	// 301 them (spec V4: fuzzed ids get a direct 404, no redirect dance).
	return HostGate(s.Cfg.AllowedHosts)(bodyLimitMiddleware(cleanPathGate(mux)))
}

// cleanPathGate answers dot-segment and double-slash paths with a JSON
// 404. Without it, net/http's mux issues a 301 to the cleaned path,
// which leaks redirect noise on every fuzzed id (spec V4).
func cleanPathGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p != "" && p != path.Clean(p) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// dispatch routes authenticated /v1/ requests.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/keys" || strings.HasPrefix(r.URL.Path, "/v1/keys/") {
		if s.dispatchKeys(w, r) {
			return
		}
	}
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

	// V12 (SURFACE-SPEC): SIGTERM/SIGINT resolves every pending
	// approval denied:shutdown, cancels running turns (append-only
	// integrity: a cancelled turn commits no partial assistant event),
	// and drains within 60 s.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(sigs)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	select {
	case err := <-serveErr:
		return err
	case <-sigs:
	}
	if s.Hub != nil {
		s.Hub.Shutdown()
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := srv.Shutdown(drainCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
