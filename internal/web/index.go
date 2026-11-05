package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Policy enum values for session act policies.
const (
	PolicyAlwaysAsk  = "always_ask"
	PolicyDailyAllow = "daily_allow"
)

// Frozen wallet policy error values.
var (
	ErrInvalidPolicy   = errors.New("invalid policy enum: must be always_ask or daily_allow")
	ErrSessionNotFound = errors.New("session not found")
)

// ValidPolicy reports whether p is an allowed act policy enum.
func ValidPolicy(p string) bool {
	return p == PolicyAlwaysAsk || p == PolicyDailyAllow
}

// WalletEntry holds metadata for one origin session in sessions/index.json.
// Secrets, tokens, and cookies are NEVER stored here.
type WalletEntry struct {
	Origin         string `json:"origin"`
	Policy         string `json:"policy"`
	CreatedAt      string `json:"created_at"`
	LastUsedAt     string `json:"last_used_at"`
	LastGoal       string `json:"last_goal,omitempty"`
	DailyAllowDate string `json:"daily_allow_date,omitempty"`
}

// WalletIndex manages sessions/index.json in plaintext.
type WalletIndex struct {
	mu   sync.RWMutex
	path string
}

// NewWalletIndex creates a new WalletIndex rooted under home.
func NewWalletIndex(home string) *WalletIndex {
	return &WalletIndex{
		path: filepath.Join(home, "sessions", "index.json"),
	}
}

func (idx *WalletIndex) readLocked() (map[string]WalletEntry, error) {
	data, err := os.ReadFile(idx.path)
	if os.IsNotExist(err) {
		return make(map[string]WalletEntry), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read index: %w", err)
	}
	if len(bytesTrim(data)) == 0 {
		return make(map[string]WalletEntry), nil
	}
	var m map[string]WalletEntry
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("unmarshal index: %w", err)
	}
	if m == nil {
		m = make(map[string]WalletEntry)
	}
	return m, nil
}

func (idx *WalletIndex) writeLocked(m map[string]WalletEntry) error {
	dir := filepath.Dir(idx.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir index dir: %w", err)
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal index: %w", err)
	}
	data = append(data, '\n')

	tmpFile, err := os.CreateTemp(dir, ".index-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp index: %w", err)
	}
	tmpName := tmpFile.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write temp index: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("sync temp index: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp index: %w", err)
	}

	if err := os.Rename(tmpName, idx.path); err != nil {
		return fmt.Errorf("rename index: %w", err)
	}
	tmpName = ""
	return nil
}

// Get returns the WalletEntry for origin, or false if not found.
func (idx *WalletIndex) Get(origin string) (WalletEntry, bool) {
	key, err := OriginKey(origin)
	if err != nil {
		return WalletEntry{}, false
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	m, err := idx.readLocked()
	if err != nil {
		return WalletEntry{}, false
	}
	entry, ok := m[key]
	return entry, ok
}

// Set saves or updates a WalletEntry for entry.Origin.
func (idx *WalletIndex) Set(entry WalletEntry) error {
	key, err := OriginKey(entry.Origin)
	if err != nil {
		return fmt.Errorf("invalid entry origin: %w", err)
	}

	if entry.Policy == "" {
		entry.Policy = PolicyAlwaysAsk
	} else if !ValidPolicy(entry.Policy) {
		return ErrInvalidPolicy
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if entry.CreatedAt == "" {
		entry.CreatedAt = now
	}
	if entry.LastUsedAt == "" {
		entry.LastUsedAt = entry.CreatedAt
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	m, err := idx.readLocked()
	if err != nil {
		return err
	}

	m[key] = entry
	return idx.writeLocked(m)
}

// List returns all WalletEntry rows sorted alphabetically by Origin.
func (idx *WalletIndex) List() ([]WalletEntry, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	m, err := idx.readLocked()
	if err != nil {
		return nil, err
	}

	rows := make([]WalletEntry, 0, len(m))
	for _, entry := range m {
		rows = append(rows, entry)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Origin < rows[j].Origin
	})
	return rows, nil
}

// SetPolicy updates the act policy enum for origin.
func (idx *WalletIndex) SetPolicy(origin, policy string) (WalletEntry, error) {
	if !ValidPolicy(policy) {
		return WalletEntry{}, ErrInvalidPolicy
	}
	key, err := OriginKey(origin)
	if err != nil {
		return WalletEntry{}, fmt.Errorf("invalid origin: %w", err)
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	m, err := idx.readLocked()
	if err != nil {
		return WalletEntry{}, err
	}

	entry, ok := m[key]
	if !ok {
		return WalletEntry{}, ErrSessionNotFound
	}

	entry.Policy = policy
	m[key] = entry
	if err := idx.writeLocked(m); err != nil {
		return WalletEntry{}, err
	}
	return entry, nil
}

// Delete removes an entry from index.json.
func (idx *WalletIndex) Delete(origin string) (bool, error) {
	key, err := OriginKey(origin)
	if err != nil {
		return false, fmt.Errorf("invalid origin: %w", err)
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	m, err := idx.readLocked()
	if err != nil {
		return false, err
	}

	if _, ok := m[key]; !ok {
		return false, nil
	}
	delete(m, key)
	return true, idx.writeLocked(m)
}

// Touch updates LastUsedAt and LastGoal for an origin.
func (idx *WalletIndex) Touch(origin, goal string) error {
	key, err := OriginKey(origin)
	if err != nil {
		return fmt.Errorf("invalid origin: %w", err)
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	m, err := idx.readLocked()
	if err != nil {
		return err
	}

	entry, ok := m[key]
	if !ok {
		entry = WalletEntry{
			Origin:    origin,
			Policy:    PolicyAlwaysAsk,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}
	entry.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
	if strings.TrimSpace(goal) != "" {
		entry.LastGoal = goal
	}
	m[key] = entry
	return idx.writeLocked(m)
}

// StampDailyAllow records the date string for daily_allow policy.
func (idx *WalletIndex) StampDailyAllow(origin, date string) error {
	key, err := OriginKey(origin)
	if err != nil {
		return fmt.Errorf("invalid origin: %w", err)
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	m, err := idx.readLocked()
	if err != nil {
		return err
	}

	entry, ok := m[key]
	if !ok {
		entry = WalletEntry{
			Origin:    origin,
			Policy:    PolicyDailyAllow,
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}
	entry.DailyAllowDate = date
	m[key] = entry
	return idx.writeLocked(m)
}

func bytesTrim(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}
