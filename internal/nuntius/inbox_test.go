package nuntius

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTestInbox(t *testing.T) (string, *Inbox) {
	t.Helper()
	dir := t.TempDir()
	in, err := OpenInbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close() })
	return dir, in
}

// V11(a) at the inbox level: an appended record survives a reopen and
// replays as not-done; the offset advanced exactly when the record was
// durably in the inbox.
func TestInboxAppendReplay(t *testing.T) {
	dir, in := openTestInbox(t)

	off, err := in.Append(Record{UpdateID: 10, Kind: KindTurn, Payload: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if off != 10 {
		t.Fatalf("offset %d, want 10", off)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen (simulated restart): the record replays not-done.
	in2, err := OpenInbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer in2.Close()
	if in2.LastSeenUpdateID() != 10 {
		t.Fatalf("reopened offset %d", in2.LastSeenUpdateID())
	}
	recs, err := in2.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].UpdateID != 10 || recs[0].Payload != "hello" {
		t.Fatalf("replay: %+v", recs)
	}
}

// V11(b): a done tombstone removes the record from replay.
func TestInboxDoneTombstone(t *testing.T) {
	dir, in := openTestInbox(t)

	if _, err := in.Append(Record{UpdateID: 5, Kind: KindTurn, Payload: "x"}); err != nil {
		t.Fatal(err)
	}
	sess := "s_01ABC"
	if err := in.MarkDone(5, &sess); err != nil {
		t.Fatal(err)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}

	in2, err := OpenInbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer in2.Close()
	recs, err := in2.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("done record replayed: %+v", recs)
	}
	// Tombstone recorded the session used.
	data, err := os.ReadFile(in2.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), sess) {
		t.Fatalf("tombstone lacks session: %s", data)
	}
}

// Sessionless terminal outcomes tombstone with null session_id (§2).
func TestInboxSessionlessTombstone(t *testing.T) {
	_, in := openTestInbox(t)
	if _, err := in.Append(Record{UpdateID: 3, Kind: KindCommand, Payload: "/status"}); err != nil {
		t.Fatal(err)
	}
	if err := in.MarkDone(3, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(in.path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var tomb Record
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &tomb); err != nil {
		t.Fatal(err)
	}
	if !tomb.Done || tomb.SessionID != nil {
		t.Fatalf("tombstone: %+v", tomb)
	}
}

// V11(d) window: a duplicate append (Telegram redelivery after a
// pre-ack crash) does NOT double the record, and the offset still
// advances — the dedupe case must not strand the watermark (§2).
func TestInboxDedupeAdvancesOffset(t *testing.T) {
	_, in := openTestInbox(t)

	if _, err := in.Append(Record{UpdateID: 8, Kind: KindTurn, Payload: "one"}); err != nil {
		t.Fatal(err)
	}
	// Redelivered duplicate with a HIGHER id in between: 8 again then 9.
	off, err := in.Append(Record{UpdateID: 8, Kind: KindTurn, Payload: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if off != 8 {
		t.Fatalf("dup offset %d", off)
	}
	if _, err := in.Append(Record{UpdateID: 9, Kind: KindTurn, Payload: "two"}); err != nil {
		t.Fatal(err)
	}
	// Duplicate of the newest: offset must stay at 9, not regress.
	off, err = in.Append(Record{UpdateID: 9, Kind: KindTurn, Payload: "two"})
	if err != nil || off != 9 {
		t.Fatalf("dup newest: off=%d err=%v", off, err)
	}

	data, err := os.ReadFile(in.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), `"update_id":8`); got != 1 {
		t.Fatalf("update 8 recorded %d times, want 1", got)
	}
}

// N3/V2: a /pair payload is sanitized at append — the plaintext code
// never touches inbox.jsonl.
func TestInboxSanitizesPairCode(t *testing.T) {
	dir, in := openTestInbox(t)

	code := "pair1_ABCDEFGHIJKLMNOPQRST"
	if _, err := in.Append(Record{UpdateID: 1, Kind: KindCommand, Payload: "/pair " + code}); err != nil {
		t.Fatal(err)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}

	// Grep-style: no file in the dir contains the plaintext code.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), code) {
			t.Fatalf("plaintext code found in %s", e.Name())
		}
	}

	in2, err := OpenInbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer in2.Close()
	recs, err := in2.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Payload != "/pair <redacted>" {
		t.Fatalf("stored payload: %+v", recs)
	}
}

// Replay preserves inbox ORDER (a /sessions switch must re-apply
// before the message that followed it, §2 step 4).
func TestInboxReplayOrder(t *testing.T) {
	_, in := openTestInbox(t)
	for i, p := range []string{"/sessions s_01", "hello", "/status"} {
		if _, err := in.Append(Record{UpdateID: int64(100 + i), Kind: KindCommand, Payload: p}); err != nil {
			t.Fatal(err)
		}
	}
	if err := in.MarkDone(100, nil); err != nil {
		t.Fatal(err)
	}
	recs, err := in.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Payload != "hello" || recs[1].Payload != "/status" {
		t.Fatalf("replay order: %+v", recs)
	}
}

// Compaction: past 1 MiB the file keeps only not-done records.
func TestInboxCompaction(t *testing.T) {
	_, in := openTestInbox(t)

	// Pad past the threshold with done records.
	filler := strings.Repeat("x", 4096)
	n := (compactionThreshold / 4200) + 5
	for i := range n {
		id := int64(1000 + i)
		if _, err := in.Append(Record{UpdateID: id, Kind: KindTurn, Payload: filler}); err != nil {
			t.Fatal(err)
		}
		if err := in.MarkDone(id, nil); err != nil {
			t.Fatal(err)
		}
	}
	// One live record must survive.
	if _, err := in.Append(Record{UpdateID: 99999, Kind: KindTurn, Payload: "survivor"}); err != nil {
		t.Fatal(err)
	}
	// Trigger compaction with a legitimate tombstone (an appended id).
	if _, err := in.Append(Record{UpdateID: 99998, Kind: KindTurn, Payload: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := in.MarkDone(99998, nil); err != nil {
		t.Fatal(err)
	}

	st, err := os.Stat(in.path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > compactionThreshold/2 {
		t.Fatalf("inbox not compacted: %d bytes", st.Size())
	}
	recs, err := in.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Payload != "survivor" {
		t.Fatalf("after compaction: %+v", recs)
	}
	// The append handle still works post-compaction.
	if _, err := in.Append(Record{UpdateID: 100000, Kind: KindTurn, Payload: "post"}); err != nil {
		t.Fatal(err)
	}
}

// N8: a corrupt inbox line refuses to open.
func TestInboxCorruptRefuses(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, inboxFileName), []byte("{oops\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenInbox(dir)
	if !errors.Is(err, ErrCorruptState) {
		t.Fatalf("corrupt inbox: %v", err)
	}
	if !strings.Contains(err.Error(), RemedyN8) {
		t.Fatalf("missing remedy: %v", err)
	}
}

// Inbox file mode is 0600 (§2).
func TestInboxFileMode(t *testing.T) {
	_, in := openTestInbox(t)
	if _, err := in.Append(Record{UpdateID: 1, Kind: KindTurn, Payload: "m"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(in.path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("inbox mode %v", st.Mode().Perm())
	}
}
