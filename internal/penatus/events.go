package penatus

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// CorruptError indicates a seq gap or duplicate in an events log.
type CorruptError struct {
	Got  int64
	Want int64
}

func (e CorruptError) Error() string {
	return fmt.Sprintf("corrupt log: got seq %d, want %d", e.Got, e.Want)
}

// Event represents a single line in the events.jsonl transcript.
type Event struct {
	Seq    int64  `json:"seq"`
	T      string `json:"t"`
	TS     string `json:"ts"`
	Fields map[string]json.RawMessage
}

// MarshalJSON flattens Fields alongside seq, t, ts.
func (e Event) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage)
	out["seq"] = json.RawMessage(fmt.Sprintf(`%d`, e.Seq))
	out["t"] = json.RawMessage(fmt.Sprintf("%q", e.T))
	if e.TS != "" {
		out["ts"] = json.RawMessage(fmt.Sprintf("%q", e.TS))
	}
	for k, v := range e.Fields {
		out[k] = v
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads the event, keeping unknown fields in Fields.
func (e *Event) UnmarshalJSON(data []byte) error {
	raw := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	// Extract known fields
	if v, ok := raw["seq"]; ok {
		delete(raw, "seq")
		var seq int64
		if err := json.Unmarshal(v, &seq); err != nil {
			return err
		}
		e.Seq = seq
	}
	if v, ok := raw["t"]; ok {
		delete(raw, "t")
		var t string
		if err := json.Unmarshal(v, &t); err != nil {
			return err
		}
		e.T = t
	}
	if v, ok := raw["ts"]; ok {
		delete(raw, "ts")
		var ts string
		if err := json.Unmarshal(v, &ts); err != nil {
			return err
		}
		e.TS = ts
	}

	e.Fields = raw
	return nil
}

// Log is an append-only events.jsonl log for a session.
//
// Concurrency contract (SOAK-INCIDENT-2026-11-15, defect B): appends are
// serialized in-process by mu AND across Log instances/processes by an
// flock on a sidecar lock file. The next seq is always re-derived from
// the file while the lock is held, so a stale cached instance can never
// assign a duplicate seq to a second concurrent writer.
type Log struct {
	// mu guards every field below (and the in-memory events mirror).
	// Append additionally holds the file lock while assigning seq,
	// writing, and re-deriving nextSeq.
	mu       sync.Mutex
	dir      string
	path     string
	readOnly bool
	events   []Event
	nextSeq  int64
}

// Dir returns the session directory this log lives in.
func (l *Log) Dir() string { return l.dir }

// OpenLog reads and validates the events.jsonl in the given session directory.
// Seq must increase by 1 starting from 1. A gap or duplicate opens the log
// read-only and returns a CorruptError.
func OpenLog(dir string) (*Log, error) {
	path := filepath.Join(dir, "events.jsonl")

	// M7: sessions are secret-class; tighten pre-existing loose modes
	// from older builds on open (best-effort — never fail an open).
	_ = os.Chmod(dir, 0o700)
	if _, err := os.Stat(path); err == nil {
		_ = os.Chmod(path, 0o600)
	}

	log := &Log{
		dir:  dir,
		path: path,
	}

	// File might not exist yet (new session)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.nextSeq = 1
			return log, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	expectedSeq := int64(1)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var evt Event
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			return nil, fmt.Errorf("parse error at line: %w", err)
		}

		if evt.Seq != expectedSeq {
			log.readOnly = true
			return log, CorruptError{Got: evt.Seq, Want: expectedSeq}
		}

		log.events = append(log.events, evt)
		expectedSeq++
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	log.nextSeq = expectedSeq
	return log, nil
}

// Append writes an event to the log with O_APPEND and fsync.
// Assigns Seq (next) and TS (UTC now, RFC3339Nano) if empty.
// Refuses on a read-only (corrupt) log.
//
// The seq assignment and the write happen while holding both the
// in-process mutex and an exclusive flock on the sidecar lock file, and
// nextSeq is re-derived from the on-disk tail first. Two concurrent
// writers (scheduler wake vs. turn completion, separate Log instances
// or separate processes) therefore serialize instead of colliding on a
// cached seq — the seq=898 duplicate that corrupted the SOAK-INCIDENT
// transcript could not happen under this contract.
func (l *Log) Append(e Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(e)
}

// appendLocked performs the locked append. Callers hold l.mu.
func (l *Log) appendLocked(e Event) error {
	if l.readOnly {
		return fmt.Errorf("log is read-only (corrupt)")
	}

	unlock, err := l.lockFile()
	if err != nil {
		return err
	}
	defer unlock()

	// Re-derive seq from disk while the lock is held: another writer
	// (this or another process) may have appended since our open or
	// last append. resyncLocked() only advances nextSeq, never rewinds.
	if err := l.resyncLocked(); err != nil {
		return err
	}

	if e.Seq == 0 {
		e.Seq = l.nextSeq
	}
	if e.TS == "" {
		e.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}

	data, err := e.MarshalJSON()
	if err != nil {
		return err
	}

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write(append(data, '\n'))
	if err != nil {
		return err
	}

	if err := f.Sync(); err != nil {
		return err
	}

	l.events = append(l.events, e)
	l.nextSeq++

	return nil
}

// lockFile takes an exclusive flock on <events.jsonl>.lock and returns
// the unlock func. The sidecar (not events.jsonl itself) is locked so
// readers opening the transcript never block behind the lock semantics
// on the data file.
func (l *Log) lockFile() (func(), error) {
	lockPath := l.path + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lf.Fd()), unix.LOCK_EX); err != nil {
		lf.Close()
		return nil, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	return func() {
		_ = unix.Flock(int(lf.Fd()), unix.LOCK_UN)
		_ = lf.Close()
	}, nil
}

// resyncLocked re-reads the on-disk tail while the file lock is held and
// advances nextSeq (and the in-memory mirror) past anything appended by
// another writer. Returns an error if the file shrank or a seq gap or
// duplicate appears (the log is then marked read-only, same as OpenLog).
func (l *Log) resyncLocked() error {
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			// Nobody has written yet; our own append will create it.
			if l.nextSeq == 0 {
				l.nextSeq = 1
			}
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	expected := int64(1)
	var tail []Event
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var evt Event
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			l.readOnly = true
			return fmt.Errorf("penatus: resync parse error: %w", err)
		}
		if evt.Seq != expected {
			l.readOnly = true
			return CorruptError{Got: evt.Seq, Want: expected}
		}
		tail = append(tail, evt)
		expected++
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if int64(len(tail)) < int64(len(l.events)) {
		// File shrank under us — treat as corruption; never rewind.
		l.readOnly = true
		return fmt.Errorf("penatus: log shrank (%d lines < %d cached)", len(tail), len(l.events))
	}
	// Mirror what we missed so Live() sees the other writer's events too.
	l.events = tail
	l.nextSeq = expected
	return nil
}

// Live returns events that are not tombstoned. If a compact event exists,
// assembly per spec §3 is applied: summary replaces its covers range,
// kept members ride along. Handles transitive nested compactions.
//
// The snapshot is taken under mu so a concurrent Append can neither race
// the slice read nor alias the backing array.
func (l *Log) Live() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.liveLocked()
}

func (l *Log) liveLocked() []Event {
	// Build tombstone set
	tombstoned := make(map[int64]bool)
	for _, e := range l.events {
		if e.T == "tombstone" {
			if seqsRaw, ok := e.Fields["seqs"]; ok {
				var seqs []int64
				if err := json.Unmarshal(seqsRaw, &seqs); err == nil {
					for _, s := range seqs {
						tombstoned[s] = true
					}
				}
			}
		}
	}

	// Build compact map: seq -> compact event
	var compacts []Event
	for _, e := range l.events {
		if e.T == "compact" {
			compacts = append(compacts, e)
		}
	}

	if len(compacts) == 0 {
		// No compaction — just filter tombstones
		var result []Event
		for _, e := range l.events {
			if !tombstoned[e.Seq] {
				result = append(result, e)
			}
		}
		return result
	}

	// Resolve compactions transitively.
	// We need to figure out which seq ranges are covered by compaction.
	// For each compact event, covers = [start, end] inclusive range of seqs
	// that the summary replaces. kept members survive.

	// Build a map of which seqs are "covered" by a compact (and which compact covers them)
	type coveredRange struct {
		compactSeq int64
		start      int64
		end        int64
		kept       map[int64]bool
	}

	var ranges []coveredRange
	for _, c := range compacts {
		var covers []int64
		if v, ok := c.Fields["covers"]; ok {
			// Corrupt covers: skip this compact — its covered seqs
			// replay as live events (safe direction, same as the
			// < 2 bounds skip below).
			if err := json.Unmarshal(v, &covers); err != nil {
				continue
			}
		}
		if len(covers) < 2 {
			continue
		}
		start, end := covers[0], covers[1]

		keptSet := make(map[int64]bool)
		if v, ok := c.Fields["kept"]; ok {
			var kept []struct {
				Seq int64 `json:"seq"`
			}
			if err := json.Unmarshal(v, &kept); err == nil {
				for _, k := range kept {
					keptSet[k.Seq] = true
				}
			}
		}

		ranges = append(ranges, coveredRange{
			compactSeq: c.Seq,
			start:      start,
			end:        end,
			kept:       keptSet,
		})
	}

	// Determine which seqs are covered by the latest compaction(s).
	// A seq is "covered" if it falls within any compact's covers range,
	// unless it's in kept. But if a covered seq is itself a compact,
	// we resolve transitively (the inner compact's summary is subsumed).

	// Find the latest compaction (highest seq) that is not itself covered
	// by a later compaction.
	// Simple approach: process all events, track active coverage.

	covered := make(map[int64]bool)  // seqs covered by some compact (replaced by summary)
	keptSeqs := make(map[int64]bool) // seqs explicitly kept by some compact

	// Process compacts from earliest to latest; later compacts override.
	for _, r := range ranges {
		for s := r.start; s <= r.end; s++ {
			covered[s] = true
			if r.kept[s] {
				keptSeqs[s] = true
			}
		}
	}

	// Build result
	var result []Event
	for _, e := range l.events {
		seq := e.Seq

		// Skip tombstoned
		if tombstoned[seq] {
			continue
		}

		// Skip covered seqs (they're replaced by compact summary)
		if covered[seq] && !keptSeqs[seq] {
			continue
		}

		result = append(result, e)
	}

	return result
}
