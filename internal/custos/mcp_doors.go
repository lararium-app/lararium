package custos

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// WithRail attaches the RailRegistry to the door server.
func WithRail(r *RailRegistry) DoorServerOption {
	return func(s *DoorServer) {
		s.SetRailRegistry(r)
	}
}

// SetRailRegistry attaches the RailRegistry to the door server and wires the change listener.
func (s *DoorServer) SetRailRegistry(r *RailRegistry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rail = r
	if r != nil {
		r.SetChangeListener(func(connectorName string, toolsChanged bool) {
			s.BroadcastMCPStatus()
			if toolsChanged {
				s.BroadcastMCPTools(connectorName)
			}
		})
	}
}

func (s *DoorServer) railRegistry() *RailRegistry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.rail != nil {
		return s.rail
	}
	if s.workers != nil {
		return s.workers.RailRegistry()
	}
	return nil
}

// sendMCPSnapshot transmits the MCP status (and tools for ready rails) on attach per issue #83.
func (s *DoorServer) sendMCPSnapshot(client *doorClient) {
	reg := s.railRegistry()
	if reg == nil || len(reg.Connectors()) == 0 {
		return
	}

	// 1. MCP-STATUS snapshot for all configured connectors
	list := []ConnectorStatus{}
	for _, c := range reg.Connectors() {
		list = append(list, c.Status())
	}
	b, err := json.Marshal(list)
	if err == nil {
		_ = client.writeRaw([]byte("MCP-STATUS " + string(b) + "\n"))
	}

	// 2. MCP-TOOLS for connectors in ready state
	for _, c := range reg.Connectors() {
		if c.Status().State == MCPStateReady {
			tools := c.ToolStatuses()
			if tools == nil {
				tools = []ToolStatus{}
			}
			tb, err := json.Marshal(tools)
			if err == nil {
				_ = client.writeRaw([]byte(fmt.Sprintf("MCP-TOOLS %s %s\n", c.Name(), string(tb))))
			}
		}
	}
}

// BroadcastMCPStatus pushes MCP-STATUS to all attached, authenticated door subscriptions.
// An empty connector list is still broadcast (#96) so subscribers clear stale cards.
func (s *DoorServer) BroadcastMCPStatus() {
	reg := s.railRegistry()
	if reg == nil {
		return
	}
	list := []ConnectorStatus{}
	for _, c := range reg.Connectors() {
		list = append(list, c.Status())
	}
	b, err := json.Marshal(list)
	if err != nil {
		return
	}
	frame := "MCP-STATUS " + string(b) + "\n"
	s.broadcastMCPFrame(frame)
}

// BroadcastMCPTools pushes MCP-TOOLS for connectorName to all attached, authenticated door subscriptions.
func (s *DoorServer) BroadcastMCPTools(connectorName string) {
	reg := s.railRegistry()
	if reg == nil || len(reg.Connectors()) == 0 {
		return
	}
	c := reg.Connector(connectorName)
	if c == nil {
		return
	}
	tools := c.ToolStatuses()
	if tools == nil {
		tools = []ToolStatus{}
	}
	b, err := json.Marshal(tools)
	if err != nil {
		return
	}
	frame := fmt.Sprintf("MCP-TOOLS %s %s\n", connectorName, string(b))
	s.broadcastMCPFrame(frame)
}

func (s *DoorServer) broadcastMCPFrame(frame string) {
	s.mu.RLock()
	clients := s.clientList()
	s.mu.RUnlock()

	ev := doorEvent{
		frame: frame,
	}
	for _, client := range clients {
		client.enqueueEvent(ev)
	}
}

// WithConnectorStore attaches the runtime connector store (the same instance
// the boot merge loaded) so lifecycle verbs persist into it.
func WithConnectorStore(st *ConnectorStore) DoorServerOption {
	return func(s *DoorServer) {
		s.connStore = st
	}
}

// connectorManager returns the lifecycle manager, building it on first use.
func (s *DoorServer) connectorManager() (*ConnectorManager, error) {
	reg := s.railRegistry()
	s.mgrMu.Lock()
	defer s.mgrMu.Unlock()
	if s.mgr != nil && s.mgr.reg == reg {
		return s.mgr, nil
	}
	if s.connStore == nil {
		st, err := OpenConnectorStore(s.stateDir)
		if err != nil {
			return nil, lcErr("store_failed")
		}
		s.connStore = st
	}
	s.mgr = NewConnectorManager(reg, s.connStore, s.vaultInstance(), s.workersInstance(), filepath.Dir(s.stateDir))
	return s.mgr, nil
}

// handleLifecycleCommand serves MCP-ADD / MCP-PROBE / MCP-REMOVE / MCP-STATE
// (MCP-SPEC A10, CONNECTOR-LIFE-SPEC D1). It runs only on authenticated door
// sessions (handleConn rejects bad tokens before the command loop). Every
// reply is exactly one frame per line; the MCP-PROBE success reply carries
// its MCP-TOOLS frame in the same write so the pair is never interleaved with
// broadcasts and is connection-local.
func (s *DoorServer) handleLifecycleCommand(client *doorClient, line string, fields []string) bool {
	verb := fields[0]
	switch verb {
	case "MCP-ADD", "MCP-PROBE", "MCP-REMOVE", "MCP-STATE":
	default:
		return false
	}
	reply := func(msg string) {
		_ = client.writeRaw([]byte(msg + "\n"))
	}
	fail := func(err error) {
		reply("ERR " + oneLineReason(lifecycleReason(err)))
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, verb))

	mgr, err := s.connectorManager()
	if err != nil {
		fail(err)
		return true
	}

	switch verb {
	case "MCP-ADD":
		idx := strings.IndexAny(rest, " \t")
		if rest == "" || idx < 0 {
			reply("ERR bad_request")
			return true
		}
		name, cfgJSON := rest[:idx], strings.TrimSpace(rest[idx:])
		if cfgJSON == "" {
			reply("ERR bad_request")
			return true
		}
		if err := mgr.Add(name, []byte(cfgJSON)); err != nil {
			fail(err)
			return true
		}
		reply("OK")

	case "MCP-PROBE":
		if rest == "" {
			reply("ERR bad_request")
			return true
		}
		res, err := mgr.Probe([]byte(rest))
		if err != nil {
			fail(err)
			return true
		}
		tb, merr := json.Marshal(res.Tools)
		if merr != nil {
			reply("ERR internal")
			return true
		}
		_ = client.writeRaw([]byte(fmt.Sprintf("OK %d %s\nMCP-TOOLS %s %s\n",
			len(res.Tools), res.Fingerprint, probeFrameName, string(tb))))

	case "MCP-REMOVE":
		name := strings.TrimSpace(rest)
		if name == "" || strings.ContainsAny(name, " \t") {
			reply("ERR bad_request")
			return true
		}
		if err := mgr.Remove(name); err != nil {
			fail(err)
			return true
		}
		reply("OK")

	case "MCP-STATE":
		if len(fields) != 3 || (fields[2] != "enabled" && fields[2] != "disabled") {
			reply("ERR bad_request")
			return true
		}
		if err := mgr.SetState(fields[1], fields[2] == "enabled"); err != nil {
			fail(err)
			return true
		}
		reply("OK")
	}
	return true
}

// handleMcpCommand delegates MCP verbs on doors.sock to the shared dispatcher.
func (s *DoorServer) handleMcpCommand(client *doorClient, fields []string) bool {
	cmd := fields[0]
	switch cmd {
	case "MCP-STATUS", "MCP-TOOLS", "MCP-REFRESH":
		var arg string
		hasArg := len(fields) >= 2
		if hasArg {
			arg = fields[1]
		}
		return DispatchMCPVerb(client.conn, s.railRegistry(), cmd, arg, hasArg, client.writeRaw)
	default:
		return false
	}
}
