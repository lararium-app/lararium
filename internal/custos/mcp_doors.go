package custos

import (
	"encoding/json"
	"fmt"
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
func (s *DoorServer) BroadcastMCPStatus() {
	reg := s.railRegistry()
	if reg == nil || len(reg.Connectors()) == 0 {
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
