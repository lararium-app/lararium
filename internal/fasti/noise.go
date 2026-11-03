package fasti

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NormalizeAnnouncement normalizes announcement text per spec D7:
// whitespace-collapsed, case-folded, and truncated to the first 200 runes.
func NormalizeAnnouncement(text string) string {
	fields := strings.Fields(text)
	collapsed := strings.Join(fields, " ")
	folded := strings.ToLower(collapsed)
	runes := []rune(folded)
	if len(runes) > 200 {
		runes = runes[:200]
	}
	return string(runes)
}

// DedupEntry records an announcement hash/normalized text with its UTC timestamp (spec D7).
type DedupEntry struct {
	NormalizedText   string `json:"normalized_text"`
	LastAnnouncedUTC string `json:"last_announced_utc"`
}

// NoticeStats tracks operational notice counts and last occurrence (spec D7).
type NoticeStats struct {
	Count    int    `json:"count"`
	LastTime string `json:"last_time"`
}

// NoiseState persists rate-limiting and dedup counters in <hearth>/fasti/state.json (spec D7/R2).
type NoiseState struct {
	CurrentDay        string                 `json:"current_day"`
	AnnounceCount     int                    `json:"announce_count"`
	OverflowDayStreak []string               `json:"overflow_day_streak"`
	DedupWindow       []DedupEntry           `json:"dedup_window"`
	Notices           map[string]NoticeStats `json:"notices"`
}

// NoiseSnapshot is the exported read-only view for the future /v1/fasti/state surface (spec D7/D8).
type NoiseSnapshot struct {
	CurrentDay        string                 `json:"current_day"`
	AnnounceCount     int                    `json:"announce_count"`
	MaxAnnouncePerDay int                    `json:"max_announce_per_day"`
	OverflowDayStreak []string               `json:"overflow_day_streak"`
	DedupWindowCount  int                    `json:"dedup_window_count"`
	Notices           map[string]NoticeStats `json:"notices"`
}

// Prune cleans expired dedup entries (>24h), overflow streak entries (>7 days),
// and resets daily announce counters on UTC-day rollover (spec D7).
func (s *NoiseState) Prune(now time.Time) {
	nowUTC := now.UTC()
	today := nowUTC.Format("2006-01-02")
	if s.CurrentDay != today {
		s.CurrentDay = today
		s.AnnounceCount = 0
	}

	// Prune dedup window: entries within previous 24h
	var freshDedup []DedupEntry
	for _, entry := range s.DedupWindow {
		t, err := time.Parse(time.RFC3339, entry.LastAnnouncedUTC)
		if err == nil && nowUTC.Sub(t) < 24*time.Hour && !t.After(nowUTC) {
			freshDedup = append(freshDedup, entry)
		}
	}
	s.DedupWindow = freshDedup

	// Prune overflow day streak: entries within rolling 7 days
	var freshStreak []string
	for _, dayStr := range s.OverflowDayStreak {
		d, err := time.Parse("2006-01-02", dayStr)
		if err == nil && nowUTC.Sub(d) < 7*24*time.Hour && !d.After(nowUTC) {
			freshStreak = append(freshStreak, dayStr)
		}
	}
	s.OverflowDayStreak = freshStreak

	if s.Notices == nil {
		s.Notices = make(map[string]NoticeStats)
	}
}

// CheckDedup reports whether an identical normalized announcement occurred within the previous 24h (spec D7).
func (s *NoiseState) CheckDedup(normText string, now time.Time) bool {
	nowUTC := now.UTC()
	for _, entry := range s.DedupWindow {
		if entry.NormalizedText == normText {
			t, err := time.Parse(time.RFC3339, entry.LastAnnouncedUTC)
			if err == nil && nowUTC.Sub(t) < 24*time.Hour {
				return true
			}
		}
	}
	return false
}

// RecordAnnouncement increments the daily announce counter and appends to the dedup window.
func (s *NoiseState) RecordAnnouncement(normText string, now time.Time) {
	s.AnnounceCount++
	s.DedupWindow = append(s.DedupWindow, DedupEntry{
		NormalizedText:   normText,
		LastAnnouncedUTC: now.UTC().Format(time.RFC3339),
	})
}

// RecordOverflow records today as an overflow drop day and reports whether the 5-in-7 streak
// has been reached to trigger an operational digest notice (spec D7).
func (s *NoiseState) RecordOverflow(now time.Time) bool {
	today := now.UTC().Format("2006-01-02")
	found := false
	for _, d := range s.OverflowDayStreak {
		if d == today {
			found = true
			break
		}
	}
	if !found {
		s.OverflowDayStreak = append(s.OverflowDayStreak, today)
	}

	if len(s.OverflowDayStreak) >= 5 {
		s.OverflowDayStreak = []string{}
		return true
	}
	return false
}

// RecordNotice updates audit statistics for an operational notice type.
func (s *NoiseState) RecordNotice(kind string, now time.Time) {
	if s.Notices == nil {
		s.Notices = make(map[string]NoticeStats)
	}
	stats := s.Notices[kind]
	stats.Count++
	stats.LastTime = now.UTC().Format(time.RFC3339)
	s.Notices[kind] = stats
}

// Snapshot returns a copy of current noise counters for state inspection.
func (s *NoiseState) Snapshot(maxPerDay int) NoiseSnapshot {
	streakCopy := make([]string, len(s.OverflowDayStreak))
	copy(streakCopy, s.OverflowDayStreak)

	noticesCopy := make(map[string]NoticeStats, len(s.Notices))
	for k, v := range s.Notices {
		noticesCopy[k] = v
	}

	return NoiseSnapshot{
		CurrentDay:        s.CurrentDay,
		AnnounceCount:     s.AnnounceCount,
		MaxAnnouncePerDay: maxPerDay,
		OverflowDayStreak: streakCopy,
		DedupWindowCount:  len(s.DedupWindow),
		Notices:           noticesCopy,
	}
}

// NoiseStateFilePath returns the standard path to state.json under the daemon home directory.
func NoiseStateFilePath(home string) string {
	return filepath.Join(home, "fasti", "state.json")
}

// LoadNoiseState reads and parses state.json from disk, or initializes default state if absent.
func LoadNoiseState(path string, now time.Time) (*NoiseState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s := &NoiseState{
				CurrentDay:        now.UTC().Format("2006-01-02"),
				AnnounceCount:     0,
				OverflowDayStreak: []string{},
				DedupWindow:       []DedupEntry{},
				Notices:           make(map[string]NoticeStats),
			}
			return s, nil
		}
		return nil, fmt.Errorf("read noise state: %w", err)
	}

	var s NoiseState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse noise state: %w", err)
	}
	if s.Notices == nil {
		s.Notices = make(map[string]NoticeStats)
	}
	s.Prune(now)
	return &s, nil
}

// SaveNoiseState atomically writes noise state to disk under 0600 mode using the writeSyncTempNamed pattern.
func SaveNoiseState(path string, state *NoiseState) error {
	dir := filepath.Dir(path)
	filename := filepath.Base(path)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal noise state: %w", err)
	}
	return writeAtomic(dir, filename, append(data, '\n'))
}
