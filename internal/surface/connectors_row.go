package surface

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

// App-facing connector states (CONNECTOR-LIFE-SPEC D6) — exactly these five.
const (
	AppStateNotConnected = "not-connected"
	AppStateNeedsReauth  = "needs-re-auth"
	AppStateReadOnly     = "read-only"
	AppStateReadAct      = "read+act"
	AppStateDisabled     = "disabled"
)

// Rail states/reasons the surface interprets (the frozen MCP-SPEC vocabulary).
const (
	railStateReady    = "ready"
	railStateDisabled = "disabled"
	railReasonUser    = "user_disabled"
	// connStatePending marks an add awaiting its custody verdict; it exists
	// only on the phone rail (the daemon has no such connector yet).
	connStatePending       = "pending"
	connReasonPendingAppvl = "pending_approval"
)

// ConnectorTool is one tool's review flags (from the mirrored MCP-TOOLS
// flags string: "none" or a comma list of read_only,destructive,open_world).
type ConnectorTool struct {
	Name        string `json:"name"`
	ReadOnly    bool   `json:"read_only"`
	Destructive bool   `json:"destructive"`
	OpenWorld   bool   `json:"open_world"`
}

// ConnectorDoorStatus is one rail status row as mirrored by the door client.
type ConnectorDoorStatus struct {
	Name     string
	State    string
	Reason   string
	Tools    int
	Restarts int
	// LastUsed is RFC3339 or nil.
	LastUsed *string
}

// ConnectorDoorTool is one mirrored tool row (Flags is the wire flags string).
type ConnectorDoorTool struct {
	Name  string
	Flags string
}

// parseConnectorTool maps a mirrored tool row onto review flags.
func parseConnectorTool(t ConnectorDoorTool) ConnectorTool {
	out := ConnectorTool{Name: t.Name}
	for _, f := range strings.Split(t.Flags, ",") {
		switch strings.TrimSpace(f) {
		case "read_only":
			out.ReadOnly = true
		case "destructive":
			out.Destructive = true
		case "open_world":
			out.OpenWorld = true
		}
	}
	return out
}

func parseConnectorTools(in []ConnectorDoorTool) []ConnectorTool {
	out := make([]ConnectorTool, 0, len(in))
	for _, t := range in {
		out = append(out, parseConnectorTool(t))
	}
	return out
}

// connectorUserEnabled reports the user toggle: a connector is "enabled"
// unless the user switched it off (disabled/user_disabled). Other disabled
// reasons (name_occupied, credential_unavailable) are NOT user toggles.
func connectorUserEnabled(state, reason string) bool {
	return !(state == railStateDisabled && reason == railReasonUser)
}

// connectorCredentialReason reports the A7 credential latch / handle-gone
// family: the spec's `credential` reason and the rail's concrete
// credential_unavailable.
func connectorCredentialReason(reason string) bool {
	return strings.HasPrefix(reason, "credential")
}

// ConnectorAppState derives the D6 app-facing state from the rail state and
// reason, the user toggle and the tools' read-only flags. Precedence:
//
//  1. credential reason            -> needs-re-auth (the A7 latch parks the
//     rail in disabled/credential_unavailable, so this must outrank the
//     disabled bucket)
//  2. user toggle off              -> disabled
//  3. state ready                  -> read-only if every tool is read-only
//     hinted (vacuously true with no tools), else read+act
//  4. anything else (starting, backoff, name_occupied, pending approval)
//     -> not-connected
func ConnectorAppState(state, reason string, enabled bool, tools []ConnectorTool) string {
	switch {
	case connectorCredentialReason(reason):
		return AppStateNeedsReauth
	case !enabled:
		return AppStateDisabled
	case state == railStateReady:
		for _, t := range tools {
			if !t.ReadOnly {
				return AppStateReadAct
			}
		}
		return AppStateReadOnly
	default:
		return AppStateNotConnected
	}
}

// connectorMeta is the surface-side record of what the API committed for a
// connector. The rail status carries neither fingerprint nor target, so the
// surface remembers the sanitized view at commit time. It never holds a
// secret value. Connectors seeded from YAML (or committed before this
// process started) have no entry and show empty fingerprint/target/env.
type connectorMeta struct {
	Fingerprint  string
	URLOrCommand string
	Env          []string
}

func buildConnectorRow(st ConnectorDoorStatus, tools []ConnectorDoorTool, meta connectorMeta) ConnectorCard {
	ct := parseConnectorTools(tools)
	enabled := connectorUserEnabled(st.State, st.Reason)
	hasWrite := false
	for _, t := range ct {
		if !t.ReadOnly {
			hasWrite = true
		}
	}
	env := meta.Env
	if env == nil {
		env = []string{}
	}
	return ConnectorCard{
		Name:          st.Name,
		State:         st.State,
		Tools:         st.Tools,
		Restarts:      st.Restarts,
		Reason:        st.Reason,
		AppState:      ConnectorAppState(st.State, st.Reason, enabled, ct),
		Enabled:       enabled,
		ToolList:      ct,
		HasWriteTools: hasWrite,
		Fingerprint:   meta.Fingerprint,
		LastUsed:      st.LastUsed,
		URLOrCommand:  meta.URLOrCommand,
		Env:           env,
	}
}

// pendingConnectorRow is the phone-rail row for an add awaiting its verdict.
func pendingConnectorRow(name string, meta connectorMeta) ConnectorCard {
	return buildConnectorRow(ConnectorDoorStatus{
		Name: name, State: connStatePending, Reason: connReasonPendingAppvl,
	}, nil, meta)
}

// ConnectorDoor is the surface's view of the custosd connector plane,
// implemented over the door client in cmd/hearthd (surface never imports
// custody). Status/tools are the door client's MIRROR (no wire round-trip);
// the verbs ride doors.sock. Verb errors are mapped by the adapter onto
// ErrConnectorDoorDown, ErrConnectorDaemonTooOld, ErrLocked and
// *ConnectorReasonError.
type ConnectorDoor interface {
	ConnectorStatuses() []ConnectorDoorStatus
	ConnectorTools(name string) []ConnectorDoorTool
	// SubscribeConnectors invokes poke (non-blocking, any goroutine) after
	// every pushed MCP-STATUS / MCP-TOOLS broadcast; the caller re-reads
	// the mirror. cancel is idempotent.
	SubscribeConnectors(poke func()) (cancel func())

	Probe(ctx context.Context, configJSON []byte) (ConnectorProbe, error)
	Add(ctx context.Context, name string, configJSON []byte) error
	Remove(ctx context.Context, name string) error
	SetState(ctx context.Context, name string, enabled bool) error
	Refresh(ctx context.Context, name string) error
}

// ConnectorProbe is a successful MCP-PROBE outcome.
type ConnectorProbe struct {
	Fingerprint string
	Tools       []ConnectorDoorTool
}

// connectorLife is the server-side state of the connector lifecycle: the
// door, the surface-side committed-shape record, in-memory pending
// add/edit gates and the rail poke registry. Zero value is ready to use.
type connectorLife struct {
	mu      sync.Mutex
	door    ConnectorDoor
	meta    map[string]connectorMeta
	pending map[string]*pendingConnector
	pokes   map[int]func()
	pokeSeq int
	hold    int64 // card hold in nanoseconds; 0 = default
}

type pendingConnector struct {
	name   string
	edit   bool
	cardID string
	meta   connectorMeta
}

func (s *Server) getConnectorDoor() ConnectorDoor {
	s.connLife.mu.Lock()
	defer s.connLife.mu.Unlock()
	return s.connLife.door
}

// SetConnectorDoor attaches the connector control plane; nil-safe and safe
// for concurrent use. With none attached the mutating routes and the event
// rail answer 503 and GET /v1/mcp/connectors keeps its legacy source.
func (s *Server) SetConnectorDoor(d ConnectorDoor) {
	s.connLife.mu.Lock()
	s.connLife.door = d
	s.connLife.mu.Unlock()
	s.pokeConnectorRail()
}

// connectorRows builds the full inventory: the door mirror (or the legacy
// MCPConnectorsSource when no door is wired) plus pending adds. Sorted by name.
func (s *Server) connectorRows() []ConnectorCard {
	rows := []ConnectorCard{}
	seen := map[string]bool{}

	s.connLife.mu.Lock()
	door := s.connLife.door
	meta := make(map[string]connectorMeta, len(s.connLife.meta))
	for k, v := range s.connLife.meta {
		meta[k] = v
	}
	pend := make([]*pendingConnector, 0, len(s.connLife.pending))
	for _, p := range s.connLife.pending {
		cp := *p
		pend = append(pend, &cp)
	}
	s.connLife.mu.Unlock()

	if door != nil {
		for _, st := range door.ConnectorStatuses() {
			rows = append(rows, buildConnectorRow(st, door.ConnectorTools(st.Name), meta[st.Name]))
			seen[st.Name] = true
		}
	} else {
		s.custosMu.RLock()
		reg := s.custosReg
		s.custosMu.RUnlock()
		if src, ok := reg.(MCPConnectorsSource); ok {
			for _, c := range src.ConnectorSnapshot() {
				rows = append(rows, buildConnectorRow(ConnectorDoorStatus{
					Name: c.Name, State: c.State, Reason: c.Reason, Tools: c.Tools, Restarts: c.Restarts,
				}, nil, meta[c.Name]))
				seen[c.Name] = true
			}
		}
	}
	for _, p := range pend {
		if !p.edit && !seen[p.name] {
			rows = append(rows, pendingConnectorRow(p.name, p.meta))
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

func (s *Server) connectorRow(name string) (ConnectorCard, bool) {
	for _, r := range s.connectorRows() {
		if r.Name == name {
			return r, true
		}
	}
	return ConnectorCard{}, false
}

// registerConnectorPoke adds a rail poke listener (SSE streams).
func (s *Server) registerConnectorPoke(f func()) (cancel func()) {
	s.connLife.mu.Lock()
	if s.connLife.pokes == nil {
		s.connLife.pokes = make(map[int]func())
	}
	s.connLife.pokeSeq++
	id := s.connLife.pokeSeq
	s.connLife.pokes[id] = f
	s.connLife.mu.Unlock()
	return func() {
		s.connLife.mu.Lock()
		delete(s.connLife.pokes, id)
		s.connLife.mu.Unlock()
	}
}

// pokeConnectorRail tells every SSE stream to re-diff (pending-gate changes
// are surface-local and have no door broadcast).
func (s *Server) pokeConnectorRail() {
	s.connLife.mu.Lock()
	fs := make([]func(), 0, len(s.connLife.pokes))
	for _, f := range s.connLife.pokes {
		fs = append(fs, f)
	}
	s.connLife.mu.Unlock()
	for _, f := range fs {
		f()
	}
}

func rowJSON(r ConnectorCard) ([]byte, error) { return json.Marshal(r) }
