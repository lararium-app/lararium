// Runtime connector store per CONNECTOR-LIFE-SPEC D2: <stateDir>/connectors.json
// carries API-mutated connector configs, per-entry enabled / last_used state,
// and tombstones for deleted names. lararium.yaml is never rewritten.

package custos

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ConnectorStoreFile is the store file name inside the custos state root.
const ConnectorStoreFile = "connectors.json"

// ErrConnectorStoreCorrupt is returned when connectors.json exists but cannot
// be trusted. The file carries user intent (tombstones), so boot refuses
// instead of silently discarding it.
var ErrConnectorStoreCorrupt = errors.New("connector store corrupt")

// ConnectorRecord is one stored connector entry. A record with no transport is
// a state-only overlay (enabled / last_used) on a lararium.yaml seed of the
// same name: seed configs may hold literals and are never copied into the
// store.
type ConnectorRecord struct {
	Name               string            `json:"name"`
	Transport          string            `json:"transport,omitempty"`
	Command            string            `json:"command,omitempty"`
	Args               []string          `json:"args,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	URL                string            `json:"url,omitempty"`
	Bearer             string            `json:"bearer,omitempty"`
	ToolAllowlist      []string          `json:"tool_allowlist,omitempty"`
	ToolDenylist       []string          `json:"tool_denylist,omitempty"`
	MaxConcurrentCalls int               `json:"max_concurrent_calls,omitempty"`
	ListTimeout        string            `json:"list_timeout,omitempty"`
	Enabled            bool              `json:"enabled"`
	LastUsed           *string           `json:"last_used"`
}

// HasConfig reports whether the record carries a full connector config.
func (r ConnectorRecord) HasConfig() bool { return r.Transport != "" }

// Config converts the record to an MCPServerConfig.
func (r ConnectorRecord) Config() MCPServerConfig {
	cfg := MCPServerConfig{
		Name:               r.Name,
		Transport:          r.Transport,
		Command:            r.Command,
		Args:               r.Args,
		Env:                r.Env,
		URL:                r.URL,
		Bearer:             r.Bearer,
		ToolAllowlist:      r.ToolAllowlist,
		ToolDenylist:       r.ToolDenylist,
		MaxConcurrentCalls: r.MaxConcurrentCalls,
	}
	if d, err := time.ParseDuration(r.ListTimeout); err == nil {
		cfg.ListTimeout = d
	}
	return cfg
}

// RecordFromConfig builds an enabled record from cfg.
func RecordFromConfig(cfg MCPServerConfig) ConnectorRecord {
	rec := ConnectorRecord{
		Name:               cfg.Name,
		Transport:          cfg.Transport,
		Command:            cfg.Command,
		Args:               cfg.Args,
		Env:                cfg.Env,
		URL:                cfg.URL,
		Bearer:             cfg.Bearer,
		ToolAllowlist:      cfg.ToolAllowlist,
		ToolDenylist:       cfg.ToolDenylist,
		MaxConcurrentCalls: cfg.MaxConcurrentCalls,
		Enabled:            true,
	}
	if cfg.ListTimeout > 0 {
		rec.ListTimeout = cfg.ListTimeout.String()
	}
	return rec
}

type connectorStoreDoc struct {
	Connectors []ConnectorRecord `json:"connectors"`
	Tombstones []string          `json:"tombstones"`
}

// MergedConnector is one boot-merge result entry.
type MergedConnector struct {
	Config   MCPServerConfig
	Enabled  bool
	LastUsed *time.Time
}

// ConnectorStore is the persisted runtime connector set.
type ConnectorStore struct {
	mu   sync.Mutex
	dir  string
	path string
	recs map[string]ConnectorRecord
	tomb map[string]bool
}

// OpenConnectorStore loads <stateDir>/connectors.json. A missing file is an
// empty store; a present-but-unparseable file is ErrConnectorStoreCorrupt.
func OpenConnectorStore(stateDir string) (*ConnectorStore, error) {
	s := &ConnectorStore{
		dir:  stateDir,
		path: filepath.Join(stateDir, ConnectorStoreFile),
		recs: make(map[string]ConnectorRecord),
		tomb: make(map[string]bool),
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("%w: read %s: %w", ErrConnectorStoreCorrupt, s.path, err)
	}
	var doc connectorStoreDoc
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrConnectorStoreCorrupt, s.path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: %s: trailing data", ErrConnectorStoreCorrupt, s.path)
	}
	for _, rec := range doc.Connectors {
		if !nameGrammarRe.MatchString(rec.Name) || rec.Name == "gmail" || rec.Name == "custos" {
			return nil, fmt.Errorf("%w: %s: bad connector name", ErrConnectorStoreCorrupt, s.path)
		}
		if rec.HasConfig() && rec.Transport != "stdio" && rec.Transport != "http" {
			return nil, fmt.Errorf("%w: %s: bad transport for %q", ErrConnectorStoreCorrupt, s.path, rec.Name)
		}
		if rec.LastUsed != nil {
			if _, perr := time.Parse(time.RFC3339, *rec.LastUsed); perr != nil {
				return nil, fmt.Errorf("%w: %s: bad last_used for %q", ErrConnectorStoreCorrupt, s.path, rec.Name)
			}
		}
		if rec.ListTimeout != "" {
			if _, perr := time.ParseDuration(rec.ListTimeout); perr != nil {
				return nil, fmt.Errorf("%w: %s: bad list_timeout for %q", ErrConnectorStoreCorrupt, s.path, rec.Name)
			}
		}
		s.recs[rec.Name] = rec
	}
	for _, name := range doc.Tombstones {
		if !nameGrammarRe.MatchString(name) {
			return nil, fmt.Errorf("%w: %s: bad tombstone name", ErrConnectorStoreCorrupt, s.path)
		}
		s.tomb[name] = true
	}
	return s, nil
}

// Merge applies the store over the lararium.yaml seed list (D2): seed first,
// store entries override by name (carrying enabled / last_used), tombstoned
// names dropped even when a seed has them. Store-only entries follow seeds.
func (s *ConnectorStore) Merge(seed []MCPServerConfig) []MergedConnector {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []MergedConnector
	used := make(map[string]bool)
	apply := func(m *MergedConnector, rec ConnectorRecord) {
		m.Enabled = rec.Enabled
		if rec.LastUsed != nil {
			if t, err := time.Parse(time.RFC3339, *rec.LastUsed); err == nil {
				m.LastUsed = &t
			}
		}
	}
	for _, cfg := range seed {
		if s.tomb[cfg.Name] {
			continue
		}
		m := MergedConnector{Config: cfg, Enabled: true}
		if rec, ok := s.recs[cfg.Name]; ok {
			if rec.HasConfig() {
				m.Config = rec.Config()
			}
			apply(&m, rec)
		}
		used[cfg.Name] = true
		out = append(out, m)
	}
	names := make([]string, 0, len(s.recs))
	for name := range s.recs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rec := s.recs[name]
		if used[name] || s.tomb[name] || !rec.HasConfig() {
			continue
		}
		m := MergedConnector{Config: rec.Config(), Enabled: true}
		apply(&m, rec)
		out = append(out, m)
	}
	return out
}

// Put stores (adds or replaces) a connector record and clears any tombstone.
func (s *ConnectorStore) Put(rec ConnectorRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs[rec.Name] = rec
	delete(s.tomb, rec.Name)
}

// Remove drops the record for name and writes a tombstone.
func (s *ConnectorStore) Remove(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recs, name)
	s.tomb[name] = true
}

// SetEnabled records the user toggle; a seed-only connector gets a state
// overlay record (no config copied).
func (s *ConnectorStore) SetEnabled(name string, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.recs[name]
	rec.Name = name
	rec.Enabled = enabled
	s.recs[name] = rec
}

// Get returns the record for name plus whether it existed and whether the
// name was tombstoned (used to roll back a failed write).
func (s *ConnectorStore) Get(name string) (rec ConnectorRecord, ok bool, tombstoned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok = s.recs[name]
	return rec, ok, s.tomb[name]
}

// Restore reverts name to a state captured by Get.
func (s *ConnectorStore) Restore(name string, rec ConnectorRecord, ok bool, tombstoned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.recs[name] = rec
	} else {
		delete(s.recs, name)
	}
	if tombstoned {
		s.tomb[name] = true
	} else {
		delete(s.tomb, name)
	}
}

// Snapshot returns a deep-enough copy for tests / inspection.
func (s *ConnectorStore) Snapshot() ([]ConnectorRecord, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortedLocked(nil)
}

func (s *ConnectorStore) sortedLocked(lastUsed func(name string) *time.Time) ([]ConnectorRecord, []string) {
	recs := make(map[string]ConnectorRecord, len(s.recs))
	for k, v := range s.recs {
		recs[k] = v
	}
	if lastUsed != nil {
		for name, rec := range recs {
			if t := lastUsed(name); t != nil {
				ts := t.UTC().Format(time.RFC3339)
				rec.LastUsed = &ts
				recs[name] = rec
			}
		}
	}
	out := make([]ConnectorRecord, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	tombs := make([]string, 0, len(s.tomb))
	for name := range s.tomb {
		tombs = append(tombs, name)
	}
	sort.Strings(tombs)
	return out, tombs
}

// Save writes the store atomically (temp + rename + fsync, mode 0600).
// lastUsed, when non-nil, is consulted per connector name so last_used rides
// along with whatever write happens next (best-effort persistence, D6);
// connectors that have been used but have no record get a state overlay.
func (s *ConnectorStore) Save(lastUsed func(name string) *time.Time, liveNames []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if lastUsed != nil {
		for _, name := range liveNames {
			if _, ok := s.recs[name]; ok || s.tomb[name] {
				continue
			}
			if lastUsed(name) != nil {
				s.recs[name] = ConnectorRecord{Name: name, Enabled: true}
			}
		}
	}
	recs, tombs := s.sortedLocked(lastUsed)
	// Fold last_used into memory so a later write without a callback keeps it.
	for _, rec := range recs {
		if cur, ok := s.recs[rec.Name]; ok {
			cur.LastUsed = rec.LastUsed
			s.recs[rec.Name] = cur
		}
	}
	// Prune pure no-op overlays.
	kept := recs[:0]
	for _, rec := range recs {
		if !rec.HasConfig() && rec.Enabled && rec.LastUsed == nil {
			delete(s.recs, rec.Name)
			continue
		}
		kept = append(kept, rec)
	}
	doc := connectorStoreDoc{Connectors: kept, Tombstones: tombs}
	if doc.Connectors == nil {
		doc.Connectors = []ConnectorRecord{}
	}
	if doc.Tombstones == nil {
		doc.Tombstones = []string{}
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal connector store: %w", err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	tmp, err := writeSyncTempNamed(s.dir, "connectors-*.tmp", data, false)
	if err != nil {
		return fmt.Errorf("write connector store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename connector store: %w", err)
	}
	return fsyncDir(s.dir)
}
