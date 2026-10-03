package surface

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
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
	switch r.URL.Path {
	case "/v1/sessions":
		switch r.Method {
		case http.MethodGet:
			s.listSessionsHandler(w, r)
		case http.MethodPost:
			s.createSessionHandler(w, r)
		default:
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	default:
		if strings.HasPrefix(r.URL.Path, "/v1/sessions/") {
			if strings.HasSuffix(r.URL.Path, "/events") && r.Method == http.MethodGet {
				s.eventsHandler(w, r)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/cancel") && r.Method == http.MethodPost {
				s.cancelHandler(w, r)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/messages") && r.Method == http.MethodPost {
				id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
				id = strings.TrimSuffix(id, "/messages")
				if !ValidSessionID(id) {
					s.error(w, "not found", http.StatusNotFound)
					return
				}
				if hub, ok := s.Hub.(*Hub); ok {
					hub.HandleMessage(w, r, id)
				} else {
					s.error(w, "not implemented", http.StatusNotImplemented)
				}
				return
			}
			if strings.HasPrefix(r.URL.Path, "/v1/sessions/") && strings.Contains(r.URL.Path, "/approvals/") && r.Method == http.MethodPost {
				id := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
				parts := strings.Split(id, "/approvals/")
				if len(parts) == 2 {
					sessionID := parts[0]
					approvalID := parts[1]
					if !ValidSessionID(sessionID) || !ValidApprovalID(approvalID) {
						s.error(w, "not found", http.StatusNotFound)
						return
					}
					if hub, ok := s.Hub.(*Hub); ok {
						hub.HandleApproval(w, r, sessionID, approvalID)
					} else {
						s.error(w, "not implemented", http.StatusNotImplemented)
					}
					return
				}
			}
		}
		s.error(w, "not found", http.StatusNotFound)
	}
}

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

	ln, err := net.Listen("tcp", s.Cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	return http.Serve(ln, NewServer(s))
}
