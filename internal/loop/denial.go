package loop

import (
	"encoding/json"
	"strings"
)

// Gate identifiers understood by denialGuidance. They are derived from the
// frozen approval reason enum (SURFACE-SPEC §6 / CUSTOS-SPEC) plus the
// engine's own "no-approver" case.
const (
	gateNoApprover = "no-approver"
	gateDenied     = "denied"
	gateTimeout    = "timeout"
	gateNoChannel  = "disconnected"
	gateCustos     = "custos_locked"
)

const retryInvite = " If the user allows it, run the same call again."

// denialGuidance returns the text the assistant receives when a tool call
// is blocked. It names the gate, says what allowing it would enable, says
// how the user can allow it (strings taken from docs/INSTALL-GUIDE.md and
// the approval card buttons) and invites a retry: blocked is not impossible.
func denialGuidance(gate, tool string) string {
	var b strings.Builder
	switch gate {
	case gateTimeout:
		b.WriteString("Approval for tool " + tool + " timed out: nobody answered the approval card " +
			"within serve.approval_timeout (default 5m), so it was NOT explicitly denied.")
		b.WriteString(" Allowing it would let " + tool + " run.")
		b.WriteString(" Ask the user to tap Allow on the approval card (web UI or phone) when it appears, " +
			"or to widen serve.approval_timeout in lararium.yaml.")
	case gateNoApprover:
		b.WriteString("Blocked by the approval policy: tool " + tool + " requires approval and no approver is wired.")
		b.WriteString(" Allowing it would let " + tool + " run.")
		b.WriteString(" The user can connect an approver (open the web UI or pair the phone app) " +
			"so an approval card can be shown.")
	case gateNoChannel:
		b.WriteString("Blocked by the approval policy: tool " + tool + " needs approval but no approver channel " +
			"(web UI or phone) was connected to answer it.")
		b.WriteString(" Allowing it would let " + tool + " run.")
		b.WriteString(" The user can open the web UI or the phone app, then tap Allow on the approval card.")
	case gateCustos:
		b.WriteString("Blocked at the custos door: credential custody is locked or the credential was revoked, " +
			"so tool " + tool + " could not get approval.")
		b.WriteString(" Allowing it would let " + tool + " use its credential.")
		b.WriteString(" The user can run `custos -config ./lararium.yaml unlock`, then tap Allow once/Allow on the card.")
	default:
		b.WriteString("Blocked by the approval policy: the user denied tool " + tool + " on its approval card.")
		b.WriteString(" Allowing it would let " + tool + " run.")
		b.WriteString(" The user can allow it by tapping Allow on the approval card (web UI or phone); " +
			"for a connector they can stop the prompts with `custos -config ./lararium.yaml mcp trust <name>`.")
	}
	b.WriteString(retryInvite)
	b.WriteString(" Do not give up: explain this to the user and offer to retry.")
	return b.String()
}

// denialGate maps an engine approval string plus the optional audit object
// (Session.AuditApproval) onto a denialGuidance gate identifier.
func denialGate(approval string, audit func(string) (json.RawMessage, bool)) string {
	if approval == "denied:"+gateNoApprover {
		return gateNoApprover
	}
	if audit == nil {
		return gateDenied
	}
	obj, ok := audit(approval)
	if !ok {
		return gateDenied
	}
	var f struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(obj, &f); err != nil {
		return gateDenied
	}
	switch f.Reason {
	case "timeout", "timed_out":
		return gateTimeout
	case "disconnected", "undeliverable":
		return gateNoChannel
	case "custos_locked", "credential_revoked":
		return gateCustos
	}
	return gateDenied
}
