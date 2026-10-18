package custos

import (
	"fmt"
	"net"
	"strings"
)

func (s *CtlServer) handleMcpCmd(conn net.Conn, cmd string, parts []string) bool {
	switch cmd {
	case "MCP-STATUS", "MCP-TOOLS", "MCP-REFRESH":
	case "MCP-CALL":
		// Frame: MCP-CALL <token> <name> <tool> <args-json>
		// (ctl.go pre-splits to 3 parts; args may contain spaces).
		if len(parts) < 3 {
			_, _ = fmt.Fprintf(conn, "ERR bad_request\n")
			return true
		}
		argParts := strings.SplitN(parts[2], " ", 3)
		var tool, args string
		if len(argParts) >= 2 {
			tool = argParts[1]
		}
		if len(argParts) == 3 {
			args = argParts[2]
		}
		return DispatchMCPCall(conn, s.railRegistry(), argParts[0], tool, args, nil)
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
