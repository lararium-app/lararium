package web

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Activity record kind constants.
const (
	KindRead    = "read"
	KindAct     = "act"
	KindSignOut = "sign_out"

	VerdictOK      = "ok"
	VerdictBlocked = "blocked"
	VerdictDenied  = "denied"

	DefaultActivityRotateBytes int64 = 5 * 1024 * 1024 // 5 MiB
)

// ActivityRecord is one entry in sessions/activity.jsonl.
// By law, page text, cookies, and tokens are NEVER recorded.
type ActivityRecord struct {
	TS            string `json:"ts"`
	Origin        string `json:"origin"`
	URL           string `json:"url"`
	Kind          string `json:"kind"`
	Verdict       string `json:"verdict"`
	Intent        string `json:"intent,omitempty"`
	ScreenshotRef string `json:"screenshot_ref,omitempty"`
}

// ActivityLogger manages append-only logging to sessions/activity.jsonl with 5 MiB rotation.
type ActivityLogger struct {
	mu          sync.Mutex
	path        string
	rotateBytes int64
}

// NewActivityLogger creates a new ActivityLogger rooted at home.
func NewActivityLogger(home string) *ActivityLogger {
	return &ActivityLogger{
		path:        filepath.Join(home, "sessions", "activity.jsonl"),
		rotateBytes: DefaultActivityRotateBytes,
	}
}

// SetRotateBytes overrides rotation threshold (useful for testing).
func (l *ActivityLogger) SetRotateBytes(n int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rotateBytes = n
}

// Append writes a new ActivityRecord to activity.jsonl, rotating if size exceeds threshold.
func (l *ActivityLogger) Append(rec ActivityRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if rec.TS == "" {
		rec.TS = time.Now().UTC().Format(time.RFC3339)
	}

	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir activity dir: %w", err)
	}

	limit := l.rotateBytes
	if limit <= 0 {
		limit = DefaultActivityRotateBytes
	}

	if fi, err := os.Stat(l.path); err == nil {
		if fi.Size() >= limit {
			gen1 := l.path + ".1"
			_ = os.Remove(gen1)
			_ = os.Rename(l.path, gen1)
		}
	}

	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open activity log: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal activity record: %w", err)
	}
	data = append(data, '\n')

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write activity record: %w", err)
	}
	return nil
}

// Query returns records newest-first, capped at limit. If originFilter is non-empty,
// only records matching that origin are returned.
func (l *ActivityLogger) Query(limit int, originFilter string) ([]ActivityRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return queryActivityFiles(l.path, limit, originFilter)
}

// QueryActivity is a package-level helper to query activity records.
func QueryActivity(home string, limit int, originFilter string) ([]ActivityRecord, error) {
	logger := NewActivityLogger(home)
	return logger.Query(limit, originFilter)
}

func queryActivityFiles(primaryPath string, limit int, originFilter string) ([]ActivityRecord, error) {
	if limit <= 0 {
		limit = 50
	}

	var allRecords []ActivityRecord

	// Read primary file
	records1, err := readRecordsFromFile(primaryPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	// Read rotated generation (.1) if needed
	var records0 []ActivityRecord
	gen1Path := primaryPath + ".1"
	if r, err := readRecordsFromFile(gen1Path); err == nil {
		records0 = r
	}

	// Order: records in gen1 are older than records in primary file.
	// Within each file, records are appended chronologically.
	// So newest to oldest order is: primary reversed, then gen1 reversed.
	originFilter = strings.TrimSpace(originFilter)

	for i := len(records1) - 1; i >= 0; i-- {
		rec := records1[i]
		if originFilter != "" && !strings.EqualFold(rec.Origin, originFilter) {
			continue
		}
		allRecords = append(allRecords, rec)
		if len(allRecords) >= limit {
			return allRecords, nil
		}
	}

	for i := len(records0) - 1; i >= 0; i-- {
		rec := records0[i]
		if originFilter != "" && !strings.EqualFold(rec.Origin, originFilter) {
			continue
		}
		allRecords = append(allRecords, rec)
		if len(allRecords) >= limit {
			return allRecords, nil
		}
	}

	return allRecords, nil
}

func readRecordsFromFile(path string) ([]ActivityRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []ActivityRecord
	scanner := bufio.NewScanner(f)
	// Allow lines up to 64 KiB
	scanner.Buffer(make([]byte, 1024), 64*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec ActivityRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return out, err
	}
	return out, nil
}
