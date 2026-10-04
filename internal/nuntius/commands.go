package nuntius

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// jsonUnmarshal wraps json.Unmarshal so bridge.go maintains zero
// encoding/json imports (§4).
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// id8 returns the first 8 characters after the s_ prefix of a session
// ULID body; main and short ids pass through; empty returns main (§5).
func id8(id string) string {
	if id == "" || id == "main" {
		return "main"
	}
	body := strings.TrimPrefix(id, "s_")
	if len(body) <= 8 {
		return body
	}
	return body[:8]
}

// commandArgs extracts the argument portion following the leading
// /command[@BotName] in text, trimmed of leading/trailing space.
func commandArgs(text string) string {
	loc := commandNameRE.FindStringIndex(strings.TrimSpace(text))
	if loc == nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimSpace(text)[loc[1]:])
}

// matchSession reports whether a candidate session id matches a user-supplied
// session reference: main matches main exactly; any other session matches by
// unique prefix of the full id or ULID body (§5).
func matchSession(id, arg string) bool {
	if id == "main" {
		return arg == "main"
	}
	if arg == "main" {
		return false
	}
	if strings.HasPrefix(id, arg) {
		return true
	}
	idBody := strings.TrimPrefix(id, "s_")
	argBody := strings.TrimPrefix(arg, "s_")
	return strings.HasPrefix(idBody, argBody)
}

// handleCommand dispatches slash commands from the owner (§5).
func (b *Bridge) handleCommand(ctx context.Context, m *Message, rec Record) {
	switch CommandName(m.Text) {
	case "start":
		b.replyText(ctx, m.Chat.ID, MsgGreeting)
		b.markDone(rec, nil)

	case "pair":
		b.replyText(ctx, m.Chat.ID, MsgAlreadyPaired)
		b.markDone(rec, nil)

	case "new":
		b.cmdNew(ctx, m, rec)

	case "sessions":
		b.cmdSessions(ctx, m, rec)

	case "status":
		b.cmdStatus(ctx, m, rec)

	case "cancel":
		active := b.activeSession()
		if b.deps.Wire.CancelTurn == nil || !b.deps.Wire.CancelTurn(active) {
			b.replyText(ctx, m.Chat.ID, MsgNothingRunning)
			b.markDone(rec, nil)
			return
		}
		b.queuesMu.Lock()
		q := b.queues[m.Chat.ID]
		b.queuesMu.Unlock()
		reply := MsgCancelled
		if q != nil && q.queued() > 0 {
			reply = MsgCancelledQueued
		}
		b.replyText(ctx, m.Chat.ID, reply)
		b.markDone(rec, &active)

	case "help":
		b.replyText(ctx, m.Chat.ID, MsgHelp)
		b.markDone(rec, nil)

	default:
		b.log.Printf(LogUnknownCommand, CommandName(m.Text), rec.UpdateID)
		b.replyText(ctx, m.Chat.ID, MsgUnknownCommand)
		b.markDone(rec, nil)
	}
}

// cmdNew creates and selects a new session (§5).
func (b *Bridge) cmdNew(ctx context.Context, m *Message, rec Record) {
	active := b.activeSession()
	if b.deps.Wire.InFlight != nil && b.deps.Wire.InFlight(active) {
		b.replyText(ctx, m.Chat.ID, MsgTurnRunning)
		b.markDone(rec, &active)
		return
	}
	args := commandArgs(m.Text)
	if b.deps.Sess == nil {
		b.replyText(ctx, m.Chat.ID, MsgNoSessions)
		b.markDone(rec, nil)
		return
	}
	ref, err := b.deps.Sess.Create(strings.TrimSpace(args), "")
	if err != nil {
		b.log.Printf("nuntius: session create failed: %v", err)
		b.replyText(ctx, m.Chat.ID, MsgNoSessions)
		b.markDone(rec, nil)
		return
	}
	if err := b.setActiveSession(ref.ID); err != nil {
		b.log.Printf("nuntius: set active session failed: %v", err)
	}
	b.replyText(ctx, m.Chat.ID, "session "+id8(ref.ID)+" created")
	b.markDone(rec, &ref.ID)
}

// cmdSessions lists sessions or switches the active session (§5).
func (b *Bridge) cmdSessions(ctx context.Context, m *Message, rec Record) {
	args := commandArgs(m.Text)
	if args == "" {
		if b.deps.Sess == nil {
			b.replyText(ctx, m.Chat.ID, MsgNoSessions)
			b.markDone(rec, nil)
			return
		}
		refs, err := b.deps.Sess.List()
		if err != nil || len(refs) == 0 {
			b.replyText(ctx, m.Chat.ID, MsgNoSessions)
			b.markDone(rec, nil)
			return
		}
		// Reverse the store order: up to 20 newest-first (§5).
		n := len(refs)
		reversed := make([]SessionRef, n)
		for i, r := range refs {
			reversed[n-1-i] = r
		}
		if len(reversed) > 20 {
			reversed = reversed[:20]
		}
		active := b.activeSession()
		lines := make([]string, 0, len(reversed))
		for _, r := range reversed {
			display := id8(r.ID)
			if r.Title != "" {
				display += " " + r.Title
			}
			if r.ID == active {
				display = "▶ " + display
			}
			lines = append(lines, display)
		}
		b.replyText(ctx, m.Chat.ID, strings.Join(lines, "\n"))
		b.markDone(rec, nil)
		return
	}

	// With arg: turn gate as for /new.
	active := b.activeSession()
	if b.deps.Wire.InFlight != nil && b.deps.Wire.InFlight(active) {
		b.replyText(ctx, m.Chat.ID, MsgTurnRunning)
		b.markDone(rec, &active)
		return
	}
	if b.deps.Sess == nil {
		b.replyText(ctx, m.Chat.ID, MsgUnknownRef)
		b.markDone(rec, nil)
		return
	}
	refs, err := b.deps.Sess.List()
	if err != nil {
		b.replyText(ctx, m.Chat.ID, MsgUnknownRef)
		b.markDone(rec, nil)
		return
	}
	hasMain := false
	for _, r := range refs {
		if r.ID == "main" {
			hasMain = true
			break
		}
	}
	pool := refs
	if !hasMain {
		pool = append([]SessionRef{{ID: "main"}}, refs...)
	}
	var matches []SessionRef
	for _, r := range pool {
		if matchSession(r.ID, args) {
			matches = append(matches, r)
		}
	}
	switch {
	case len(matches) == 0:
		b.replyText(ctx, m.Chat.ID, MsgUnknownRef)
		b.markDone(rec, nil)
	case len(matches) > 1:
		b.replyText(ctx, m.Chat.ID, MsgAmbiguousRef)
		b.markDone(rec, nil)
	default:
		target := matches[0]
		if err := b.setActiveSession(target.ID); err != nil {
			b.log.Printf("nuntius: set active session failed: %v", err)
		}
		reply := "active " + id8(target.ID)
		if target.Title != "" {
			reply += " " + target.Title
		}
		b.replyText(ctx, m.Chat.ID, reply)
		b.markDone(rec, &target.ID)
	}
}

// cmdStatus reports the status of the active session (§5).
func (b *Bridge) cmdStatus(ctx context.Context, m *Message, rec Record) {
	active := b.activeSession()
	var lines []string
	lines = append(lines, "session: "+id8(active))
	if b.deps.Wire.ModelRef != nil {
		if mref := b.deps.Wire.ModelRef(); mref != "" {
			lines = append(lines, "model: "+mref)
		}
	}
	if b.deps.Wire.InFlight != nil {
		inflight := "no"
		if b.deps.Wire.InFlight(active) {
			inflight = "yes"
		}
		lines = append(lines, "in flight: "+inflight)
	}
	if b.deps.Wire.PendingApprovals != nil {
		lines = append(lines, fmt.Sprintf("pending approvals: %d", b.deps.Wire.PendingApprovals(active)))
	}
	inTok, outTok, ok := b.lastTurnUsage(active)
	if ok {
		lines = append(lines, fmt.Sprintf("last turn: %d in / %d out", inTok, outTok))
	} else {
		lines = append(lines, "last turn: none")
	}
	b.replyText(ctx, m.Chat.ID, strings.Join(lines, "\n"))
	b.markDone(rec, nil)
}
