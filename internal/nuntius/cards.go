package nuntius

import (
	"context"
	"strings"
	"time"
	"unicode"
)

// Hub fan-out legs (Wire.SetFanout): the card lifecycle per §7. Card
// sends and outcome edits are synchronous on the caller's goroutine —
// fan-out is already off the hub lock, and per-approval causality
// (pending before terminal) must hold, so nothing here may be
// fire-and-forget.

// approvalPending delivers the inline-keyboard card for a new pending
// approval (§7.2) to the paired owner's chat, retrying a fixed number
// of times before declaring the card undeliverable (§7.6).
func (b *Bridge) approvalPending(id, sessionID, name, argsSummary string) {
	st, err := b.state.Load()
	if err != nil || st.OwnerChat == "" {
		// No destination exists: fail fast, hub keeps any web card.
		b.log.Printf("nuntius: approval card %s undeliverable: no owner chat", id)
		if b.deps.Wire.MarkUndeliverable != nil {
			b.deps.Wire.MarkUndeliverable(id)
		}
		return
	}
	chatID := st.OwnerChat

	body := "Approve " + name + " for " + id8(sessionID) + "?"
	if argsSummary != "" {
		body += "\n" + truncateArgs(argsSummary, 700)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var lastErr error
	for attempt := range cardAttempts {
		if attempt > 0 {
			if err := b.deps.sleep(ctx, cardBackoff); err != nil {
				lastErr = err
				break
			}
		}
		msg, err := b.tg.SendMessage(ctx, chatID, body, ApprovalKeyboard(id))
		if err == nil {
			b.cardsMu.Lock()
			b.cards[id] = &card{chatID: chatID, msgID: msg.MessageID}
			b.cardsMu.Unlock()
			return
		}
		lastErr = err
	}
	b.log.Printf("nuntius: card send failed for approval %s: %v", id, lastErr)
	if b.deps.Wire.MarkUndeliverable != nil {
		b.deps.Wire.MarkUndeliverable(id)
	}
}

// approvalTerminal edits a delivered card to its outcome and strips
// the keyboard (§7.5: one edit, one retry, then log; stale cards are
// cosmetic, the hub keeps rejecting late taps). Called on every
// terminal transition regardless of which surface caused it.
func (b *Bridge) approvalTerminal(id, sessionID, state, reason, source string) {
	if reason == "undeliverable" {
		return // §7.6: the card was never delivered — nothing to edit
	}

	b.cardsMu.Lock()
	c, ok := b.cards[id]
	delete(b.cards, id)
	b.cardsMu.Unlock()

	if !ok {
		return // no card of ours: web-only approval (or already cleaned)
	}

	outcome := outcomeText(reason, source)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := b.tg.EditMessageText(ctx, c.chatID, c.msgID, outcome, nil); err != nil {
		if err2 := b.tg.EditMessageText(ctx, c.chatID, c.msgID, outcome, nil); err2 != nil {
			b.log.Printf(LogCardStale, id)
		}
	}
}

// outcomeText maps a settled hub reason to the frozen outcome string
// (§7.5, byte-exact). Inputs are the exact reason values
// ApprovalHub.settle emits: ok, denied, timed_out, cancelled,
// shutdown, disconnected.
func outcomeText(reason, source string) string {
	switch reason {
	case "ok":
		return "Allowed ✓ (" + source + ")"
	case "timed_out":
		return "Denied — timed out"
	case "cancelled":
		return "Denied — cancelled"
	case "shutdown":
		return "Denied — shutdown"
	case "disconnected":
		return "Denied ✗ (web)"
	default: // reason "denied": explicit verdict, source attributes it
		if source == "" {
			source = "web"
		}
		return "Denied ✗ (" + source + ")"
	}
}

// handleCallback evaluates an inline-keyboard tap (§7.2, N5): parse
// the callback_data, resolve through the hub (first tap wins, N5),
// and ALWAYS toast so no client spinner hangs (N10).
func (b *Bridge) handleCallback(ctx context.Context, u Update, rec Record) {
	if strings.HasPrefix(rec.Payload, "cu:") {
		b.handleCustodyCallback(ctx, u, rec)
		return
	}
	id, verdict, ok := parseApprovalData(rec.Payload)
	if !ok {
		b.toast(ctx, callbackIDOf(u), ToastNotPending)
		b.log.Printf(LogForeignCallback, rec.Payload, rec.UpdateID)
		b.markDone(rec, nil)
		return
	}
	if b.deps.Wire.SessionOf == nil || b.deps.Wire.ResolveApproval == nil {
		b.toast(ctx, callbackIDOf(u), ToastNotPending)
		b.log.Printf(LogForeignCallback, id, rec.UpdateID)
		b.markDone(rec, nil)
		return
	}
	sessionID, known := b.deps.Wire.SessionOf(id)
	if !known {
		b.toast(ctx, callbackIDOf(u), ToastNotPending)
		b.log.Printf(LogForeignCallback, id, rec.UpdateID)
		b.markDone(rec, nil)
		return
	}
	switch b.deps.Wire.ResolveApproval(sessionID, id, verdict == "a") {
	case 200:
		if verdict == "a" {
			b.toast(ctx, callbackIDOf(u), ToastAllowed)
		} else {
			b.toast(ctx, callbackIDOf(u), ToastDenied)
		}
	case 410: // resolved by the other surface first: honest toast, no state change
		b.toast(ctx, callbackIDOf(u), ToastAlready)
	default: // 404: unknown id — same class as a foreign callback
		b.toast(ctx, callbackIDOf(u), ToastNotPending)
		b.log.Printf(LogForeignCallback, id, rec.UpdateID)
	}
	b.markDone(rec, &sessionID)
}

// parseApprovalData decodes callback_data of the form ap:<id>:<a|d>
// (§7.2). Anything else — including malformed or foreign keyboards —
// is rejected so it can never reach the hub.
func parseApprovalData(data string) (id, verdict string, ok bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "ap" || !approvalIDRe.MatchString(parts[1]) {
		return "", "", false
	}
	if parts[2] != "a" && parts[2] != "d" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// truncateArgs cuts s AT the last whitespace boundary <= limit, never
// mid-token (§7.2); with no boundary in range it hard-cuts.
func truncateArgs(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	sub := s[:limit]
	if idx := strings.LastIndexFunc(sub, unicode.IsSpace); idx >= 0 {
		return sub[:idx]
	}
	return sub
}

// envelopeFromRecord reconstructs the routing envelope of a persisted
// update for replay, from the record and the owner identity (§2 step 4).
func envelopeFromRecord(rec Record, ownerID, ownerChat string) Envelope {
	return rebuildUpdate(rec, ownerID, ownerChat).Envelope()
}

// rebuildUpdate reconstructs a minimal Update from an inbox record so
// replay runs the same code path as a live delivery (§2 step 4). The
// callback id is gone (long answered), which is exactly why the toast
// on replay is a no-op.
func rebuildUpdate(rec Record, ownerID, ownerChat string) Update {
	if rec.Kind == KindCallback {
		return Update{
			UpdateID: rec.UpdateID,
			CallbackQuery: &CallbackQuery{
				From: User{ID: ownerID},
				Message: Message{
					Chat: Chat{ID: ownerChat, Type: "private"},
				},
				Data: rec.Payload,
			},
		}
	}
	return Update{
		UpdateID: rec.UpdateID,
		Message: &Message{
			From: &User{ID: ownerID},
			Chat: Chat{ID: ownerChat, Type: "private"},
			Text: rec.Payload,
		},
	}
}
