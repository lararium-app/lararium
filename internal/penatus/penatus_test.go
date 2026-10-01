package penatus

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Frontmatter tests ---

func TestParseDoc(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantErr  bool
		errType  error
		wantMeta map[string]string
		wantKeys []string
		wantBody string
	}{
		{
			name:     "basic frontmatter",
			input:    "---\npenatus: 1\nupdated: 2026-09-28T00:00:00Z\n---\nsome body text",
			wantMeta: map[string]string{"penatus": "1", "updated": "2026-09-28T00:00:00Z"},
			wantKeys: []string{"penatus", "updated"},
			wantBody: "some body text",
		},
		{
			name:     "value with colon",
			input:    "---\npenatus: 1\nupdated: 2026-09-28T00:00:00Z\nvoice: be kind: always\n---\nbody",
			wantMeta: map[string]string{"penatus": "1", "updated": "2026-09-28T00:00:00Z", "voice": "be kind: always"},
			wantKeys: []string{"penatus", "updated", "voice"},
			wantBody: "body",
		},
		{
			name:    "no frontmatter",
			input:   "just plain text",
			wantErr: true,
			errType: ErrNoFrontmatter,
		},
		{
			name:    "unclosed frontmatter",
			input:   "---\npenatus: 1\nbody here",
			wantErr: true,
		},
		{
			name:     "empty body",
			input:    "---\npenatus: 1\nupdated: 2026-09-28T00:00:00Z\n---\n",
			wantMeta: map[string]string{"penatus": "1", "updated": "2026-09-28T00:00:00Z"},
			wantKeys: []string{"penatus", "updated"},
			wantBody: "",
		},
		{
			name:     "blank lines in frontmatter",
			input:    "---\npenatus: 1\n\nupdated: 2026-09-28T00:00:00Z\n---\nbody",
			wantMeta: map[string]string{"penatus": "1", "updated": "2026-09-28T00:00:00Z"},
			wantKeys: []string{"penatus", "updated"},
			wantBody: "body",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := ParseDoc([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tt.errType != nil && !errors.Is(err, tt.errType) {
					t.Errorf("got error %v, want %v", err, tt.errType)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			for k, v := range tt.wantMeta {
				if doc.Meta[k] != v {
					t.Errorf("Meta[%q] = %q, want %q", k, doc.Meta[k], v)
				}
			}
			if len(doc.keyOrder) != len(tt.wantKeys) {
				t.Errorf("keyOrder len = %d, want %d", len(doc.keyOrder), len(tt.wantKeys))
			} else {
				for i, k := range tt.wantKeys {
					if doc.keyOrder[i] != k {
						t.Errorf("keyOrder[%d] = %q, want %q", i, doc.keyOrder[i], k)
					}
				}
			}
			if doc.Body != tt.wantBody {
				t.Errorf("Body = %q, want %q", doc.Body, tt.wantBody)
			}
		})
	}
}

func TestDocMarshalRoundTrip(t *testing.T) {
	input := "---\npenatus: 1\nupdated: 2026-09-28T00:00:00Z\nvoice: be kind: always\ntitle: My Soul\n---\n## Values\nBe kind."

	doc, err := ParseDoc([]byte(input))
	if err != nil {
		t.Fatalf("ParseDoc: %v", err)
	}

	out := doc.Marshal()
	doc2, err := ParseDoc(out)
	if err != nil {
		t.Fatalf("ParseDoc round-trip: %v", err)
	}

	for k, v := range doc.Meta {
		if doc2.Meta[k] != v {
			t.Errorf("Meta[%q] = %q, want %q", k, doc2.Meta[k], v)
		}
	}
	if doc2.Body != doc.Body {
		t.Errorf("Body = %q, want %q", doc2.Body, doc.Body)
	}
	if len(doc2.keyOrder) != len(doc.keyOrder) {
		t.Fatalf("keyOrder len = %d, want %d", len(doc2.keyOrder), len(doc.keyOrder))
	}
	for i, k := range doc.keyOrder {
		if doc2.keyOrder[i] != k {
			t.Errorf("keyOrder[%d] = %q, want %q", i, doc2.keyOrder[i], k)
		}
	}
}

func TestRequired(t *testing.T) {
	tests := []struct {
		name    string
		meta    map[string]string
		kind    string
		wantErr bool
	}{
		{
			name:    "valid soul",
			meta:    map[string]string{"penatus": "1", "updated": "2026-09-28T00:00:00Z"},
			kind:    "SOUL.md",
			wantErr: false,
		},
		{
			name:    "missing penatus",
			meta:    map[string]string{"updated": "2026-09-28T00:00:00Z"},
			kind:    "SOUL.md",
			wantErr: true,
		},
		{
			name:    "wrong penatus",
			meta:    map[string]string{"penatus": "2", "updated": "2026-09-28T00:00:00Z"},
			kind:    "SOUL.md",
			wantErr: true,
		},
		{
			name:    "missing updated",
			meta:    map[string]string{"penatus": "1"},
			kind:    "IDENTITY.md",
			wantErr: true,
		},
		{
			name:    "memory-line no updated needed",
			meta:    map[string]string{"penatus": "1"},
			kind:    "memory-line",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := Doc{Meta: tt.meta}
			err := Required(doc, tt.kind)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// --- Memory line tests ---

func TestParseMemoryBody(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []MemLine
	}{
		{
			name:  "standard line with priority",
			input: "- [p:high] Owner's alpha gate = core stable.",
			want:  []MemLine{{Priority: "high", Since: "", Text: "Owner's alpha gate = core stable.", Raw: "- [p:high] Owner's alpha gate = core stable."}},
		},
		{
			name:  "line with since tag",
			input: "- [since:2026-09-28] Some fact.",
			want:  []MemLine{{Priority: "med", Since: "2026-09-28", Text: "Some fact.", Raw: "- [since:2026-09-28] Some fact."}},
		},
		{
			name:  "both tags priority first",
			input: "- [p:high] [since:2026-09-28] Full line.",
			want:  []MemLine{{Priority: "high", Since: "2026-09-28", Text: "Full line.", Raw: "- [p:high] [since:2026-09-28] Full line."}},
		},
		{
			name:  "both tags since first",
			input: "- [since:2026-09-28] [p:low] Reversed order.",
			want:  []MemLine{{Priority: "low", Since: "2026-09-28", Text: "Reversed order.", Raw: "- [since:2026-09-28] [p:low] Reversed order."}},
		},
		{
			name:  "malformed line degrades to med",
			input: "- just some text without tags",
			want:  []MemLine{{Priority: "med", Since: "", Text: "just some text without tags", Raw: "- just some text without tags"}},
		},
		{
			name:  "non-entry lines skipped",
			input: "## Facts\n- [p:high] real entry\n\nsome paragraph",
			want:  []MemLine{{Priority: "high", Since: "", Text: "real entry", Raw: "- [p:high] real entry"}},
		},
		{
			name:  "pointer line",
			input: "- [p:high] → memory/projects/lararium.md",
			want:  []MemLine{{Priority: "high", Since: "", Text: "→ memory/projects/lararium.md", Raw: "- [p:high] → memory/projects/lararium.md"}},
		},
		{
			name:  "empty input",
			input: "",
			want:  []MemLine{},
		},
		{
			name:  "low priority",
			input: "- [p:low] trivial fact",
			want:  []MemLine{{Priority: "low", Since: "", Text: "trivial fact", Raw: "- [p:low] trivial fact"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseMemoryBody(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
			for i, want := range tt.want {
				g := got[i]
				if g.Priority != want.Priority {
					t.Errorf("[%d] Priority = %q, want %q", i, g.Priority, want.Priority)
				}
				if g.Since != want.Since {
					t.Errorf("[%d] Since = %q, want %q", i, g.Since, want.Since)
				}
				if g.Text != want.Text {
					t.Errorf("[%d] Text = %q, want %q", i, g.Text, want.Text)
				}
			}
		})
	}
}

func TestSerializeMemoryBodyIdempotent(t *testing.T) {
	original := "- [p:high] [since:2026-09-28] Fact one.\n- [p:low] Fact two.\n- [p:med] Fact three."
	lines := ParseMemoryBody(original)
	serialized := SerializeMemoryBody(lines)
	lines2 := ParseMemoryBody(serialized)

	if len(lines) != len(lines2) {
		t.Fatalf("len mismatch: %d vs %d", len(lines), len(lines2))
	}
	for i := range lines {
		if lines[i].Priority != lines2[i].Priority {
			t.Errorf("[%d] Priority mismatch after round-trip", i)
		}
		if lines[i].Since != lines2[i].Since {
			t.Errorf("[%d] Since mismatch after round-trip", i)
		}
		if lines[i].Text != lines2[i].Text {
			t.Errorf("[%d] Text mismatch: %q vs %q", i, lines[i].Text, lines2[i].Text)
		}
	}
}

func TestSerializeMemoryBodyOmitsAbsentTags(t *testing.T) {
	lines := []MemLine{
		{Priority: "high", Since: "2026-01-01", Text: "full"},
		{Priority: "med", Since: "", Text: "no since"},
	}
	out := SerializeMemoryBody(lines)
	if !strings.Contains(out, "[p:high] [since:2026-01-01] full") {
		t.Errorf("missing full line: %s", out)
	}
	if !strings.Contains(out, "[p:med] no since") {
		t.Errorf("missing med line: %s", out)
	}
}

// --- Event JSON tests ---

func TestEventMarshalUnmarshalUnknownFields(t *testing.T) {
	data := `{"seq":1,"t":"msg","ts":"2026-01-01T00:00:00Z","role":"user","text":"hello","custom_field":"kept"}`

	var e Event
	if err := json.Unmarshal([]byte(data), &e); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if e.Seq != 1 {
		t.Errorf("Seq = %d, want 1", e.Seq)
	}
	if e.T != "msg" {
		t.Errorf("T = %q, want %q", e.T, "msg")
	}
	if _, ok := e.Fields["custom_field"]; !ok {
		t.Error("unknown field not preserved in Fields")
	}
	if _, ok := e.Fields["role"]; !ok {
		t.Error("role field not in Fields")
	}

	// Marshal back and check unknown field is preserved
	out, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), "custom_field") {
		t.Error("marshaled output missing custom_field")
	}
}

// --- Log tests ---

func TestLogAppendAndReopen(t *testing.T) {
	dir := t.TempDir()

	// Open new log
	log, err := OpenLog(dir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}

	// Append events
	for i := range 3 {
		err := log.Append(Event{T: "msg", Fields: map[string]json.RawMessage{
			"role": json.RawMessage(`"user"`),
			"text": json.RawMessage(`"hello"`),
		}})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	// Reopen and verify
	log2, err := OpenLog(dir)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	if len(log2.events) != 3 {
		t.Fatalf("events len = %d, want 3", len(log2.events))
	}
	for i, e := range log2.events {
		if e.Seq != int64(i+1) {
			t.Errorf("events[%d].Seq = %d, want %d", i, e.Seq, i+1)
		}
	}
}

func TestLogCorruptGap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Write events with a gap: 1, 2, 4 (missing 3)
	lines := []string{
		`{"seq":1,"t":"msg","ts":"2026-01-01T00:00:00Z","role":"user"}`,
		`{"seq":2,"t":"msg","ts":"2026-01-01T00:00:01Z","role":"user"}`,
		`{"seq":4,"t":"msg","ts":"2026-01-01T00:00:03Z","role":"user"}`,
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := OpenLog(dir)
	if err == nil {
		t.Fatal("expected error for gap, got nil")
	}
	var ce CorruptError
	ok := errors.As(err, &ce)
	if !ok {
		t.Fatalf("expected CorruptError, got %T: %v", err, err)
	}
	if ce.Got != 4 || ce.Want != 3 {
		t.Errorf("CorruptError{Got: %d, Want: %d}", ce.Got, ce.Want)
	}
}

func TestLogCorruptDuplicate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	lines := []string{
		`{"seq":1,"t":"msg","ts":"2026-01-01T00:00:00Z"}`,
		`{"seq":1,"t":"msg","ts":"2026-01-01T00:00:01Z"}`,
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := OpenLog(dir)
	if err == nil {
		t.Fatal("expected error for duplicate, got nil")
	}
	var ce CorruptError
	ok := errors.As(err, &ce)
	if !ok {
		t.Fatalf("expected CorruptError, got %T: %v", err, err)
	}
	if ce.Got != 1 || ce.Want != 2 {
		t.Errorf("CorruptError{Got: %d, Want: %d}", ce.Got, ce.Want)
	}
}

func TestLogReadOnlyOnCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	lines := []string{
		`{"seq":1,"t":"msg","ts":"2026-01-01T00:00:00Z"}`,
		`{"seq":3,"t":"msg","ts":"2026-01-01T00:00:02Z"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	log, err := OpenLog(dir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Log should be read-only
	err = log.Append(Event{T: "msg"})
	if err == nil {
		t.Fatal("expected error appending to read-only log")
	}
}

func TestLogTombstoneExcludedFromLive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	lines := []string{
		`{"seq":1,"t":"msg","ts":"2026-01-01T00:00:00Z","role":"user","text":"keep me"}`,
		`{"seq":2,"t":"msg","ts":"2026-01-01T00:00:01Z","role":"assistant","text":"will be deleted"}`,
		`{"seq":3,"t":"msg","ts":"2026-01-01T00:00:02Z","role":"user","text":"also keep"}`,
		`{"seq":4,"t":"tombstone","ts":"2026-01-01T00:00:03Z","seqs":[2]}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	log, err := OpenLog(dir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}

	live := log.Live()
	if len(live) != 3 {
		t.Fatalf("Live() len = %d, want 3 (events 1, 3, 4)", len(live))
	}

	seqs := make([]int64, len(live))
	for i, e := range live {
		seqs[i] = e.Seq
	}

	// Should have seqs 1, 3, 4 (tombstone event itself is not tombstoned, only seq 2)
	expectedSeqs := []int64{1, 3, 4}
	for i, want := range expectedSeqs {
		if seqs[i] != want {
			t.Errorf("Live()[%d].Seq = %d, want %d", i, seqs[i], want)
		}
	}
}

func TestLogCompactionAssembly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Events 1-5 are in the covers range [1,5].
	// Event 6 is the compact event.
	// Event 7 is after compaction.
	lines := []string{
		`{"seq":1,"t":"msg","ts":"2026-01-01T00:00:00Z","role":"user","text":"early"}`,
		`{"seq":2,"t":"msg","ts":"2026-01-01T00:00:01Z","role":"assistant","text":"early response"}`,
		`{"seq":3,"t":"msg","ts":"2026-01-01T00:00:02Z","role":"user","text":"keep this"}`,
		`{"seq":4,"t":"msg","ts":"2026-01-01T00:00:03Z","role":"assistant","text":"more"}`,
		`{"seq":5,"t":"msg","ts":"2026-01-01T00:00:04Z","role":"user","text":"end of range"}`,
		`{"seq":6,"t":"compact","ts":"2026-01-01T00:00:05Z","covers":[1,5],"summary_text":"summary of 1-5","kept":[{"t":"msg","seq":3}]}`,
		`{"seq":7,"t":"msg","ts":"2026-01-01T00:00:06Z","role":"user","text":"after compact"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	log, err := OpenLog(dir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}

	live := log.Live()

	// Should have: seq 3 (kept), seq 6 (compact), seq 7 (after)
	if len(live) != 3 {
		t.Fatalf("Live() len = %d, want 3, seqs: %v", len(live), func() []int64 {
			var s []int64
			for _, e := range live {
				s = append(s, e.Seq)
			}
			return s
		}())
	}

	expectedSeqs := []int64{3, 6, 7}
	for i, want := range expectedSeqs {
		if live[i].Seq != want {
			t.Errorf("Live()[%d].Seq = %d, want %d", i, live[i].Seq, want)
		}
	}
}

func TestLogNestedCompaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// First compaction covers [1,3], then second covers [2,7] which overlaps.
	lines := []string{
		`{"seq":1,"t":"msg","ts":"2026-01-01T00:00:00Z","role":"user","text":"a"}`,
		`{"seq":2,"t":"msg","ts":"2026-01-01T00:00:01Z","role":"user","text":"b"}`,
		`{"seq":3,"t":"msg","ts":"2026-01-01T00:00:02Z","role":"user","text":"c"}`,
		`{"seq":4,"t":"compact","ts":"2026-01-01T00:00:03Z","covers":[1,3],"summary_text":"inner summary"}`,
		`{"seq":5,"t":"msg","ts":"2026-01-01T00:00:04Z","role":"user","text":"d"}`,
		`{"seq":6,"t":"msg","ts":"2026-01-01T00:00:05Z","role":"user","text":"e"}`,
		`{"seq":7,"t":"compact","ts":"2026-01-01T00:00:06Z","covers":[2,6],"summary_text":"outer summary","kept":[{"t":"compact","seq":4}]}`,
		`{"seq":8,"t":"msg","ts":"2026-01-01T00:00:07Z","role":"user","text":"f"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	log, err := OpenLog(dir)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}

	live := log.Live()

	// The inner compact [1,3] covers seqs 1,2,3.
	// The outer compact [2,6] covers seqs 2,3,4,5,6.
	// Seq 4 is kept by outer, so it survives (inner compact is kept).
	// Seq 1 is covered by inner compact (still valid since inner is kept).
	// Expected: 4 (kept by outer), 7 (outer compact itself), 8
	if len(live) != 3 {
		t.Fatalf("Live() len = %d, want 3, seqs: %v", len(live), func() []int64 {
			var s []int64
			for _, e := range live {
				s = append(s, e.Seq)
			}
			return s
		}())
	}

	expectedSeqs := []int64{4, 7, 8}
	for i, want := range expectedSeqs {
		if live[i].Seq != want {
			t.Errorf("Live()[%d].Seq = %d, want %d", i, live[i].Seq, want)
		}
	}
}

// --- Session tests ---

func TestNewID(t *testing.T) {
	id := NewID()
	if !strings.HasPrefix(id, "s_") {
		t.Errorf("ID %q does not start with s_", id)
	}
	if len(id) != 28 { // "s_" (2) + 10 time chars + 16 random chars = 28
		t.Errorf("ID len = %d, want 28", len(id))
	}

	// Check sortability: generate a few and verify they're ordered
	ids := make([]string, 5)
	for i := range ids {
		ids[i] = NewID()
	}
	// They should be roughly sorted (same ms)
	for i := 1; i < len(ids); i++ {
		if ids[i] < ids[i-1] {
			// Not strictly required to be sorted within same ms,
			// but the time prefix should be the same
			if ids[i][:12] != ids[i-1][:12] {
				t.Errorf("IDs not sortable: %s < %s", ids[i], ids[i-1])
			}
		}
	}
}

func TestCreateSession(t *testing.T) {
	root := t.TempDir()

	log, err := CreateSession(root, "test-session-001", "main")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Check session.json exists
	headerPath := filepath.Join(root, "sessions", "test-session-001", "session.json")
	data, err := os.ReadFile(headerPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var header SessionHeader
	if err := json.Unmarshal(data, &header); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if header.Penatus != 1 {
		t.Errorf("Penatus = %d, want 1", header.Penatus)
	}
	if header.ID != "test-session-001" {
		t.Errorf("ID = %q, want %q", header.ID, "test-session-001")
	}
	if header.Kind != "main" {
		t.Errorf("Kind = %q, want %q", header.Kind, "main")
	}
	if header.Title != nil {
		t.Errorf("Title = %v, want nil", header.Title)
	}

	// Check events.jsonl exists
	eventsPath := filepath.Join(root, "sessions", "test-session-001", "events.jsonl")
	if _, err := os.Stat(eventsPath); os.IsNotExist(err) {
		t.Error("events.jsonl not created")
	}

	// Log should be writable
	err = log.Append(Event{T: "msg"})
	if err != nil {
		t.Fatalf("Append to new session: %v", err)
	}
}

func TestCreateSessionInvalidKind(t *testing.T) {
	root := t.TempDir()
	_, err := CreateSession(root, "test", "invalid")
	if err == nil {
		t.Fatal("expected error for invalid kind")
	}
}

func TestCreateSessionSide(t *testing.T) {
	root := t.TempDir()
	log, err := CreateSession(root, "side-001", "side")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	headerPath := filepath.Join(root, "sessions", "side-001", "session.json")
	data, err := os.ReadFile(headerPath)
	if err != nil {
		t.Fatal(err)
	}

	var header SessionHeader
	if err := json.Unmarshal(data, &header); err != nil {
		t.Fatal(err)
	}
	if header.Kind != "side" {
		t.Errorf("Kind = %q, want %q", header.Kind, "side")
	}

	_ = log // log is valid
}
