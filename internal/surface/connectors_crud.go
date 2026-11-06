package surface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Connector lifecycle REST (CONNECTOR-LIFE-SPEC D3, D4, D7, D8).
//
// AUTH: every route is served behind the same BearerAuth gate as
// /v1/credentials (server.go mounts /v1/ behind it, so a missing or invalid
// token answers 401 before dispatch and no door verb is ever sent).
//
// SECRETS: request configs carry literal secrets (env values, bearer, URL
// userinfo). They flow only to custosd over doors.sock. Nothing in this file
// writes a config value into a response, card, event or log line — responses
// and cards are built from connectorMeta / review text, which hold names,
// flags, fingerprints and userinfo-stripped targets only.

// DefaultConnectorCardHold is how long an add/edit custody card waits for a
// verdict before it auto-denies (mirrors custosd's default ask hold).
const DefaultConnectorCardHold = 330 * time.Second

// connectorSettleWait bounds how long enable/disable/reauth wait for the
// door mirror to reflect the change before answering with the latest row.
// A variable so tests stay fast.
var connectorSettleWait = 2 * time.Second

const maxConnectorBody = 64 << 10

// Sentinels the ConnectorDoor adapter maps door failures onto.
var (
	// ErrConnectorDoorDown: no live door connection (503).
	ErrConnectorDoorDown = errors.New("custos door not connected")
	// ErrConnectorDaemonTooOld: custosd does not know the lifecycle verbs (502).
	ErrConnectorDaemonTooOld = errors.New("custos daemon too old for connector lifecycle")
)

// ConnectorReasonError carries a closed, secret-free refusal reason from
// custosd ("ERR <reason>").
type ConnectorReasonError struct{ Reason string }

func (e *ConnectorReasonError) Error() string { return e.Reason }

// SetConnectorCardHold sets the add/edit card hold (ask_hold_timeout);
// <= 0 restores the default. Safe for concurrent use.
func (s *Server) SetConnectorCardHold(d time.Duration) {
	s.connLife.mu.Lock()
	s.connLife.hold = int64(d)
	s.connLife.mu.Unlock()
}

func (s *Server) connectorCardHold() time.Duration {
	s.connLife.mu.Lock()
	defer s.connLife.mu.Unlock()
	if s.connLife.hold <= 0 {
		return DefaultConnectorCardHold
	}
	return time.Duration(s.connLife.hold)
}

func (s *Server) connError(w http.ResponseWriter, code int, msg, reason string) {
	writeJSONAny(w, code, map[string]string{"error": msg, "reason": reason})
}

// dispatchMCPConnectors routes /v1/mcp/connector-form and /v1/mcp/connectors[/...].
func (s *Server) dispatchMCPConnectors(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch p {
	case "/v1/mcp/connector-form":
		s.connectorFormHandler(w, r)
	case "/v1/mcp/connectors":
		switch r.Method {
		case http.MethodGet:
			s.mcpConnectorsHandler(w, r)
		case http.MethodPost:
			s.connectorCreateHandler(w, r)
		default:
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case "/v1/mcp/connectors/events":
		if r.Method != http.MethodGet {
			s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.connectorEventsHandler(w, r)
	default:
		rest := strings.TrimPrefix(p, "/v1/mcp/connectors/")
		name, action, hasAction := strings.Cut(rest, "/")
		if !connectorNameRe.MatchString(name) || (hasAction && strings.Contains(action, "/")) {
			s.error(w, "not found", http.StatusNotFound)
			return
		}
		switch {
		case !hasAction:
			switch r.Method {
			case http.MethodPatch:
				s.connectorPatchHandler(w, r, name)
			case http.MethodDelete:
				s.connectorDeleteHandler(w, r, name)
			default:
				s.error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		case action == "enable" || action == "disable" || action == "reauth":
			if r.Method != http.MethodPost {
				s.error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			switch action {
			case "reauth":
				s.connectorReauthHandler(w, r, name)
			default:
				s.connectorToggleHandler(w, r, name, action == "enable")
			}
		default:
			s.error(w, "not found", http.StatusNotFound)
		}
	}
}

// requireConnectorDoor answers 503 when the door plane is not wired.
func (s *Server) requireConnectorDoor(w http.ResponseWriter) ConnectorDoor {
	d := s.getConnectorDoor()
	if d == nil {
		s.connError(w, http.StatusServiceUnavailable, "connector door unavailable", "door_not_connected")
	}
	return d
}

// connDoorError maps a door failure onto the response. probe selects the D3
// rule that a refused probe is a 400 carrying the probe's reason.
func (s *Server) connDoorError(w http.ResponseWriter, op string, err error, probe bool) {
	var re *ConnectorReasonError
	switch {
	case errors.Is(err, ErrConnectorDoorDown):
		s.connError(w, http.StatusServiceUnavailable, "custos door not connected", "door_down")
	case errors.Is(err, ErrConnectorDaemonTooOld):
		s.connError(w, http.StatusBadGateway, "daemon too old: connector lifecycle verbs are not supported by this custosd", "daemon_too_old")
	case isLockedErr(err):
		s.connError(w, http.StatusLocked, "locked", "locked")
	case errors.As(err, &re):
		switch {
		case probe:
			s.connError(w, http.StatusBadRequest, "probe failed", re.Reason)
		case re.Reason == "not_found":
			s.connError(w, http.StatusNotFound, "not found", "not_found")
		case strings.HasPrefix(re.Reason, "bad_") || re.Reason == "unknown_key" ||
			re.Reason == "missing_key" || re.Reason == "reserved_name":
			s.connError(w, http.StatusBadRequest, "invalid connector config", re.Reason)
		default:
			s.connError(w, http.StatusBadGateway, "daemon refused", re.Reason)
		}
	default:
		// #nosec G706 -- op is a constant; err text is door transport text, sanitized.
		log.Printf("surface: connector %s door error: %s", op, logSanitize(err.Error()))
		s.connError(w, http.StatusBadGateway, "door error", "door_error")
	}
}

func readConnectorBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxConnectorBody+1))
	if err != nil || len(raw) > maxConnectorBody {
		writeJSONAny(w, http.StatusBadRequest, map[string]string{"error": "invalid request body", "reason": connReasonBadRequest})
		return nil, false
	}
	return raw, true
}

func (s *Server) connectorCreateHandler(w http.ResponseWriter, r *http.Request) {
	door := s.requireConnectorDoor(w)
	if door == nil {
		return
	}
	body, ok := readConnectorBody(w, r)
	if !ok {
		return
	}
	s.runConnectorGate(w, r, door, body, "", false)
}

func (s *Server) connectorPatchHandler(w http.ResponseWriter, r *http.Request, name string) {
	door := s.requireConnectorDoor(w)
	if door == nil {
		return
	}
	body, ok := readConnectorBody(w, r)
	if !ok {
		return
	}
	s.runConnectorGate(w, r, door, body, name, true)
}

// runConnectorGate is the D3/D4 pipeline: validate -> duplicate/pending
// check -> MCP-PROBE -> custody card -> 202, then an async verdict waiter.
func (s *Server) runConnectorGate(w http.ResponseWriter, r *http.Request, door ConnectorDoor, body []byte, pathName string, edit bool) {
	if edit {
		// PATCH carries the full replacement config; the path owns the name.
		var top map[string]json.RawMessage
		if err := json.Unmarshal(body, &top); err != nil || top == nil {
			s.connError(w, http.StatusBadRequest, "invalid connector config", connReasonBadRequest)
			return
		}
		if raw, has := top["name"]; has {
			var got string
			if json.Unmarshal(raw, &got) != nil || got != pathName {
				s.connError(w, http.StatusBadRequest, "invalid connector config", connReasonBadName)
				return
			}
		} else {
			// Re-marshaling raw fragments can fail on pathological input;
			// treat that as a bad config, never as a silent corrupt save.
			nameJSON, nErr := json.Marshal(pathName)
			if nErr != nil {
				s.connError(w, http.StatusBadRequest, "invalid connector config", connReasonBadRequest)
				return
			}
			top["name"] = nameJSON
			rebuilt, rErr := json.Marshal(top)
			if rErr != nil {
				s.connError(w, http.StatusBadRequest, "invalid connector config", connReasonBadRequest)
				return
			}
			body = rebuilt
		}
	}
	defer zeroBytes(body)

	// (a) strict validation.
	cfg, reason := validateConnectorConfig(body)
	if reason != "" {
		s.connError(w, http.StatusBadRequest, "invalid connector config", reason)
		return
	}
	name := cfg.Name

	hub := s.approvalsViaRegistry()
	if hub == nil {
		s.connError(w, http.StatusServiceUnavailable, "approval cards unavailable", "approvals_unavailable")
		return
	}

	// (b) duplicate / pending check against the current snapshot.
	committed := false
	for _, st := range door.ConnectorStatuses() {
		if st.Name == name {
			committed = true
			break
		}
	}
	switch {
	case !edit && committed:
		s.connError(w, http.StatusConflict, "connector already exists; edit it instead", "duplicate_name")
		return
	case edit && !committed:
		s.connError(w, http.StatusNotFound, "not found", "not_found")
		return
	}
	newMeta := connectorMeta{URLOrCommand: cfg.targetSummary(), Env: cfg.envNames()}
	if !s.reservePending(name, edit, newMeta) {
		s.connError(w, http.StatusConflict, "an add or edit of this connector is already pending approval", "pending_add")
		return
	}
	if !edit {
		s.pokeConnectorRail()
	}

	// Snapshot the current shape for the edit diff BEFORE the probe.
	var old connectorOld
	if edit {
		s.connLife.mu.Lock()
		m, known := s.connLife.meta[name]
		s.connLife.mu.Unlock()
		old = connectorOld{
			edit: true, tools: parseConnectorTools(door.ConnectorTools(name)),
			meta: m, metaKnown: known,
		}
	}

	// (c) probe: failure = 400 with the probe's reason, no card.
	probe, err := door.Probe(r.Context(), body)
	if err != nil {
		s.releasePending(name, "")
		s.pokeConnectorRail()
		s.connDoorError(w, "probe", err, true)
		return
	}

	// (d) raise the custody card.
	review := connectorReviewText(cfg, probe, old)
	label := "mcp-add:" + name
	if edit {
		label = "mcp-edit:" + name
	}
	hold := s.connectorCardHold()
	cardID, decision := hub.registerCustosNamed("", "", label, "", review, hold, nil)
	meta := newMeta
	meta.Fingerprint = cleanFingerprint(probe.Fingerprint)
	s.setPendingCard(name, cardID, meta)

	// The waiter needs the config past this handler's return; take a copy
	// (zeroed once the verdict is applied).
	held := append([]byte(nil), body...)
	s.verdictWg.Add(1)
	//nolint:contextcheck // the verdict may land after this handler returns; the waiter enforces its own hold deadline.
	go func() {
		defer s.verdictWg.Done()
		s.awaitConnectorVerdict(door, hub, name, cardID, decision, held, meta, hold)
	}()

	// (e) 202 immediately — name and card id only.
	writeJSONAny(w, http.StatusAccepted, map[string]string{"name": name, "card_id": cardID})
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func (s *Server) reservePending(name string, edit bool, meta connectorMeta) bool {
	s.connLife.mu.Lock()
	defer s.connLife.mu.Unlock()
	if s.connLife.pending == nil {
		s.connLife.pending = make(map[string]*pendingConnector)
	}
	if _, busy := s.connLife.pending[name]; busy {
		return false
	}
	s.connLife.pending[name] = &pendingConnector{name: name, edit: edit, meta: meta}
	return true
}

func (s *Server) setPendingCard(name, cardID string, meta connectorMeta) {
	s.connLife.mu.Lock()
	if p := s.connLife.pending[name]; p != nil {
		p.cardID = cardID
		p.meta = meta
	}
	s.connLife.mu.Unlock()
	s.pokeConnectorRail()
}

// releasePending clears the gate for name. cardID "" clears unconditionally
// (probe-failure path); otherwise only the entry owned by that card.
func (s *Server) releasePending(name, cardID string) {
	s.connLife.mu.Lock()
	if p := s.connLife.pending[name]; p != nil && (cardID == "" || p.cardID == cardID) {
		delete(s.connLife.pending, name)
	}
	s.connLife.mu.Unlock()
}

// awaitConnectorVerdict settles one add/edit. Allow -> MCP-ADD (custosd
// persists and audits mcp_added/mcp_edited). Deny or hold expiry (the
// decision channel's false) -> nothing was ever sent or persisted here.
// The mcp_add_denied audit record is dispatched through the door server's
// terminal approval fanout.
func (s *Server) awaitConnectorVerdict(
	door ConnectorDoor, hub *ApprovalHub, name, cardID string,
	decision <-chan bool, body []byte, meta connectorMeta, hold time.Duration,
) {
	defer zeroBytes(body)
	safety := time.NewTimer(hold + 10*time.Second)
	defer safety.Stop()

	allow := false
	select {
	case allow = <-decision:
	case <-safety.C:
		hub.CancelCard(cardID, "timeout", "custos")
	}

	if !allow {
		// #nosec G706 -- name is grammar-validated; sanitized anyway.
		log.Printf("surface: connector %q add/edit not approved (denied or hold expired); nothing persisted", logSanitize(name))
		s.releasePending(name, cardID)
		s.pokeConnectorRail()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := door.Add(ctx, name, body); err != nil {
		// #nosec G706 -- name sanitized; err is a closed door reason/transport text.
		log.Printf("surface: connector %q approved but MCP-ADD failed: %s", logSanitize(name), logSanitize(err.Error()))
		s.releasePending(name, cardID)
		s.pokeConnectorRail()
		return
	}
	s.connLife.mu.Lock()
	if s.connLife.meta == nil {
		s.connLife.meta = make(map[string]connectorMeta)
	}
	s.connLife.meta[name] = meta
	s.connLife.mu.Unlock()
	s.releasePending(name, cardID)
	s.pokeConnectorRail()
}

// cancelPendingFor cancels a pending add/edit card for name (DELETE wins).
func (s *Server) cancelPendingFor(name string) {
	s.connLife.mu.Lock()
	p := s.connLife.pending[name]
	var id string
	if p != nil {
		id = p.cardID
	}
	s.connLife.mu.Unlock()
	if id == "" {
		return
	}
	if hub := s.approvalsViaRegistry(); hub != nil {
		hub.CancelCard(id, "connector_removed", "custos")
	}
}

func (s *Server) connectorDeleteHandler(w http.ResponseWriter, r *http.Request, name string) {
	door := s.requireConnectorDoor(w)
	if door == nil {
		return
	}
	// Idempotent: custosd answers OK for an unknown name, and a not_found
	// refusal is treated the same — DELETE of an absent connector is 204.
	if err := door.Remove(r.Context(), name); err != nil {
		var re *ConnectorReasonError
		if !errors.As(err, &re) || re.Reason != "not_found" {
			s.connDoorError(w, "remove", err, false)
			return
		}
	}
	s.cancelPendingFor(name)
	s.connLife.mu.Lock()
	delete(s.connLife.meta, name)
	s.connLife.mu.Unlock()
	s.pokeConnectorRail()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) connectorToggleHandler(w http.ResponseWriter, r *http.Request, name string, enable bool) {
	door := s.requireConnectorDoor(w)
	if door == nil {
		return
	}
	if err := door.SetState(r.Context(), name, enable); err != nil {
		s.connDoorError(w, "state", err, false)
		return
	}
	row := s.settledRow(r.Context(), name, func(c ConnectorCard) bool { return c.Enabled == enable })
	writeJSONAny(w, http.StatusOK, row)
}

func (s *Server) connectorReauthHandler(w http.ResponseWriter, r *http.Request, name string) {
	door := s.requireConnectorDoor(w)
	if door == nil {
		return
	}
	if err := door.Refresh(r.Context(), name); err != nil {
		var re *ConnectorReasonError
		if errors.As(err, &re) && re.Reason != "not_found" {
			// Frozen failure reason; the connector stays needs-re-auth.
			s.connError(w, http.StatusConflict, "re-authentication failed", re.Reason)
			return
		}
		s.connDoorError(w, "reauth", err, false)
		return
	}
	row := s.settledRow(r.Context(), name, func(c ConnectorCard) bool { return c.AppState != AppStateNeedsReauth })
	writeJSONAny(w, http.StatusOK, row)
}

// settledRow returns the row for name once pred holds, waiting briefly for
// the door's pushed status broadcast; on expiry it answers with the latest.
func (s *Server) settledRow(ctx context.Context, name string, pred func(ConnectorCard) bool) ConnectorCard {
	deadline := time.Now().Add(connectorSettleWait)
	for {
		row, ok := s.connectorRow(name)
		if ok && pred(row) {
			return row
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			if !ok {
				return ConnectorCard{Name: name, ToolList: []ConnectorTool{}, Env: []string{}}
			}
			return row
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// connectorOld is the current shape an edit card diffs against.
type connectorOld struct {
	edit      bool
	tools     []ConnectorTool
	meta      connectorMeta
	metaKnown bool
}

// sanitizeReviewText neutralizes control characters and bounds a
// child-supplied string (tool names are untrusted input on a card).
func sanitizeReviewText(s string, maxRunes int) string {
	s = logSanitize(s)
	if utf8.RuneCountInString(s) > maxRunes {
		r := []rune(s)
		s = string(r[:maxRunes]) + "…"
	}
	return s
}

func cleanFingerprint(fp string) string {
	if fp == "" || len(fp) > 128 {
		return ""
	}
	for _, c := range fp {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return ""
		}
	}
	return fp
}

func toolFlagsText(t ConnectorTool) string {
	var f []string
	if t.ReadOnly {
		f = append(f, "read_only")
	}
	if t.Destructive {
		f = append(f, "destructive")
	}
	if t.OpenWorld {
		f = append(f, "open_world")
	}
	if len(f) == 0 {
		return "none"
	}
	return strings.Join(f, ",")
}

const maxReviewTools = 50

// connectorReviewText builds the custody card review: name, transport,
// target, tools with flags, fingerprint and env NAMES. For an edit it adds
// the diff against the current shape. It is assembled only from validated,
// sanitized parts — never from env values, bearer or URL userinfo/query.
func connectorReviewText(cfg connectorConfig, probe ConnectorProbe, old connectorOld) string {
	var b strings.Builder
	verb := "ADD"
	if old.edit {
		verb = "EDIT"
	}
	fmt.Fprintf(&b, "%s connector %q\n", verb, cfg.Name)
	fmt.Fprintf(&b, "transport: %s\n", cfg.Transport)
	if cfg.Transport == "http" {
		fmt.Fprintf(&b, "url: %s\n", cfg.targetSummary())
	} else {
		fmt.Fprintf(&b, "command: %s\n", cfg.targetSummary())
		b.WriteString("(arguments are shown as supplied; secret-looking flag values are masked)\n")
	}
	if cfg.Bearer != "" {
		b.WriteString("auth: bearer configured (value hidden)\n")
	}
	env := cfg.envNames()
	if len(env) == 0 {
		b.WriteString("env: none\n")
	} else {
		fmt.Fprintf(&b, "env (names only): %s\n", strings.Join(env, ", "))
	}

	newTools := parseConnectorTools(probe.Tools)
	fmt.Fprintf(&b, "tools (%d):\n", len(newTools))
	for i, t := range newTools {
		if i == maxReviewTools {
			fmt.Fprintf(&b, "  … and %d more\n", len(newTools)-maxReviewTools)
			break
		}
		fmt.Fprintf(&b, "  - %s [%s]\n", sanitizeReviewText(t.Name, 64), toolFlagsText(t))
	}
	fp := cleanFingerprint(probe.Fingerprint)
	if fp == "" {
		fp = "(unavailable)"
	}
	fmt.Fprintf(&b, "fingerprint: %s\n", fp)

	if old.edit {
		b.WriteString("changes vs current:\n")
		added, removed := diffToolNames(old.tools, newTools)
		fmt.Fprintf(&b, "  tools added: %s\n", listOrNone(added))
		fmt.Fprintf(&b, "  tools removed: %s\n", listOrNone(removed))
		oldFP := cleanFingerprint(old.meta.Fingerprint)
		if !old.metaKnown || oldFP == "" {
			oldFP = "(unknown)"
		}
		fmt.Fprintf(&b, "  fingerprint: %s -> %s\n", oldFP, fp)
		if !old.metaKnown {
			fmt.Fprintf(&b, "  env: now [%s]; previous set unknown\n", strings.Join(env, ", "))
		} else {
			ea, er := diffStrings(old.meta.Env, env)
			fmt.Fprintf(&b, "  env added: %s; removed: %s\n", listOrNone(ea), listOrNone(er))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func listOrNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

func diffToolNames(oldT, newT []ConnectorTool) (added, removed []string) {
	o := make([]string, 0, len(oldT))
	for _, t := range oldT {
		o = append(o, sanitizeReviewText(t.Name, 64))
	}
	n := make([]string, 0, len(newT))
	for _, t := range newT {
		n = append(n, sanitizeReviewText(t.Name, 64))
	}
	return diffStrings(o, n)
}

// diffStrings returns (in b not a, in a not b), sorted.
func diffStrings(a, b []string) (added, removed []string) {
	as := make(map[string]bool, len(a))
	for _, x := range a {
		as[x] = true
	}
	bs := make(map[string]bool, len(b))
	for _, x := range b {
		bs[x] = true
		if !as[x] {
			added = append(added, x)
		}
	}
	for _, x := range a {
		if !bs[x] {
			removed = append(removed, x)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}
