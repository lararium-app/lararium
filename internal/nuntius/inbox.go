package nuntius

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// inboxFileName is <hearth>/nuntius/inbox.jsonl (spec §2): append-only,
// 0600, fsync per batch.
const inboxFileName = "inbox.jsonl"

// compactionThreshold compacts the inbox past 1 MiB (spec §2 step 3).
const compactionThreshold = 1 << 20

// Kind classifies an inbox record.
type Kind string

const (
	// KindTurn is an owner message that becomes a model turn.
	KindTurn Kind = "turn"
	// KindCommand is a slash command (§5).
	KindCommand Kind = "command"
	// KindCallback is an approval-card tap (§7.2).
	KindCallback Kind = "callback"
)

// Record is one inbox.jsonl line (spec §2 step 1). session_id is NOT
// bound at append time — routing resolves at execution, because a batch
// can contain /sessions <ref> followed by a message that must land in
// the new session. The tombstone records the session actually used, or
// null for updates that terminate outside any session.
type Record struct {
	UpdateID  int64   `json:"update_id"`
	Kind      Kind    `json:"kind"`
	Payload   string  `json:"payload"`
	Done      bool    `json:"done"`
	SessionID *string `json:"session_id"`
}

// Inbox is the durable write-ahead inbox (spec §2, N10). Every update
// is fsynced here before its offset advances; startup replays not-done
// entries in inbox order.
type Inbox struct {
	path string
	dir  string

	f      *os.File
	offset int64 // last update durably in the inbox
	seen   map[int64]bool
}

// OpenInbox opens (creating if needed) the inbox in dir and loads the
// seen-set for dedupe. A corrupt line is ErrCorruptState with the N8
// remedy — never a silent reset.
func OpenInbox(dir string) (*Inbox, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("nuntius: create state dir: %w", err)
	}
	path := filepath.Join(dir, inboxFileName)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("nuntius: open inbox: %w", err)
	}

	in := &Inbox{path: path, dir: dir, f: f, seen: map[int64]bool{}}
	if err := in.loadSeen(); err != nil {
		f.Close()
		return nil, err
	}
	return in, nil
}

// loadSeen reads existing records, filling the dedupe set and the
// offset watermark.
func (in *Inbox) loadSeen() error {
	// Re-open for reading from the start.
	rf, err := os.Open(in.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("nuntius: read inbox: %w", err)
	}
	defer rf.Close()

	sc := bufio.NewScanner(rf)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(text), &rec); err != nil {
			return fmt.Errorf("%w: %s:%d: %w — %s", ErrCorruptState, in.path, line, err, RemedyN8)
		}
		in.seen[rec.UpdateID] = true
		if rec.UpdateID > in.offset {
			in.offset = rec.UpdateID
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("nuntius: scan inbox: %w", err)
	}
	return nil
}

// LastSeenUpdateID is the highest update id durably in the inbox — a
// derived hint from surviving lines, NOT the offset to pass to
// getUpdates: compaction prunes done records, so this value can regress
// below what Telegram has already served. The authoritative offset for
// getUpdates is state.json (§2); never pass this to getUpdates.
func (in *Inbox) LastSeenUpdateID() int64 { return in.offset }

// Seen reports whether an update id is already durably in the inbox.
func (in *Inbox) Seen(updateID int64) bool { return in.seen[updateID] }

// Append sanitizes and durably appends one update, returning the new
// offset. Per §2 the offset advances when the update is durably in the
// inbox — freshly appended OR already present (the dedupe case) — never
// only on a fresh append, or a skipped duplicate would strand the
// watermark in a redelivery loop.
func (in *Inbox) Append(rec Record) (int64, error) {
	rec.Payload = SanitizePayload(rec.Payload)
	rec.Done = false
	rec.SessionID = nil

	if !in.seen[rec.UpdateID] {
		if err := in.appendLine(rec); err != nil {
			return in.offset, err
		}
		in.seen[rec.UpdateID] = true
	}
	if rec.UpdateID > in.offset {
		in.offset = rec.UpdateID
	}
	return in.offset, nil
}

// MarkDone appends a tombstone for updateID recording the session used
// (nil for sessionless terminal outcomes, §2). Tombstoning an id that
// was never appended is a caller bug — it is an error, not a silent
// orphan line.
func (in *Inbox) MarkDone(updateID int64, sessionID *string) error {
	if !in.seen[updateID] {
		return fmt.Errorf("nuntius: MarkDone(%d): update never appended to the inbox", updateID)
	}
	if err := in.appendLine(Record{UpdateID: updateID, Done: true, SessionID: sessionID}); err != nil {
		return err
	}
	return in.compactIfNeeded()
}

// appendLine writes one JSON line and fsyncs (spec §2: fsync per batch;
// single-record batches here).
func (in *Inbox) appendLine(rec Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("nuntius: encode inbox record: %w", err)
	}
	if _, err := in.f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("nuntius: append inbox: %w", err)
	}
	if err := in.f.Sync(); err != nil {
		return fmt.Errorf("nuntius: fsync inbox: %w", err)
	}
	return nil
}

// Replay returns the not-done records in inbox order (spec §2 step 4).
// The caller resolves each record's session at execution time from the
// active_session pointer, so a replayed /sessions switch re-applies
// before the messages that followed it.
func (in *Inbox) Replay() ([]Record, error) {
	rf, err := os.Open(in.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("nuntius: read inbox: %w", err)
	}
	defer rf.Close()

	var (
		all   []Record
		sc    = bufio.NewScanner(rf)
		index = map[int64]int{}
	)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(text), &rec); err != nil {
			return nil, fmt.Errorf("%w: %s: %w — %s", ErrCorruptState, in.path, err, RemedyN8)
		}
		if prev, ok := index[rec.UpdateID]; ok {
			all[prev] = rec
			continue
		}
		index[rec.UpdateID] = len(all)
		all = append(all, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("nuntius: scan inbox: %w", err)
	}

	var out []Record
	for _, rec := range all {
		if !rec.Done {
			out = append(out, rec)
		}
	}
	return out, nil
}

// compactIfNeeded rewrites the inbox keeping only not-done records,
// once the file passes the threshold (spec §2 step 3).
func (in *Inbox) compactIfNeeded() error {
	st, err := in.f.Stat()
	if err != nil {
		return fmt.Errorf("nuntius: stat inbox: %w", err)
	}
	if st.Size() <= compactionThreshold {
		return nil
	}

	live, err := in.Replay()
	if err != nil {
		return err
	}

	tmp := in.path + ".compact"
	tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("nuntius: create compact: %w", err)
	}
	for _, rec := range live {
		data, err := json.Marshal(rec)
		if err != nil {
			tf.Close()
			os.Remove(tmp)
			return fmt.Errorf("nuntius: encode compact: %w", err)
		}
		if _, err := tf.Write(append(data, '\n')); err != nil {
			tf.Close()
			os.Remove(tmp)
			return fmt.Errorf("nuntius: write compact: %w", err)
		}
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		os.Remove(tmp)
		return fmt.Errorf("nuntius: fsync compact: %w", err)
	}
	if err := tf.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("nuntius: close compact: %w", err)
	}
	if err := os.Rename(tmp, in.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("nuntius: rename compact: %w", err)
	}

	// Re-open the append handle on the compacted file.
	if err := in.f.Close(); err != nil {
		return fmt.Errorf("nuntius: close inbox: %w", err)
	}
	nf, err := os.OpenFile(in.path, os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("nuntius: reopen inbox: %w", err)
	}
	in.f = nf
	return nil
}

// Close releases the append handle.
func (in *Inbox) Close() error {
	if in.f == nil {
		return nil
	}
	err := in.f.Close()
	in.f = nil
	return err
}

// pairCodeAnywhere matches a pairing code in any position, independent
// of command shape — the shape-independent N3 backstop below.
var pairCodeAnywhere = regexp.MustCompile(`pair1_[0-9A-Za-z]{20}`)

// SanitizePayload strips secrets from a payload before it touches the
// inbox (spec §2 step 1, N3): a /pair <code> is stored as
// /pair <redacted> so plaintext pairing codes never reach disk here.
// Beyond the command shape, ANY pair1_-shaped token is redacted
// wherever it appears — whatever the caller's command parser decides
// about leading whitespace or trailing text, the code never lands.
func SanitizePayload(payload string) string {
	if IsPairCommand(payload) {
		head := payload
		if i := strings.IndexAny(head, " 	"); i >= 0 {
			head = head[:i]
		}
		return head + " <redacted>"
	}
	return pairCodeAnywhere.ReplaceAllString(payload, "<redacted>")
}
