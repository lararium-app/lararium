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
	// Hold is how long an `ask` waits on the approval card (populated
	// from cfg.AskHoldTimeout, default 330 s per CUSTOS-SPEC §7;
	// zero falls back to the connector/worker config or mcpAskHoldDefault).
	Hold time.Duration
	// Workers attaches the worker server park manager so ask cards
	// surface on CardSnapshot and DoorServer (A-3).
	Workers *WorkerServer
	// Host is the connector's target authority for card/park attribution.
	Host string
	// Review is the caller/schema-derived review summary for ask cards.
	Review string
	// OnParked is invoked with the minted card ID after card registration
	// and before blocking on the decision channel (A-4).
	OnParked func(cardID string)
}

// NewMCPCallGuard builds an MCPCallGuard populated from cfg.AskHoldTimeout
// (CUSTOS-SPEC §7 hold doctrine). When cfg is nil, Hold falls back to
// mcpAskHoldDefault.
func NewMCPCallGuard(cfg *Config, vault *Vault, hub *surface.ApprovalHub, actor string) MCPCallGuard {
	hold := mcpAskHoldDefault
	if cfg != nil {
		if cfg.AskHoldTimeout > 0 {
			hold = cfg.AskHoldTimeout
		} else {
			c := *cfg
			c.Normalize()
			hold = c.AskHoldTimeout
		}
	}
	return MCPCallGuard{
		Vault: vault,
		Hub:   hub,
		Actor: actor,
		Hold:  hold,
	}
}

// mcpAskHoldDefault is a last-resort fallback for the approval wait when
// no Config or WorkerServer hold value exists; the hold doctrine is
// cfg.AskHoldTimeout (default 330 s per CUSTOS-SPEC §7).
const mcpAskHoldDefault = 60 * time.Second

// FormatMCPParkedFrame renders the single-line interim parked notification
// frame ("PARKED <id>\n") emitted on ctl.sock before blocking on an
// ask-gated MCP-CALL approval card (A-4).
func FormatMCPParkedFrame(cardID string) string {
	return "PARKED " + oneLineReason(cardID) + "\n"
}

func (g MCPCallGuard) effectiveHold() time.Duration {
	if g.Hold > 0 {
		return g.Hold
	}
	if g.Workers != nil && g.Workers.cfg != nil && g.Workers.cfg.AskHoldTimeout > 0 {
		return g.Workers.cfg.AskHoldTimeout
	}
	return mcpAskHoldDefault
}

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
		if g.Hub == nil && g.Vault != nil {
			g.Hub = g.Vault.ApprovalHub()
		}
		if g.Hub == nil {
			g.auditDenied(connector, tool, "approval_required")
			return verdictError{"approval_required"}
		}
		hold := g.effectiveHold()
		review := g.Review
		if review == "" {
			review = "mcp-call"
		}
		host := g.Host
		if host == "" {
			host = connector
		}
		id, decision, unpark := g.Workers.registerMCPPark(g.Hub, g.Actor, connector, host, tool, review, hold)
		defer unpark()
		if g.OnParked != nil {
			g.OnParked(id)
		}
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
	trimmedName := strings.TrimSpace(name)
	trimmedTool := strings.TrimSpace(tool)
	if guard.Workers == nil {
		guard.Workers = c.workersServer()
	}
	if guard.Hold <= 0 {
		if h := c.AskHoldTimeout(); h > 0 {
			guard.Hold = h
		}
	}
	if guard.Hub == nil && guard.Vault != nil {
		guard.Hub = guard.Vault.ApprovalHub()
	}
	if guard.Host == "" {
		guard.Host = c.Host()
	}
	if guard.Review == "" {
		if rev := c.ReviewFields(trimmedTool, json.RawMessage(args)); rev != "" {
			guard.Review = rev
		} else {
			guard.Review = "mcp-call"
		}
	}
	if guard.OnParked == nil && conn != nil && writeFn == nil {
		guard.OnParked = func(cardID string) {
			write(FormatMCPParkedFrame(cardID))
		}
	}

	execTimeout := c.ExecTimeout()
	hold := guard.effectiveHold()
	if conn != nil {
		_ = conn.SetDeadline(time.Now().Add(workerConnDeadline(hold, execTimeout)))
	}

	// H2 gate: lock check + worker-lane policy + audit, before any
	// execution (mirrors the worker lane; every socket caller
	// inherits it through this dispatch).
	if err := guard.gate(trimmedName, trimmedTool); err != nil {
		if writeFn != nil && conn != nil {
			_ = conn.SetDeadline(time.Time{})
		}
		write("ERR " + oneLineReason(err.Error()) + "\n")
		return true
	}
	guard.auditAllowed(trimmedName, trimmedTool)
	// Execution deadline: worker exec timeout + 30 s slack (§7 budget anchor).
	timeout := execTimeout + 30*time.Second
	if conn != nil {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result, err := c.Execute(ctx, trimmedTool, json.RawMessage(args), Credential{})
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
