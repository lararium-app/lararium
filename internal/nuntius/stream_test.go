package nuntius

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeClock drives Stream tests without wall-clock waits.
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

func TestCutPointSentenceBoundary(t *testing.T) {
	full := []rune("First one. Second two. Third")
	// Latest sentence boundary in the whole tail (cut consumes the
	// following whitespace so the next piece starts on a word).
	if got := CutPoint(0, full); got != len("First one. Second two. ") {
		t.Fatalf("sentence cut = %d, want %d", got, len("First one. Second two. "))
	}
	// Only the unedited tail counts.
	if got := CutPoint(len("First one. "), full); got != len("First one. Second two. ") {
		t.Fatalf("tail cut = %d, want %d", got, len("First one. Second two. "))
	}
}

func TestCutPointFallbacks(t *testing.T) {
	// No sentence punctuation: latest newline.
	full := []rune("line one\nline two\nno period here")
	if got := CutPoint(0, full); got != len("line one\nline two\n") {
		t.Fatalf("newline cut = %d, want %d", got, len("line one\nline two\n"))
	}
	// No newline: latest whitespace.
	full = []rune("alpha beta gamma")
	if got := CutPoint(0, full); got != len("alpha beta ") {
		t.Fatalf("space cut = %d, want %d", got, len("alpha beta "))
	}
	// One long word (code/base64): the tail itself.
	full = []rune("abcdef")
	if got := CutPoint(0, full); got != 6 {
		t.Fatalf("word cut = %d, want 6", got)
	}
	// Nothing new: editedLen.
	if got := CutPoint(6, []rune("abcdef")); got != 6 {
		t.Fatalf("empty tail cut = %d, want 6", got)
	}
}

func TestSplitMessageVerbatimSlices(t *testing.T) {
	long := strings.Repeat("word ", msgCap) // > cap, spaces everywhere
	parts := splitMessage(long)
	if len(parts) < 2 {
		t.Fatalf("expected multiple parts, got %d", len(parts))
	}
	joined := strings.Join(parts, "")
	if joined != long {
		t.Fatal("splits must concatenate back to the source verbatim")
	}
	for _, p := range parts {
		if len([]rune(p)) > msgCap {
			t.Fatalf("part exceeds cap: %d runes", len([]rune(p)))
		}
	}
}

func TestSplitMessageHardSplit(t *testing.T) {
	// No whitespace at all: hard split at the cap.
	long := strings.Repeat("x", msgCap*2+10)
	parts := splitMessage(long)
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3", len(parts))
	}
	if strings.Join(parts, "") != long {
		t.Fatal("hard split lost text")
	}
}

func TestSplitMessageShort(t *testing.T) {
	if parts := splitMessage("short"); len(parts) != 1 || parts[0] != "short" {
		t.Fatalf("short text split = %q", parts)
	}
	if parts := splitMessage(""); parts != nil {
		t.Fatalf("empty split = %q, want nil", parts)
	}
}

// --- Stream ---

func TestStreamFirstDeltaSendsImmediately(t *testing.T) {
	f := &Fake{}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	s := NewStream(f, "42", 2*time.Second, clk.now)

	s.Delta(context.Background(), "Hello there. More")
	if len(f.Sent) != 1 {
		t.Fatalf("sends = %d, want 1", len(f.Sent))
	}
	// First send cuts at the sentence boundary.
	if f.Sent[0].Text != "Hello there. " {
		t.Fatalf("first send = %q", f.Sent[0].Text)
	}
	if len(f.Edits) != 0 {
		t.Fatalf("unexpected edits: %v", f.Edits)
	}
}

func TestStreamEditInterval(t *testing.T) {
	f := &Fake{}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	s := NewStream(f, "42", 2*time.Second, clk.now)

	s.Delta(context.Background(), "One. ")
	// Inside the interval: no edit.
	s.Delta(context.Background(), "One. Two. ")
	if len(f.Edits) != 0 {
		t.Fatalf("edit inside interval: %v", f.Edits)
	}
	clk.advance(2 * time.Second)
	s.Delta(context.Background(), "One. Two. Three. ")
	if len(f.Edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(f.Edits))
	}
	if f.Edits[0].Text != "One. Two. Three. " {
		t.Fatalf("edit text = %q", f.Edits[0].Text)
	}
	if f.Edits[0].MessageID != f.Sent[0].MsgID {
		t.Fatal("edit must target the sent message")
	}
}

func TestStreamFinalEditAlways(t *testing.T) {
	f := &Fake{}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	s := NewStream(f, "42", 2*time.Second, clk.now)

	s.Delta(context.Background(), "One. ")
	// No interval elapsed: the final edit still goes out.
	s.TurnDone(context.Background(), "One. Two.")
	if len(f.Edits) != 1 {
		t.Fatalf("final edits = %d, want 1", len(f.Edits))
	}
	if f.Edits[0].Text != "One. Two." {
		t.Fatalf("final text = %q", f.Edits[0].Text)
	}
	// Single message: no done marker.
	if strings.Contains(f.Edits[0].Text, doneMarker) {
		t.Fatal("single-message turn must not carry the done marker")
	}
}

func TestStreamCapRollAndDoneMarker(t *testing.T) {
	f := &Fake{}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	s := NewStream(f, "42", 2*time.Second, clk.now)

	// Build text well past two caps with clean sentence boundaries.
	sentence := strings.Repeat("a", msgCap-1) + ". " // exactly msgCap runes incl. space
	full := strings.Repeat(sentence, 2) + "tail end."
	s.Delta(context.Background(), full[:100])
	s.TurnDone(context.Background(), full)

	if s.MessagesUsed() < 2 {
		t.Fatalf("expected a roll past the cap, messages used = %d", s.MessagesUsed())
	}
	// Every send+edit text must respect the cap.
	all := append(f.SentTexts(), editedTexts(f)...)
	for _, tx := range all {
		if len([]rune(tx)) > msgCap+len(doneMarker)+1 {
			t.Fatalf("render exceeds cap+marker: %d runes", len([]rune(tx)))
		}
	}
	// The last render carries the done marker (multi-message turn).
	last := f.Edits[len(f.Edits)-1].Text
	if !strings.HasSuffix(last, doneMarker) {
		t.Fatalf("last render %q lacks done marker", last)
	}
	// Reassembly: each message's FINAL visible text (its last edit,
	// or its send if never edited) concatenates back to the source.
	visible := map[int64]string{}
	var order []int64
	for _, m := range f.Sent {
		if _, seen := visible[m.MsgID]; !seen {
			order = append(order, m.MsgID)
		}
		visible[m.MsgID] = m.Text
	}
	for _, e := range f.Edits {
		visible[e.MessageID] = e.Text
	}
	var reassembled strings.Builder
	for i, id := range order {
		tx := visible[id]
		if i == len(order)-1 {
			tx = strings.TrimSuffix(strings.TrimSuffix(tx, " "+doneMarker), doneMarker)
		}
		reassembled.WriteString(tx)
	}
	if reassembled.String() != full {
		t.Fatalf("reassembly mismatch:\n got %.120q\nwant %.120q", reassembled.String(), full)
	}
}

func editedTexts(f *Fake) []string {
	out := make([]string, len(f.Edits))
	for i, e := range f.Edits {
		out[i] = e.Text
	}
	return out
}

func TestStreamThreeEditFailuresStopsEditing(t *testing.T) {
	f := &Fake{}
	boom := errors.New("telegram 400")
	f.EditErrs = []error{boom, boom, boom}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	s := NewStream(f, "42", 2*time.Second, clk.now)

	s.Delta(context.Background(), "One. ")
	for i := range 3 {
		clk.advance(2 * time.Second)
		s.Delta(context.Background(), "One. "+strings.Repeat("x", i)+" two.")
	}
	// Edits went down: the final render must fresh-send ▼(retried).
	s.TurnDone(context.Background(), "One. two three. final.")

	var found bool
	for _, tx := range f.SentTexts() {
		if strings.HasPrefix(tx, retriedPrefix) {
			found = true
			if !strings.Contains(tx, "final.") {
				t.Fatalf("fallback send lost the text: %q", tx)
			}
		}
	}
	if !found {
		t.Fatalf("no ▼(retried) fallback among sends: %q", f.SentTexts())
	}
}

func TestStreamAbortedSuffix(t *testing.T) {
	f := &Fake{}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	s := NewStream(f, "42", 2*time.Second, clk.now)

	s.Delta(context.Background(), "Partial outp")
	s.TurnAborted(context.Background(), "model stream error")

	if len(f.Edits) != 1 {
		t.Fatalf("aborted final edits = %d, want 1", len(f.Edits))
	}
	want := "Partial outp " + abortedSuffix + "model stream error"
	if f.Edits[0].Text != want {
		t.Fatalf("aborted render = %q, want %q", f.Edits[0].Text, want)
	}
}

func TestStreamAbortedBeforeFirstSend(t *testing.T) {
	f := &Fake{}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	s := NewStream(f, "42", 2*time.Second, clk.now)

	// Nothing streamed yet; abort with a reason must still tell the
	// user something went wrong.
	s.TurnAborted(context.Background(), "upstream 500")
	if len(f.Sent) != 1 {
		t.Fatalf("sends = %d, want 1 (abort notice fresh-sent)", len(f.Sent))
	}
	if !strings.Contains(f.Sent[0].Text, "upstream 500") {
		t.Fatalf("abort notice = %q", f.Sent[0].Text)
	}
}

func TestStreamEmptyTurnSendsNothing(t *testing.T) {
	f := &Fake{}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	s := NewStream(f, "42", 2*time.Second, clk.now)
	s.TurnDone(context.Background(), "")
	if len(f.Sent) != 0 || len(f.Edits) != 0 {
		t.Fatal("empty turn must not touch Telegram")
	}
}
