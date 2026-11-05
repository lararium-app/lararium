package web

import (
	"strings"
)

const retryInvite = " If the user allows it, run the same call again."

// denialGuidance mirrors internal/loop/denial.go frozen approval guidance text
// byte-for-byte for tool "web_browse".
func denialGuidance(gate string) string {
	var b strings.Builder
	const tool = "web_browse"
	switch gate {
	case "timeout":
		b.WriteString("Approval for tool " + tool + " timed out: nobody answered the approval card " +
			"within serve.approval_timeout (default 5m), so it was NOT explicitly denied.")
		b.WriteString(" Allowing it would let " + tool + " run.")
		b.WriteString(" Ask the user to tap Allow on the approval card (web UI or phone) when it appears, " +
			"or to widen serve.approval_timeout in lararium.yaml.")
	case "no-approver":
		b.WriteString("Blocked by the approval policy: tool " + tool + " requires approval and no approver is wired.")
		b.WriteString(" Allowing it would let " + tool + " run.")
		b.WriteString(" The user can connect an approver (open the web UI or pair the phone app) " +
			"so an approval card can be shown.")
	case "disconnected":
		b.WriteString("Blocked by the approval policy: tool " + tool + " needs approval but no approver channel " +
			"(web UI or phone) was connected to answer it.")
		b.WriteString(" Allowing it would let " + tool + " run.")
		b.WriteString(" The user can open the web UI or the phone app, then tap Allow on the approval card.")
	case "custos_locked":
		b.WriteString("Blocked at the custos door: credential custody is locked or the credential was revoked, " +
			"so tool " + tool + " could not get approval.")
		b.WriteString(" Allowing it would let " + tool + " use its credential.")
		b.WriteString(" The user can run `custos -config ./lararium.yaml unlock`, then tap Allow once/Allow on the card.")
	default:
		b.WriteString("Blocked by the approval policy: the user denied tool " + tool + " on its approval card.")
		b.WriteString(" Allowing it would let " + tool + " run.")
		b.WriteString(" The user can allow it by tapping Allow on the approval card (web UI or phone); " +
			"for a connector they can stop the prompts with `custos -config ./lararium.yaml mcp trust " + tool + "`.")
	}
	b.WriteString(retryInvite)
	b.WriteString(" Do not give up: explain this to the user and offer to retry.")
	return b.String()
}
