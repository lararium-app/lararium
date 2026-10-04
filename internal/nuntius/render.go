package nuntius

// Telegram message cap and the spec's working cap (§6: a message is
// capped at 3900 chars).
const (
	// MaxMessageChars is Telegram's hard limit.
	MaxMessageChars = 4096
	// msgCap is the spec §6 working cap: finalize and roll to a new
	// message past this length.
	msgCap = 3900
)

// doneMarker appends to the final edit of the LAST message when a
// turn's reply spanned more than one message (§6).
const doneMarker = "▼done"

// abortedSuffix marks a turn that ended in error (§6).
const abortedSuffix = "— aborted: "

// retriedPrefix marks the fresh-send fallback after 3 consecutive
// edit failures (§6 final-render rule).
const retriedPrefix = "▼(retried)"

// CutPoint returns the index in full where the next streaming edit
// should cut, per §6: the latest sentence boundary (.!? + whitespace)
// in the unedited tail full[editedLen:]; else the latest newline; else
// the latest whitespace; else the tail itself. The returned index is
// the number of characters safe to render now (boundary included).
func CutPoint(editedLen int, full []rune) int {
	tail := full[editedLen:]
	if len(tail) == 0 {
		return editedLen
	}
	// Latest sentence boundary: punctuation followed by whitespace
	// (or end of text). Scan backwards over the tail; the cut
	// consumes the whitespace so the next piece starts on a word.
	for i := len(tail) - 1; i >= 0; i-- {
		if isSentencePunct(tail[i]) && (i == len(tail)-1 || isSpaceRune(tail[i+1])) {
			j := i + 1
			for j < len(tail) && isSpaceRune(tail[j]) {
				j++
			}
			return editedLen + j
		}
	}
	// No sentence boundary: latest newline.
	for i := len(tail) - 1; i >= 0; i-- {
		if tail[i] == '\n' {
			return editedLen + i + 1
		}
	}
	// No newline: latest whitespace.
	for i := len(tail) - 1; i >= 0; i-- {
		if isSpaceRune(tail[i]) {
			return editedLen + i + 1
		}
	}
	// Code, base64, one long word: the tail itself.
	return len(full)
}

func isSentencePunct(r rune) bool {
	return r == '.' || r == '!' || r == '?'
}

func isSpaceRune(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
}

// splitMessage breaks one over-cap text into messages of at most
// msgCap chars, splitting at the whitespace boundary nearest the cap;
// with no whitespace in the last msgCap chars it hard-splits at the
// cap (mid-token beats a dropped message, §6). Every output piece is
// a verbatim slice of s — no inserted characters, ever.
func splitMessage(s string) []string {
	r := []rune(s)
	if len(r) <= msgCap {
		if len(r) == 0 {
			return nil
		}
		return []string{s}
	}
	var out []string
	for len(r) > msgCap {
		cut := nearestWhitespaceSplit(r)
		out = append(out, string(r[:cut]))
		r = r[cut:]
	}
	if len(r) > 0 {
		out = append(out, string(r))
	}
	return out
}

// nearestWhitespaceSplit picks the cut index ≤ msgCap nearest the cap:
// after the last whitespace at-or-before the cap (whitespace stays
// with the earlier slice so slices concatenate back to the source), or
// the cap itself when the window has no whitespace.
func nearestWhitespaceSplit(r []rune) int {
	for i := msgCap; i > 0; i-- {
		if isSpaceRune(r[i-1]) {
			return i
		}
	}
	return msgCap
}
