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
// Invariants: full holds the turn's text so far; finalized holds
// verbatim slices covering full[:liveStart]; the live message shows
// full[liveStart:rendered] (visible is its last successfully sent
// text); rendered - liveStart ≤ msgCap always.
type Stream struct {
	t        Transport
	chatID   string
	interval time.Duration
	now      func() time.Time

	full      []rune
	finalized []string
	liveStart int   // runes finalized before the live message
	rendered  int   // runes of full visible up to and incl. live msg
	msgID     int64 // live message id (0 = no live message)
	visible   string
	lastEdit  time.Time

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
	r := []rune(textSoFar)
	if len(r) == len(s.full) && s.msgID != 0 {
		return // unchanged text: no edit
	}
	s.full = r

	if s.msgID == 0 && len(s.full) > 0 && s.rendered == len(s.joinRunes()) {
		// First delta: initial sendMessage at the first cut (no
		// placeholder text, §6).
		cut := CutPoint(0, s.full)
		s.roll(ctx, string(s.full[:cut]))
		if s.msgID != 0 {
			s.rendered = cut
			s.lastEdit = s.now()
		}
		return
	}
	if s.msgID == 0 || s.now().Sub(s.lastEdit) < s.interval {
		return // nothing live, or interval not elapsed: never early
	}
	s.tryEdit(ctx)
	s.lastEdit = s.now()
}

// joinRunes returns the finalized text as runes.
func (s *Stream) joinRunes() []rune {
	var j []rune
	for _, p := range s.finalized {
		j = append(j, []rune(p)...)
	}
	return j
}

// tryEdit renders the next cut into the live message, rolling to a
// fresh message when the cut crosses the cap.
func (s *Stream) tryEdit(ctx context.Context) {
	cut := CutPoint(s.rendered, s.full)
	if cut <= s.rendered {
		return // no boundary in the unedited tail yet
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
				return // send failed; retry at next delta/final
			}
			s.rendered = next
			continue
		}
		target := cut
		if target > s.liveStart+room {
			target = s.liveStart + room
		}
		s.edit(ctx, string(s.full[s.liveStart:target]))
		s.rendered = target
	}
}

// edit sends one EditMessageText, tracking the 3-failure rule (§6).
func (s *Stream) edit(ctx context.Context, text string) {
	if s.editDown || text == s.visible || s.msgID == 0 {
		return
	}
	if err := s.t.EditMessageText(ctx, s.chatID, s.msgID, text, nil); err != nil && ctx.Err() == nil {
		s.editFails++
		if s.editFails >= 3 {
			s.editDown = true
		}
		return
	}
	s.editFails = 0
	s.visible = text
}

// roll starts a fresh live message (the previous one, if any, must be
// finalized first).
func (s *Stream) roll(ctx context.Context, text string) {
	msg, err := s.t.SendMessage(ctx, s.chatID, text, nil)
	if err != nil && ctx.Err() == nil {
		return // msgID stays 0; final render retries delivery
	}
	if msg != nil {
		s.msgID = msg.MessageID
	}
	s.visible = text
}

// finalize moves the live message's visible text into the finalized
// chain and clears the live message.
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
// the last message when more than one was used (§6).
func (s *Stream) TurnDone(ctx context.Context, fullText string) {
	s.full = []rune(fullText)
	s.renderFinal(ctx, "")
}

// TurnAborted ends the stream on error: one final rendering of the
// text so far appending "— aborted: <reason>" (§6). Nothing is
// deleted; the partial stays as a rendering artifact.
func (s *Stream) TurnAborted(ctx context.Context, reason string) {
	s.renderFinal(ctx, abortedSuffix+reason)
}

// renderFinal drives the last pass.
func (s *Stream) renderFinal(ctx context.Context, suffix string) {
	// If edits died, everything from the live message on goes out as
	// one fresh ▼(retried) send of the remaining text (§6).
	if s.editDown {
		rest := string(s.full[s.liveStart:])
		if s.msgID != 0 {
			s.finalize()
		}
		s.freshFallback(ctx, rest, suffix)
		return
	}

	for {
		remaining := len(s.full) - s.liveStart
		if remaining <= msgCap {
			text := string(s.full[s.liveStart:])
			if suffix != "" {
				text = joinWithSpace(text, suffix)
			}
			if len(s.finalized) > 0 {
				text = joinWithSpace(text, doneMarker)
			}
			if s.msgID == 0 {
				if text != "" {
					s.roll(ctx, text)
					if s.msgID != 0 {
						s.rendered = len(s.full)
						s.finalize()
					}
				}
				return
			}
			s.edit(ctx, text)
			return
		}
		// Over cap: fill the live message to the cap, finalize, roll
		// the next chunk live, and loop.
		cut := s.liveStart + msgCap
		if s.msgID != 0 {
			s.edit(ctx, string(s.full[s.liveStart:cut]))
			s.finalize()
		}
		next := cut + msgCap
		if next > len(s.full) {
			next = len(s.full)
		}
		s.roll(ctx, string(s.full[cut:next]))
		s.liveStart = cut
		s.rendered = next
		suffix = "" // suffix rides only the very last piece
		if s.msgID == 0 {
			// Roll failed: send the rest fresh and stop.
			s.freshFallback(ctx, string(s.full[cut:]), "")
			return
		}
	}
}

// freshFallback sends the remaining text as fresh messages prefixed
// ▼(retried) after edits went down (§6 final-render rule). A fresh
// send is attempted even after edit failures and even for aborted
// turns.
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
