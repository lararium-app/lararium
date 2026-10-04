package nuntius

import (
	"regexp"
	"strings"
)

// Frozen static lines (spec §3/§5 — byte-exact, asserted in tests).
const (
	// MsgPairingRequired is the unpaired-state reply to everything
	// except /pair (rate-limited per user per pair_reply_gap).
	MsgPairingRequired = "pairing required"
	// MsgInvalidCode is the generic wrong/expired-code reply (no
	// hints why, §3).
	MsgInvalidCode = "invalid code"
	// MsgPaired is the successful-pairing reply (static, no code
	// material, §3).
	MsgPaired = "paired — this bot is now yours"
	// MsgAlreadyPaired is the owner's /pair once paired (§3).
	MsgAlreadyPaired = "already paired"
	// MsgPrivate is the non-owner refusal, once per user per 5 min
	// (§3).
	MsgPrivate = "this bot is private"
	// MsgUnknownCommand is the unknown-command refusal (§5).
	MsgUnknownCommand = "unknown command"
	// MsgMediaUnsupported is the inbound-media refusal (§11).
	MsgMediaUnsupported = "media not supported in v1"
	// LogPairedOwner is the pairing log line template (no code
	// material, §3).
	LogPairedOwner = "nuntius: paired owner %s"
)

// pairCommandRE matches `/pair <code>` (bare or @BotName form). The
// code itself is validated by the store, not here.
var pairCommandRE = regexp.MustCompile(`^/pair(?:@[A-Za-z0-9_]+)?[ \t]+(\S+)[ \t]*$`)

// commandNameRE extracts the command name from a leading /command
// (bare or @BotName form).
var commandNameRE = regexp.MustCompile(`^/([A-Za-z0-9_]+)(?:@[A-Za-z0-9_]+)?`)

// IsPairCommand reports whether text is a /pair <code> message.
func IsPairCommand(text string) bool {
	return pairCommandRE.MatchString(text)
}

// PairCode extracts the code from a /pair command (empty if not one).
func PairCode(text string) string {
	m := pairCommandRE.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return m[1]
}

// CommandName returns the lowercase command name of a leading
// /command, or "" if the text is not a command.
func CommandName(text string) string {
	m := commandNameRE.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

// Decision is the gate's verdict for one inbound update (spec §3: the
// gate runs on the envelope only — update id, from.id, chat.type —
// before any semantic handling, queueing, or model call).
type Decision int

const (
	// Drop acknowledges and ignores: zero replies, zero calls (group
	// updates, including the owner's own group messages).
	Drop Decision = iota
	// Admit lets the owner's private update through to commands/turns.
	Admit
	// PairAttempt is the unpaired state's private /pair <code>:
	// evaluate the code.
	PairAttempt
	// ReplyPairingRequired is the unpaired state's answer to anything
	// else — static line (message) or toast (callback), rate-limited.
	ReplyPairingRequired
	// ReplyPrivate is the non-owner private refusal — static refusal
	// (message) or toast-only (callback), rate-limited.
	ReplyPrivate
)

// Envelope is the minimal parse the gate sees (§3).
type Envelope struct {
	UpdateID   int64
	IsCallback bool
	FromID     string
	ChatType   string // "private" | "group" | "supergroup" | "channel"
	Text       string // message text or callback data
}

// Decide applies the §3 gate. owners is the state re-read from disk
// this poll cycle (freshness doctrine — no cache, no signals).
func Decide(env Envelope, owners Owners) Decision {
	if env.ChatType != "private" {
		// Group updates never pass, even from the owner (N2).
		return Drop
	}
	if len(owners.OwnerIDs) == 0 {
		// While unpaired the ONLY accepted input is /pair <code>
		// from any private chat; everything else is the rate-
		// limited pairing-required line (toast for callbacks).
		// An update with no sender is malformed: it never reaches
		// the code path (an empty id must never bind as owner).
		if !env.IsCallback && env.FromID != "" && IsPairCommand(env.Text) {
			return PairAttempt
		}
		return ReplyPairingRequired
	}
	if IsOwner(owners.OwnerIDs, env.FromID) {
		return Admit
	}
	return ReplyPrivate
}

// IsOwner reports whether id is in the owner allowlist.
func IsOwner(ownerIDs []string, id string) bool {
	for _, o := range ownerIDs {
		if o == id {
			return true
		}
	}
	return false
}
