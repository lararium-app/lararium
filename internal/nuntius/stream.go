package nuntius

import (
	"context"
	"strings"
	"time"
)

// Stream renders one turn's deltas into Telegram messages per §6:
// first delta sends, edits only after edit_interval, cut at sentence
// boundaries, roll to a new message past the cap, final edit always.
// Owned by the supervisor goroutine; not for concurrent use.
//
// Invariants (the whole design rests on these):
//   - full holds the turn's text so far; it never shrinks (a shorter
//     Delta is ignored — you cannot unsend).
//   - finalized holds verbatim slices covering exactly full[:liveStart].
//   - When a live message exists (msgID != 0): it shows exactly
//     full[liveStart:rendered] (visible is that text), and
//     rendered-liveStart ≤ msgCap.
//   - When no live message exists: rendered == liveStart ==
//     rune length of the finalized chain.
//   - rendered advances ONLY when text actually reached Telegram (a
//     successful edit or send), so failed renders are retried, never
//     skipped.
//   - After the terminal call (TurnDone/TurnAborted) the stream is
//     done: every later call is a no-op.
type Stream struct {
	t        Transport
	chatID   string
	interval time.Duration
	now      func() time.Time

	full      []rune
	finalized []string
	liveStart int   // runes finalized before the live message
	rendered  int   // runes of full actually visible across messages
	msgID     int64 // live message id (0 = no live message)
	visible   string
	lastEdit  time.Time
	done      bool

	editFails int
	editDown  bool // 3 consecutive edit failures: stop editing (§6)
	fellBack  bool // fresh-send fallback already used
}

// NewStream starts a stream for one turn in chatID.
func NewStream(t Transport, chatID string, interval time.Duration, now func() time.Time) *Stream {
	if now == nil {
		now = time.Now
	}
	return &Stream{t: t, chatID: chatID, interval: interval, now: now}
}

// Delta feeds the assistant text so far (the full text to date, not an
// increment). Sends the first message, then edits on the interval at
// a sentence cut. Transport errors are counted, never returned: a
// degraded Telegram must not kill the turn — the final render is the
// delivery guarantee (§6).
func (s *Stream) Delta(ctx context.Context, textSoFar string) {
	if s.done {
		return
	}
	r := []rune(textSoFar)
	if len(r) < s.rendered {
		return // text never shrinks; nothing to unsend
	}
	if len(r) == len(s.full) && s.msgID != 0 {
		return // unchanged text: no edit
	}
	s.full = r

	if s.msgID == 0 {
		if len(s.full) <= s.rendered {
			return // nothing new to show
		}
		// No live message: start one (first delta) or resume from the
		// finalized frontier (a previous send failed). Never re-sends
		// text already visible.
		cut := CutPoint(s.rendered, s.full)
		if cut > s.liveStart+msgCap {
			cut = s.liveStart + msgCap
		}
		if cut <= s.rendered {
			return
		}
		s.roll(ctx, string(s.full[s.liveStart:cut]))
		if s.msgID != 0 {
			s.rendered = cut
			s.lastEdit = s.now()
		}
		return
	}
	if s.now().Sub(s.lastEdit) < s.interval {
		return // interval not elapsed: never early
	}
	s.tryEdit(ctx)
	s.lastEdit = s.now()
}

// tryEdit renders the next cut into the live message, rolling to a
// fresh message when the cut crosses the cap. rendered advances only
// on successful renders; a failed edit stops the pass (the next delta
// or the final render retries from the same frontier).
func (s *Stream) tryEdit(ctx context.Context) {
	cut := CutPoint(s.rendered, s.full)
	if cut > len(s.full) {
		cut = len(s.full)
	}
	for s.rendered < cut {
		room := msgCap - (s.rendered - s.liveStart)
		if room <= 0 {
			s.finalize()
			next := cut
			if next-s.liveStart > msgCap {
				next = s.liveStart + msgCap
			}
			s.roll(ctx, string(s.full[s.liveStart:next]))
			if s.msgID == 0 {
				return // send failed; resume at next delta/final
			}
			s.rendered = next
			continue
		}
		target := cut
		if target > s.liveStart+room {
			target = s.liveStart + room
		}
		if !s.edit(ctx, string(s.full[s.liveStart:target])) {
			return // edit failed (or went down): retry from here later
		}
		s.rendered = target
	}
}

// edit sends one EditMessageText, tracking the 3-failure rule (§6).
// It reports whether the text is now visible on Telegram.
func (s *Stream) edit(ctx context.Context, text string) bool {
	if s.msgID == 0 {
		return false
	}
	if text == s.visible {
		return true // already showing exactly this
	}
	if s.editDown {
		return false
	}
	if err := s.t.EditMessageText(ctx, s.chatID, s.msgID, text, nil); err != nil && ctx.Err() == nil {
		s.editFails++
		if s.editFails >= 3 {
			s.editDown = true
		}
		return false
	}
	s.editFails = 0
	s.visible = text
	return true
}

// roll starts a fresh live message (the previous one, if any, must be
// finalized first).
func (s *Stream) roll(ctx context.Context, text string) {
	msg, err := s.t.SendMessage(ctx, s.chatID, text, nil)
	if err != nil && ctx.Err() == nil {
		return // msgID stays 0; the frontier did not move
	}
	if msg != nil {
		s.msgID = msg.MessageID
	}
	s.visible = text
}

// finalize moves the live message's visible text into the finalized
// chain and clears the live message. By the invariants visible is
// exactly full[liveStart:rendered], so the chain stays verbatim.
func (s *Stream) finalize() {
	if s.msgID != 0 {
		s.finalized = append(s.finalized, s.visible)
		s.liveStart = s.rendered
	}
	s.msgID = 0
	s.visible = ""
}

// TurnDone completes the stream with the full text: final edit
// always (even inside the interval), cap rolls as needed, ▼done on
// the last message when more than one was used (§6). Idempotent.
func (s *Stream) TurnDone(ctx context.Context, fullText string) {
	if s.done {
		return
	}
	r := []rune(fullText)
	if len(r) >= s.rendered {
		s.full = r
	}
	s.finish(ctx, "")
}

// TurnAborted ends the stream on error: one final rendering of the
// textSoFar (the partial as streamed, never shrinking what was seen)
// appending "— aborted: <reason>" (§6). Nothing is deleted; the
// partial stays as a rendering artifact. Idempotent.
func (s *Stream) TurnAborted(ctx context.Context, textSoFar, reason string) {
	if s.done {
		return
	}
	r := []rune(textSoFar)
	if len(r) > s.rendered && len(r) > len(s.full) {
		s.full = r
	}
	s.finish(ctx, abortedSuffix+reason)
}

// finish is the terminal pass shared by TurnDone/TurnAborted.
func (s *Stream) finish(ctx context.Context, suffix string) {
	s.done = true

	// If edits died, everything from the frontier on goes out as one
	// fresh ▼(retried) send of the remaining text (§6: a fresh send is
	// attempted even after edit failures, and even for aborted turns).
	if s.editDown {
		rest := s.full[s.liveStart:]
		if s.msgID != 0 {
			s.finalize()
		}
		s.freshFallback(ctx, string(rest), suffix)
		return
	}

	for len(s.full)-s.liveStart > msgCap {
		if s.msgID != 0 {
			// Fill the live message to the cap and finalize it.
			if s.edit(ctx, string(s.full[s.liveStart:s.liveStart+msgCap])) {
				s.rendered = s.liveStart + msgCap
			}
			s.finalize()
		}
		// Start the next live message at the frontier (unchanged when
		// the previous roll failed, so no text is skipped).
		next := s.liveStart + msgCap
		if next > len(s.full) {
			next = len(s.full)
		}
		s.roll(ctx, string(s.full[s.liveStart:next]))
		if s.msgID == 0 {
			s.freshFallback(ctx, string(s.full[s.liveStart:]), suffix)
			return
		}
		s.rendered = next
		suffix = "" // suffix rides only the very last piece
	}

	remaining := string(s.full[s.liveStart:])
	if suffix != "" {
		remaining = joinWithSpace(remaining, suffix)
	}
	if len(s.finalized) > 0 {
		remaining = joinWithSpace(remaining, doneMarker)
	}
	if s.msgID == 0 {
		if remaining != "" {
			s.roll(ctx, remaining)
			if s.msgID != 0 {
				s.rendered = len(s.full)
				s.finalize()
			}
		}
		return
	}
	s.edit(ctx, remaining)
}

// freshFallback sends the remaining text as fresh messages prefixed
// ▼(retried) after edits went down (§6 final-render rule).
func (s *Stream) freshFallback(ctx context.Context, text, suffix string) {
	if s.fellBack {
		return
	}
	s.fellBack = true
	if suffix != "" {
		text = joinWithSpace(text, suffix)
	}
	text = retriedPrefix + "\n" + text
	for _, p := range splitMessage(text) {
		s.roll(ctx, p)
		if s.msgID != 0 {
			s.finalize()
		}
	}
}

// joinWithSpace appends suffix with exactly one space, trimming
// trailing whitespace off the base (§6: verbatim slices — the marker
// is the only addition, and only on the final edit).
func joinWithSpace(base, suffix string) string {
	base = strings.TrimRight(base, " \t\n")
	if base == "" {
		return suffix
	}
	return base + " " + suffix
}

// MessagesUsed reports how many messages the stream consumed.
func (s *Stream) MessagesUsed() int {
	n := len(s.finalized)
	if s.msgID != 0 {
		n++
	}
	return n
}
