package fasti

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// BudgetCounters tracks execution and skip counts for a job row (spec D5/D10).
type BudgetCounters struct {
	TotalRuns     int `json:"total_runs,omitempty"`
	TotalSkips    int `json:"total_skips,omitempty"`
	TotalDefers   int `json:"total_defers,omitempty"`
	TotalFailures int `json:"total_failures,omitempty"`
}

// Job represents a single cron or interval task (spec D5).
type Job struct {
	ID               string         `json:"id"`
	Name             string         `json:"name,omitempty"`
	Schedule         Schedule       `json:"schedule"`
	Prompt           string         `json:"prompt"`
	Session          string         `json:"session,omitempty"`
	Announce         string         `json:"announce,omitempty"`
	Quiet            *QuietOverride `json:"quiet,omitempty"`
	Enabled          bool           `json:"enabled"`
	Paused           bool           `json:"paused"`
	CreatedBy        string         `json:"created_by"`
	LastFire         string         `json:"last_fire,omitempty"`
	NextDue          string         `json:"next_due,omitempty"`
	ConsecutiveSkips int            `json:"consecutive_skips"`
	BudgetCounters   BudgetCounters `json:"budget_counters,omitempty"`
}

// NewJobID generates a k-sortable ULID-style ID ("j_" + 26 Crockford base32 chars)
// using the provided timestamp (spec D5, penatus/session.go:19).
func NewJobID(now time.Time) string {
	ms := now.UnixMilli()

	var sb strings.Builder
	sb.WriteString("j_")

	// 10 chars ms timestamp in Crockford base32
	msBig := big.NewInt(ms)
	var timeChars [10]byte
	for i := 9; i >= 0; i-- {
		mod := new(big.Int).Mod(msBig, big.NewInt(32))
		timeChars[i] = crockford[mod.Int64()]
		msBig.Div(msBig, big.NewInt(32))
	}
	sb.Write(timeChars[:])

	// 16 random Crockford base32 chars
	var randChars [16]byte
	for i := range randChars {
		n, err := rand.Int(rand.Reader, big.NewInt(32))
		if err != nil {
			randChars[i] = '0'
		} else {
			randChars[i] = crockford[n.Int64()]
		}
	}
	sb.Write(randChars[:])
	return sb.String()
}

// ValidatePrompt checks that the prompt is non-empty, single-paragraph, and <= 1000 runes (spec D8/D9).
func ValidatePrompt(prompt string) error {
	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return errors.New("prompt must not be empty")
	}
	if utf8.RuneCountInString(trimmed) > 1000 {
		return fmt.Errorf("prompt exceeds 1000 runes (%d runes)", utf8.RuneCountInString(trimmed))
	}
	if strings.Contains(trimmed, "\n") || strings.Contains(trimmed, "\r") {
		return errors.New("prompt must be a single paragraph (no newlines)")
	}
	return nil
}

// Validate checks the structural validity of a Job row.
func (j *Job) Validate() error {
	if strings.TrimSpace(j.ID) == "" {
		return errors.New("job id must not be empty")
	}
	if err := ValidatePrompt(j.Prompt); err != nil {
		return err
	}
	if _, err := ParseSchedule(j.Schedule); err != nil {
		return fmt.Errorf("invalid schedule: %w", err)
	}
	switch j.CreatedBy {
	case "", "user", "model":
		if j.CreatedBy == "" {
			j.CreatedBy = "user"
		}
	default:
		// Spec D5 / brief item F1-2: created_by must be in {"", "user", "model"}
		return fmt.Errorf("invalid created_by %q: must be 'user' or 'model'", j.CreatedBy)
	}
	if strings.TrimSpace(j.Session) == "" {
		j.Session = "cron-" + j.ID
	}
	return nil
}

// JobsFilePath returns the standard path to jobs.json under the daemon home directory.
func JobsFilePath(home string) string {
	return filepath.Join(home, "fasti", "jobs.json")
}

// writeAtomic persists data atomically following the writeSyncTempNamed pattern
// (internal/custos/vault.go:36-68) with 0600 mode and temp+rename+fsync.
func writeAtomic(dir, filename string, data []byte) error {
	if filepath.Base(filename) != filename || filename == "." || filename == ".." {
		return fmt.Errorf("unsafe filename %q", filename)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}

	pattern := filename + "-*.tmp"
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}

	target := filepath.Join(dir, filename)
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("rename temp to %s: %w", target, err)
	}
	cleanup = false
	return nil
}

// LoadJobs reads and parses the jobs file from disk.
// If the file does not exist, an empty slice is returned.
func LoadJobs(path string) ([]Job, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Job{}, nil
		}
		return nil, fmt.Errorf("read jobs file: %w", err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return []Job{}, nil
	}
	var jobs []Job
	if err := json.Unmarshal([]byte(trimmed), &jobs); err != nil {
		return nil, fmt.Errorf("parse jobs file: %w", err)
	}
	return jobs, nil
}

// SaveJobs atomically writes the jobs array to disk under 0600 permissions.
func SaveJobs(path string, jobs []Job) error {
	dir := filepath.Dir(path)
	filename := filepath.Base(path)
	if jobs == nil {
		jobs = []Job{}
	}
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal jobs: %w", err)
	}
	return writeAtomic(dir, filename, append(data, '\n'))
}
