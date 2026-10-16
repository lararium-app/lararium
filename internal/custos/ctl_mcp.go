package custos

import (
	"net"
)

func (s *CtlServer) handleMcpCmd(conn net.Conn, cmd string, parts []string) bool {
	switch cmd {
	case "MCP-STATUS", "MCP-TOOLS", "MCP-REFRESH":
	default:
		return false
	}
	var name string
	hasArg := len(parts) >= 3
	if hasArg {
		name = parts[2]
	}
	return DispatchMCPVerb(conn, s.railRegistry(), cmd, name, hasArg, nil)
}
