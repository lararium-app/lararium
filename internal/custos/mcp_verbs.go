package custos

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lararium-app/lararium/internal/surface"
)

// MCPCallGuard carries the enforcement context the MCP-CALL verb must
// apply on every socket (H2): a vault-lock check, worker-lane policy,
// an approval hold for `ask`, and the audit line. Mirrors the worker
// lane gate (CUSTOS-SPEC §6.1/§8.1) so a door.token holder or ctl
// caller can never reach c.Execute with a locked vault or an
// ask/deny verdict set. The zero guard enforces nothing — test
// harness only.
type MCPCallGuard struct {
	Vault *Vault
	Hub   *surface.ApprovalHub
	// Actor attributes the audit line and the approval card (the door
	// client name, or "ctl").
	Actor string
	// Hold is how long an `ask` waits on the approval card. Bounded
	// well under the door client's 90 s reply wait so a human who does
	// not answer surfaces as a refusal, never a zombie execution after
	// the caller gave up. Zero → 60 s.
	Hold time.Duration
}

// mcpAskHoldDefault bounds the dispatch-layer approval wait (§7 hold
// doctrine, shortened to fit inside the door client reply budget).
const mcpAskHoldDefault = 60 * time.Second

// verdictError is the closed refusal reason surfaced as ERR <reason>.
type verdictError struct{ reason string }

func (v verdictError) Error() string { return v.reason }

// gate evaluates the H2 checks before any tool byte moves. allow=false
// carries the frozen reason for the ERR frame; every refusal is
// audited with the worker-lane audit kinds (§8.1).
func (g MCPCallGuard) gate(connector, tool string) error {
	if g.Vault == nil {
		// Harness path: no vault, nothing to enforce (mirrors the
		// `v != nil &&` convention across the door verbs).
		return nil
	}
	if !g.Vault.IsUnlocked() {
		g.auditDenied(connector, tool, "locked")
		return verdictError{"locked"}
	}
	verdict := VerdictAsk
	if pe := g.Vault.Policy(); pe != nil {
		if v, _ := pe.Decide(DecideInput{
			Lane:       "worker",
			Credential: connector,
			Tool:       tool,
			Host:       connector,
		}); v != "" {
			verdict = v
		}
	}
	switch verdict {
	case VerdictAuto:
		return nil
	case VerdictDeny:
		g.auditDenied(connector, tool, "verdict")
		return verdictError{"denied"}
	default: // ask (also the fail-closed default for missing policy)
		if g.Hub == nil {
			g.auditDenied(connector, tool, "approval_required")
			return verdictError{"approval_required"}
		}
		hold := g.Hold
		if hold <= 0 {
			hold = mcpAskHoldDefault
		}
		_, decision := g.Hub.RegisterCustos(g.Actor, connector,
			connector+"/"+tool, "mcp-call", hold, nil)
		approved, ok := <-decision
		if !ok {
			approved = false
		}
		if !approved {
			g.auditDenied(connector, tool, "approval")
			return verdictError{"denied"}
		}
		// §7 doctrine: policy flipped to deny while the card was held
		// kills the flow before any upstream byte.
		if pe := g.Vault.Policy(); pe != nil {
			if v2, _ := pe.Decide(DecideInput{
				Lane:       "worker",
				Credential: connector,
				Tool:       tool,
				Host:       connector,
			}); v2 == VerdictDeny {
				g.auditDenied(connector, tool, "policy_changed_during_hold")
				return verdictError{"denied"}
			}
		}
		return nil
	}
}

func (g MCPCallGuard) auditDenied(connector, tool, reason string) {
	if g.Vault == nil || g.Vault.Audit() == nil {
		return
	}
	_ = g.Vault.Audit().Append(AuditRecord{
		Kind:    AuditKindWorkerCallDenied,
		Cred:    connector,
		Actor:   g.Actor,
		Host:    connector,
		Tool:    tool,
		Verdict: "deny",
		Reason:  reason,
	})
}

func (g MCPCallGuard) auditAllowed(connector, tool string) {
	if g.Vault == nil || g.Vault.Audit() == nil {
		return
	}
	_ = g.Vault.Audit().Append(AuditRecord{
		Kind:    AuditKindWorkerCallAllowed,
		Cred:    connector,
		Actor:   g.Actor,
		Host:    connector,
		Tool:    tool,
		Verdict: "allow",
	})
}

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
	case "MCP-STATUS":
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

	case "MCP-TOOLS":
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

	case "MCP-REFRESH":
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

		refreshErr := c.ManualRefresh(ctx)
		// doors.sock has no per-command deadline reset (unlike ctl.sock,
		// whose loop re-arms every read); clear the armed reply deadline so
		// the long-lived door connection survives a REFRESH.
		if writeFn != nil && conn != nil {
			_ = conn.SetDeadline(time.Time{})
		}
		if refreshErr != nil {
			return write(fmt.Sprintf("ERR %s\n", refreshErr.Error()))
		}
		return write("OK refreshed\n")

	default:
		return false
	}
}

// DispatchMCPCall executes the MCP-CALL verb per MCP-SPEC §9 (amendment
// A3): look up the connector by name, validate the tool against the
// current snapshot (Execute does this), and write the single-line
// `OK <json>` / `ERR <reason>` frame. Shared by ctl.sock and doors.sock.
// argsJSON is the raw tool arguments object (may be empty → "{}").
func DispatchMCPCall(conn net.Conn, reg *RailRegistry, name, tool, argsJSON string, writeFn func([]byte) error, guard MCPCallGuard) bool {
	write := func(s string) {
		data := []byte(s)
		if writeFn != nil {
			_ = writeFn(data)
			return
		}
		if conn != nil {
			_, _ = conn.Write(data)
		}
	}
	if strings.TrimSpace(name) == "" || strings.TrimSpace(tool) == "" {
		write("ERR bad_request\n")
		return true
	}
	if reg == nil {
		write("ERR no_registry\n")
		return true
	}
	c := reg.Connector(strings.TrimSpace(name))
	if c == nil {
		write(fmt.Sprintf("ERR connector %q not found\n", strings.TrimSpace(name)))
		return true
	}
	args := strings.TrimSpace(argsJSON)
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		write("ERR bad_args\n")
		return true
	}
	// H2 gate: lock check + worker-lane policy + audit, before any
	// execution (mirrors the worker lane; every socket caller
	// inherits it through this dispatch).
	if err := guard.gate(strings.TrimSpace(name), strings.TrimSpace(tool)); err != nil {
		write("ERR " + oneLineReason(err.Error()) + "\n")
		return true
	}
	guard.auditAllowed(strings.TrimSpace(name), strings.TrimSpace(tool))
	// Execution deadline: worker exec timeout + 30 s slack (§7 budget anchor).
	timeout := DefaultWorkerExecTimeout + 30*time.Second
	if c.execTimeout > 0 {
		timeout = c.execTimeout + 30*time.Second
	}
	if conn != nil {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result, err := c.Execute(ctx, strings.TrimSpace(tool), json.RawMessage(args), Credential{})
	// doors.sock has no per-command deadline re-arm; clear the armed
	// deadline so the long-lived door connection survives the call.
	if writeFn != nil && conn != nil {
		_ = conn.SetDeadline(time.Time{})
	}
	if err != nil {
		write(fmt.Sprintf("ERR %s\n", oneLineReason(err.Error())))
		return true
	}
	write(fmt.Sprintf("OK %s\n", string(result)))
	return true
}

// oneLineReason forces an error string into the frozen single-line
// frame contract: Execute reasons can embed MCP-server-supplied text
// (tool errors, validation detail), and an unescaped newline there
// would forge frames on a control channel. Whitespace runs collapse
// and the reason is capped.
func oneLineReason(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const reasonLimit = 300
	if len(s) > reasonLimit {
		cut := reasonLimit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}
