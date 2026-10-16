package custos

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

// DispatchMCPVerb executes the shared MCP verb logic (STATUS, TOOLS, REFRESH)
// per MCP-SPEC §9 and CUSTOS-SPEC §6.4c for both ctl.sock and doors.sock.
//
// Arguments:
//   - conn: connection for setting deadlines and fallback writes (may be nil in tests)
//   - reg: the live RailRegistry
//   - cmd: command verb (MCP-STATUS, STATUS, MCP-TOOLS, TOOLS, MCP-REFRESH, REFRESH)
//   - name: connector argument (if any)
//   - hasArg: whether an argument was provided
//   - writeFn: optional write callback; if nil, writes directly to conn
func DispatchMCPVerb(conn net.Conn, reg *RailRegistry, cmd string, name string, hasArg bool, writeFn func([]byte) error) bool {
	write := func(s string) bool {
		data := []byte(s)
		if writeFn != nil {
			_ = writeFn(data)
			return true
		}
		if conn != nil {
			_, _ = conn.Write(data)
			return true
		}
		return true
	}

	switch cmd {
	case "MCP-STATUS", "STATUS":
		list := []ConnectorStatus{}
		if reg != nil {
			for _, c := range reg.Connectors() {
				list = append(list, c.Status())
			}
		}
		b, err := json.Marshal(list)
		if err != nil {
			return write(fmt.Sprintf("ERR %s\n", err.Error()))
		}
		return write(fmt.Sprintf("OK %s\n", string(b)))

	case "MCP-TOOLS", "TOOLS":
		if !hasArg || strings.TrimSpace(name) == "" {
			return write("ERR bad_request\n")
		}
		trimmedName := strings.TrimSpace(name)
		if reg == nil {
			return write(fmt.Sprintf("ERR connector %q not found\n", trimmedName))
		}
		c := reg.Connector(trimmedName)
		if c == nil {
			return write(fmt.Sprintf("ERR connector %q not found\n", trimmedName))
		}
		tools := c.ToolStatuses()
		if tools == nil {
			tools = []ToolStatus{}
		}
		b, err := json.Marshal(tools)
		if err != nil {
			return write(fmt.Sprintf("ERR %s\n", err.Error()))
		}
		return write(fmt.Sprintf("OK %s\n", string(b)))

	case "MCP-REFRESH", "REFRESH":
		if !hasArg || strings.TrimSpace(name) == "" {
			return write("ERR bad_request\n")
		}
		trimmedName := strings.TrimSpace(name)
		if reg == nil {
			return write(fmt.Sprintf("ERR connector %q not found\n", trimmedName))
		}
		c := reg.Connector(trimmedName)
		if c == nil {
			return write(fmt.Sprintf("ERR connector %q not found\n", trimmedName))
		}

		// Reply deadline: connector's list_timeout + 5 s grace (A3)
		timeout := c.cfg.ListTimeout
		if timeout <= 0 {
			timeout = DefaultMCPListTimeout
		}
		replyDeadline := timeout + 5*time.Second
		if conn != nil {
			_ = conn.SetDeadline(time.Now().Add(replyDeadline))
		}

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		if err := c.ManualRefresh(ctx); err != nil {
			return write(fmt.Sprintf("ERR %s\n", err.Error()))
		}
		return write("OK refreshed\n")

	default:
		return false
	}
}
