package surface

import (
	"encoding/json"
	"errors"
	"fmt"
	stdlog "log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// CustosCard is the uniform wire shape for pending custody approval cards
// per CUSTOS-SPEC §6.4a and SURFACE-SPEC §4. Frozen: rail state never rides
// this shape — see ConnectorCard.
type CustosCard struct {
	ID         string `json:"id,omitempty"`
	Cell       string `json:"cell,omitempty"`
	Cred       string `json:"cred,omitempty"`
	Tool       string `json:"tool,omitempty"`
	Dest       string `json:"dest,omitempty"`
	Review     string `json:"review,omitempty"`
	AgeS       int    `json:"age_s,omitempty"`
	ExpiresInS int    `json:"expires_in_s,omitempty"`
}

// ConnectorCard is one rail connector's live state, served by the additive
// GET /v1/mcp/connectors endpoint ({"connectors": [...]}). It deliberately
// stays off the frozen CustosCard shape (CUSTOS-SPEC §6.4a): rail state is
// observable inventory, never a resolvable approval.
type ConnectorCard struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Tools    int    `json:"tools"`
	Restarts int    `json:"restarts"`
	Reason   string `json:"reason,omitempty"`
}

// MCPConnectorsSource is an optional interface a CustosRegistry implements
// when the MCP rail is wired. The surface serves /v1/mcp/connectors from it.
type MCPConnectorsSource interface {
	ConnectorSnapshot() []ConnectorCard
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

// SetApprovalHub bridges turn-approval cards into the custody card
// surface (issue #128): GET /v1/custos/cards merges pending turn cards,
// POST /v1/custos/cards/{id}/resolve falls back to the hub when the door
// registry does not know the id, and the SSE stream emits hub turn-card
// lifecycle. Safe for concurrent use and nil-safe.
func (s *Server) SetApprovalHub(h *ApprovalHub) {
	s.custosMu.Lock()
	defer s.custosMu.Unlock()
	s.approvals = h
	if h != nil {
		s.alwaysMu.RLock()
		hook := s.connectorTrustHook
		s.alwaysMu.RUnlock()
		if hook != nil {
			h.SetConnectorTrustHook(hook)
		}
	}
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
	// Issue #128: merge pending turn-approval cards. Hub turn cards carry
	// empty cred/cell, worker-lane cards come from the registry alone;
	// id de-duplication guards any transient mirror regardless.
	seen := make(map[string]bool, len(cards))
	for _, c := range cards {
		seen[c.ID] = true
	}
	if hub := s.approvalsViaRegistry(); hub != nil {
		for _, c := range hub.PendingTurnCards() {
			if !seen[c.ID] {
				cards = append(cards, c)
				seen[c.ID] = true
			}
		}
	}
	if cards == nil {
		cards = []CustosCard{}
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].ID < cards[j].ID })
	writeJSONAny(w, http.StatusOK, map[string]any{"cards": cards})
}

// approvalsViaRegistry returns the bridged ApprovalHub when the door
// registry is wired or explicitly absent — hub-only deployments still
// get turn cards on the custody surface (issue #128).
func (s *Server) approvalsViaRegistry() *ApprovalHub {
	s.custosMu.RLock()
	defer s.custosMu.RUnlock()
	return s.approvals
}

func (s *Server) custosResolveHandler(w http.ResponseWriter, r *http.Request, id string) {
	if !ValidApprovalID(id) {
		s.error(w, "not found", http.StatusNotFound)
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

	reg := s.getCustosRegistry()
	if reg != nil {
		state, err := reg.Resolve(id, req.Verdict)
		switch {
		case err == nil:
			if state == "" {
				state = registryAnswerState(req.Verdict)
			}
			writeJSONAny(w, http.StatusOK, map[string]string{"state": state})
			return
		case errors.Is(err, ErrNoSuchCard) || err.Error() == "no_such_card":
			// Fall through to the turn-approval hub (issue #128).
		default:
			switch {
			case errors.Is(err, ErrCardAnswered) || err.Error() == "already_answered":
				s.error(w, "already_answered", http.StatusConflict)
				return
			case errors.Is(err, ErrStaleVerdict) || err.Error() == "stale_verdict":
				s.error(w, "stale_verdict", http.StatusConflict)
				return
			case errors.Is(err, ErrLocked) || err.Error() == "locked":
				s.error(w, "locked", http.StatusLocked)
				return
			case errors.Is(err, ErrIPAskOnly) || err.Error() == "ip_ask_only":
				s.error(w, "ip_ask_only", http.StatusBadRequest)
				return
			case errors.Is(err, ErrBadSource) || err.Error() == "bad_source":
				s.error(w, "bad_source", http.StatusBadRequest)
				return
			default:
				if hub := s.approvalsViaRegistry(); hub != nil {
					if _, ok := hub.SessionOf(id); ok {
						break
					}
				}
				// #nosec G706 -- id is shape-validated by the card ID gate, logSanitize strips control chars
				stdlog.Printf("surface: custos card resolve %s failed: %v", logSanitize(id), err)
				s.error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
	}

	// Turn-approval fallback: ids the door registry never saw (web/MCP
	// rail approvals). Settles the live turn via the hub, source custos.
	if hub := s.approvalsViaRegistry(); hub != nil {
		// Issue #132: capture the tool name BEFORE settling so the
		// mint sees the card's own tool even if terminal entries are
		// pruned later.
		toolName := ""
		if req.Verdict == "always" {
			toolName, _ = hub.NameOf(id)
		}
		allow := req.Verdict != "deny"
		switch hub.ResolveFromVia("", id, allow, "custos", "") {
		case 200:
			if req.Verdict == "always" {
				s.mintTurnAlwaysRule(id, toolName)
			}
			writeJSONAny(w, http.StatusOK, map[string]string{"state": registryAnswerState(req.Verdict)})
			return
		case 410:
			s.error(w, "already_answered", http.StatusConflict)
			return
		}
	}

	// Spec ambiguity resolution: nothing knows this id → 404 no_such_card.
	s.error(w, "no_such_card", http.StatusNotFound)
}

// logSanitize removes control characters (newlines included) from
// request- and model-adjacent strings before they reach the log stream,
// so a hostile tool name or card id cannot forge log lines (gosec G706).
func logSanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
}

func registryAnswerState(verdict string) string {
	if verdict == "deny" {
		return "denied"
	}
	return "approved"
}

// SetAlwaysRuleHook attaches the card-minted always-rule minter used
// when a turn-approval card settles with verdict "always" (issue #132).
// The hook receives the model-facing tool name exactly as the approval
// was raised (mcp__<connector>__<tool> for rail tools); reverse-mapping
// and rule writing live in the wiring layer so surface never imports
// custody (frozen import direction). Safe for concurrent use; nil clears.
func (s *Server) SetAlwaysRuleHook(f func(toolName string) error) {
	s.alwaysMu.Lock()
	defer s.alwaysMu.Unlock()
	s.alwaysHook = f
}

// SetConnectorTrustHook registers the minter hook called when an approval card
// is resolved with scope "connector" (issue #142).
func (s *Server) SetConnectorTrustHook(f func(toolName string) error) {
	s.alwaysMu.Lock()
	s.connectorTrustHook = f
	s.alwaysMu.Unlock()

	s.custosMu.RLock()
	ap := s.approvals
	s.custosMu.RUnlock()
	if ap != nil {
		ap.SetConnectorTrustHook(f)
	}
}

func (s *Server) getAlwaysRuleHook() func(toolName string) error {
	s.alwaysMu.RLock()
	defer s.alwaysMu.RUnlock()
	return s.alwaysHook
}

// mintTurnAlwaysRule asks the wiring layer to persist the always-rule
// for a card just settled with "always" (CUSTOS-SPEC §6.5 card-minted
// posture, issue #132). The approval already settled when this runs, so
// a mint failure MUST NOT flip the resolve ack — it degrades the verdict
// to once and is logged loudly instead. Silent degradation was the bug.
func (s *Server) mintTurnAlwaysRule(cardID, toolName string) {
	hook := s.getAlwaysRuleHook()
	switch {
	case hook == nil:
		// #nosec G706 -- cardID is shape-validated ([a-z0-9]{24}) at dispatch and re-sanitized here
		stdlog.Printf("WARN issue #132: card %s settled 'always' but no always-rule hook is wired; verdict degraded to once", logSanitize(cardID))
	case toolName == "":
		// #nosec G706 -- sanitized above; ids and tool names cannot carry control chars here
		stdlog.Printf("WARN issue #132: card %s settled 'always' but its approval entry carried no tool name; verdict degraded to once", logSanitize(cardID))
	default:
		if err := hook(toolName); err != nil {
			// #nosec G706 -- both values pass logSanitize; err is engine text
			stdlog.Printf("ERROR issue #132: minting always-rule for card %s (tool %q) failed: %v; verdict degraded to once",
				logSanitize(cardID), logSanitize(toolName), err)
		}
	}
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

	push := func(card *CustosCard, gone *CustosGone) {
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
	}

	reg := s.getCustosRegistry()
	if reg != nil {
		cancel := reg.Subscribe(push)
		defer cancel()
	}
	// Issue #128: turn-approval cards ride the same stream.
	if hub := s.approvalsViaRegistry(); hub != nil {
		cancelHub := hub.SubscribeCustos(push)
		defer cancelHub()
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

// mcpConnectorsHandler serves GET /v1/mcp/connectors: the rail inventory.
// Answers {"connectors":[]} when no registry is wired or it does not expose
// connectors, so the web UI can poll unconditionally.
func (s *Server) mcpConnectorsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cards := []ConnectorCard{}
	s.custosMu.Lock()
	reg := s.custosReg
	s.custosMu.Unlock()
	if src, ok := reg.(MCPConnectorsSource); ok {
		if got := src.ConnectorSnapshot(); got != nil {
			cards = got
		}
	}
	writeJSONAny(w, http.StatusOK, map[string]any{"connectors": cards})
}
