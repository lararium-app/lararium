package surface

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CustosCard is the uniform wire shape for pending custody approval cards
// per CUSTOS-SPEC §6.4a and SURFACE-SPEC §4.
// Field names and JSON tags match internal/custos.ApprovalCardWire verbatim.
type CustosCard struct {
	ID         string `json:"id"`
	Cell       string `json:"cell"`
	Cred       string `json:"cred"`
	Tool       string `json:"tool"`
	Dest       string `json:"dest"`
	Review     string `json:"review"`
	AgeS       int    `json:"age_s"`
	ExpiresInS int    `json:"expires_in_s"`
}

// CustosGone represents a terminal transition for a custody card per CUSTOS-SPEC §6.4b.
type CustosGone struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// CustosEvent is a single custody card event. Exactly one of Card or Gone is non-nil.
type CustosEvent struct {
	Card *CustosCard
	Gone *CustosGone
}

// Sentinel error values for CustosRegistry.Resolve matching the frozen ack set
// per CUSTOS-SPEC §6.4b and SURFACE-SPEC §4.
var (
	ErrCardAnswered = errors.New("already_answered")
	ErrStaleVerdict = errors.New("stale_verdict")
	ErrLocked       = errors.New("locked")
	ErrNoSuchCard   = errors.New("no_such_card")
	ErrIPAskOnly    = errors.New("ip_ask_only")
	ErrBadSource    = errors.New("bad_source")
)

// CustosRegistry is the interface connecting the web surface to the door client
// (which speaks doors.sock per CUSTOS-SPEC §6.4b).
//
// Concurrency and non-blocking contract:
//   - Callbacks passed to Subscribe must not block the publisher (the hearthd door
//     client). Implementations may drop-to-dead on slow subscribers; the surface
//     delivers via its own buffered channel (cap 64) per SSE client.
//   - Implementers must make Attach atomic (snapshot + subscribe under the same lock,
//     mirroring the hearthd HELLO-replay pattern) so no events are lost or duplicated.
type CustosRegistry interface {
	Snapshot() []CustosCard
	Subscribe(f func(card *CustosCard, gone *CustosGone)) (cancel func())
	Resolve(id, verdict string) (state string, err error)
	Attach() (cards []CustosCard, cancel func(), events <-chan CustosEvent)
}

// SetCustosRegistry attaches a CustosRegistry to the server.
// Safe for concurrent use and nil-safe.
func (s *Server) SetCustosRegistry(r CustosRegistry) {
	s.custosMu.Lock()
	defer s.custosMu.Unlock()
	s.custosReg = r
}

func (s *Server) getCustosRegistry() CustosRegistry {
	s.custosMu.RLock()
	defer s.custosMu.RUnlock()
	return s.custosReg
}

// custosHeartbeatInterval is the SSE : ping interval (default 5 s per SURFACE-SPEC §5).
// Internal tests may adjust this for fast execution without sleeps >100ms.
var custosHeartbeatInterval = 5 * time.Second

// dispatchCustos routes /v1/custos/cards, /v1/custos/cards/events, and
// /v1/custos/cards/{id}/resolve. Returns true if the path is under /v1/custos/cards.
//
//nolint:unparam // always-true by design: every /v1/custos/cards* path is fully handled here (server.go early-returns)
func (s *Server) dispatchCustos(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/v1/custos/cards" {
		if r.Method != http.MethodGet {
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		s.custosCardsHandler(w, r)
		return true
	}

	if r.URL.Path == "/v1/custos/cards/events" {
		if r.Method != http.MethodGet {
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		s.custosEventsHandler(w, r)
		return true
	}

	if strings.HasPrefix(r.URL.Path, "/v1/custos/cards/") && strings.HasSuffix(r.URL.Path, "/resolve") {
		id := strings.TrimPrefix(r.URL.Path, "/v1/custos/cards/")
		id = strings.TrimSuffix(id, "/resolve")
		if strings.Contains(id, "/") || id == "" {
			s.error(w, "not found", http.StatusNotFound)
			return true
		}
		if r.Method != http.MethodPost {
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		s.custosResolveHandler(w, r, id)
		return true
	}

	s.error(w, "not found", http.StatusNotFound)
	return true
}

func (s *Server) custosCardsHandler(w http.ResponseWriter, _ *http.Request) {
	reg := s.getCustosRegistry()
	var cards []CustosCard
	if reg != nil {
		cards = reg.Snapshot()
	}
	if cards == nil {
		cards = []CustosCard{}
	}
	writeJSONAny(w, http.StatusOK, map[string]any{"cards": cards})
}

func (s *Server) custosResolveHandler(w http.ResponseWriter, r *http.Request, id string) {
	if !ValidApprovalID(id) {
		s.error(w, "not found", http.StatusNotFound)
		return
	}

	reg := s.getCustosRegistry()
	if reg == nil {
		// Spec ambiguity resolution: unset registry answers 404 no_such_card.
		s.error(w, "no_such_card", http.StatusNotFound)
		return
	}

	var req struct {
		Verdict string `json:"verdict"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Verdict != "once" && req.Verdict != "always" && req.Verdict != "deny" {
		s.error(w, "invalid verdict", http.StatusBadRequest)
		return
	}

	state, err := reg.Resolve(id, req.Verdict)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoSuchCard) || err.Error() == "no_such_card":
			s.error(w, "no_such_card", http.StatusNotFound)
		case errors.Is(err, ErrCardAnswered) || err.Error() == "already_answered":
			s.error(w, "already_answered", http.StatusConflict)
		case errors.Is(err, ErrStaleVerdict) || err.Error() == "stale_verdict":
			s.error(w, "stale_verdict", http.StatusConflict)
		case errors.Is(err, ErrLocked) || err.Error() == "locked":
			s.error(w, "locked", http.StatusLocked)
		case errors.Is(err, ErrIPAskOnly) || err.Error() == "ip_ask_only":
			s.error(w, "ip_ask_only", http.StatusBadRequest)
		case errors.Is(err, ErrBadSource) || err.Error() == "bad_source":
			s.error(w, "bad_source", http.StatusBadRequest)
		default:
			s.error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	if state == "" {
		if req.Verdict == "deny" {
			state = "denied"
		} else {
			state = "approved"
		}
	}
	writeJSONAny(w, http.StatusOK, map[string]string{"state": state})
}

func (s *Server) custosEventsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONBody(w, http.StatusInternalServerError, `{"error":"streaming unsupported"}`)
		return
	}
	flusher.Flush()

	events := make(chan CustosEvent, 64)
	overflowCh := make(chan struct{})
	var overflowOnce sync.Once

	reg := s.getCustosRegistry()
	if reg != nil {
		cancel := reg.Subscribe(func(card *CustosCard, gone *CustosGone) {
			if card == nil && gone == nil {
				return
			}
			ev := CustosEvent{Card: card, Gone: gone}
			select {
			case events <- ev:
			default:
				// Slow client buffer overflow (cap 64): close stream to trigger reconnect resync.
				overflowOnce.Do(func() {
					close(overflowCh)
				})
			}
		})
		defer cancel()
	}

	ticker := time.NewTicker(custosHeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-overflowCh:
			return
		case <-ticker.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case ev := <-events:
			if ev.Card != nil {
				//nolint:gosec // G117: Cred carries a provider/credential NAME, never a secret value
				data, err := json.Marshal(ev.Card)
				if err == nil {
					_, _ = fmt.Fprintf(w, "event: custos_card\ndata: %s\n\n", data)
					flusher.Flush()
				}
			} else if ev.Gone != nil {
				data, err := json.Marshal(ev.Gone)
				if err == nil {
					_, _ = fmt.Fprintf(w, "event: custos_gone\ndata: %s\n\n", data)
					flusher.Flush()
				}
			}
		}
	}
}
