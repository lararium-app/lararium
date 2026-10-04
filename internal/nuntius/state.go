package nuntius

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// stateFileName is <hearth>/nuntius/state.json (spec §2): the getUpdates
// offset watermark and the chat→session pointer.
const stateFileName = "state.json"

// State is the persisted bridge state: the offset is what the next
// getUpdates passes (which is what acks at Telegram), and active_session
// is the one pointer the chat routes to (default "main", §5).
type State struct {
	Offset        int64  `json:"offset"`
	ActiveSession string `json:"active_session"`
}

// StateFile owns state.json (0600, atomic replace, fsync — §2).
type StateFile struct {
	dir string
}

// NewStateFile prepares <dir> (0700) and returns its file.
func NewStateFile(dir string) (*StateFile, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("nuntius: create state dir: %w", err)
	}
	return &StateFile{dir: dir}, nil
}

// Path is the state.json path.
func (f *StateFile) Path() string { return filepath.Join(f.dir, stateFileName) }

// Load reads state.json. Missing file is the fresh-start state (offset
// 0, session "main"); corrupt JSON is ErrCorruptState with the N8
// remedy — never a silent offset reset.
func (f *StateFile) Load() (State, error) {
	data, err := os.ReadFile(f.Path())
	if errors.Is(err, os.ErrNotExist) {
		return State{ActiveSession: "main"}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("nuntius: read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("%w: %s: %w — %s", ErrCorruptState, f.Path(), err, RemedyN8)
	}
	if s.ActiveSession == "" {
		s.ActiveSession = "main"
	}
	return s, nil
}

// Save writes state.json atomically.
func (f *StateFile) Save(s State) error {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("nuntius: encode state: %w", err)
	}
	return atomicWrite(f.dir, stateFileName, data, 0o600)
}

// atomicWrite writes data to dir/name via temp-file + rename with fsync
// before the rename, at the given mode (same doctrine as the keystore
// and surface token stores).
func atomicWrite(dir, name string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(dir, name+".tmp*")
	if err != nil {
		return fmt.Errorf("nuntius: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup of the temp file on any failure path.
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("nuntius: write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("nuntius: fsync temp: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("nuntius: chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("nuntius: close temp: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("nuntius: rename: %w", err)
	}
	return nil
}
