package fasti

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

// JournalEvent represents a single event in the fasti run journal (spec F1-4, D3, D4, D7, D12).
type JournalEvent struct {
	Seq       int64          `json:"seq"`
	Type      string         `json:"type"`               // e.g. "fasti_fired", "fasti_job_created", "fasti_job_paused"
	Timestamp string         `json:"timestamp"`          // RFC3339 timestamp
	JobID     string         `json:"job_id,omitempty"`   // target job ID
	JobName   string         `json:"job_name,omitempty"` // target job name
	Status    string         `json:"status,omitempty"`   // "fired", "skipped", "deferred", "silenced"
	Reason    string         `json:"reason,omitempty"`   // "session_busy", "engine_busy", "dst_gap", "slipped_while_offline", "quiet_collapse", "stale_discard", "shutdown"
	Prompt    string         `json:"prompt,omitempty"`   // prompt text or summary
	SessionID string         `json:"session_id,omitempty"`
	DueAt     string         `json:"due_at,omitempty"`
	FiredAt   string         `json:"fired_at,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

// Journal is the append-only events store under <hearth>/fasti/events.jsonl.
type Journal struct {
	mu      sync.Mutex
	dir     string
	path    string
	clock   loop.Clock
	nextSeq int64
}

// OpenJournal opens or initializes the run journal at <dir>/events.jsonl.
func OpenJournal(dir string, clock loop.Clock) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create journal dir %s: %w", dir, err)
	}

	path := filepath.Join(dir, "events.jsonl")
	j := &Journal{
		dir:     dir,
		path:    path,
		clock:   clock,
		nextSeq: 1,
	}

	if _, err := os.Stat(path); err == nil {
		events, err := j.readEventsFile()
		if err != nil {
			return nil, fmt.Errorf("read existing journal: %w", err)
		}
		if len(events) > 0 {
			j.nextSeq = events[len(events)-1].Seq + 1
		}
	}

	return j, nil
}

// Append persists a new event to the journal, stamping seq and timestamp if empty.
// Spec M6: uses O_APPEND line writes (O(1)) under mutex; complete lines survive kill -9.
func (j *Journal) Append(ev JournalEvent) (JournalEvent, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if ev.Seq == 0 {
		ev.Seq = j.nextSeq
		j.nextSeq++
	}
	if ev.Timestamp == "" {
		ev.Timestamp = j.clock.Now().UTC().Format(time.RFC3339)
	}

	lineData, err := json.Marshal(ev)
	if err != nil {
		return ev, fmt.Errorf("marshal journal event: %w", err)
	}
	line := make([]byte, len(lineData)+1)
	copy(line, lineData)
	line[len(lineData)] = '\n'

	f, err := os.OpenFile(j.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return ev, fmt.Errorf("open journal for append: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(line); err != nil {
		return ev, fmt.Errorf("write journal line: %w", err)
	}
	if err := f.Sync(); err != nil {
		return ev, fmt.Errorf("sync journal line: %w", err)
	}

	return ev, nil
}

// Events returns a snapshot of all events currently in the journal.
func (j *Journal) Events() ([]JournalEvent, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.readEventsFile()
}

func (j *Journal) readEventsFile() ([]JournalEvent, error) {
	data, err := os.ReadFile(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return []JournalEvent{}, nil
		}
		return nil, err
	}

	var events []JournalEvent
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev JournalEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("parse journal line %q: %w", line, err)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan journal: %w", err)
	}
	return events, nil
}
