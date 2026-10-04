// Package keystore is the on-disk provider-credential store: a
// 0600 JSON file under the per-instance hearth dir, serialized
// mutations via flock, and the resolution order
// (env > keys.json > config literal) with visibility warnings.
package keystore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// Frozen failure strings — CLI and HTTP doors print/emit these
// byte-identically (KEYS-SPEC K3/K4).
var (
	ErrBadName = errors.New("invalid provider name")
	ErrEmpty   = errors.New("empty key")
	ErrFull    = errors.New("key store full")
)

// MaxKeys caps the store so a live daemon cannot be disk-filled
// through its own API (KEYS-SPEC K4).
const MaxKeys = 64

// ValidName is the single gate shared by both doors: lowercase letter
// or digit first, then letters/digits/underscore/hyphen, max 64 total.
// It rejects "..", "__proto__"-style leading underscores, and path
// separators by construction.
func ValidName(name string) bool {
	return nameRe.MatchString(name)
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Store is a keys.json under dir (path dir/keys.json, lock
// dir/keys.lock). Zero filesystem side effects until first use.
type Store struct {
	dir, path, lockPath string
}

// New returns a Store rooted at dir.
func New(dir string) *Store {
	return &Store{
		dir:      dir,
		path:     filepath.Join(dir, "keys.json"),
		lockPath: filepath.Join(dir, "keys.lock"),
	}
}

// Read returns a fresh copy of the store. A missing file is an empty
// store, not an error.
func (s *Store) Read() (map[string]string, error) {
	unlock, err := s.lock(syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.readLocked()
}

// Set validates, then stores the trimmed value under name. Overwrite
// of an existing name is always allowed; a NEW name at MaxKeys fails
// with ErrFull. One critical section: read → modify → atomic write.
func (s *Store) Set(name, value string) error {
	if !ValidName(name) {
		return ErrBadName
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return ErrEmpty
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	unlock, err := s.lock(syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer unlock()

	m, err := s.readLocked()
	if err != nil {
		return err
	}
	if _, exists := m[name]; !exists && len(m) >= MaxKeys {
		return ErrFull
	}
	m[name] = value
	return s.writeLocked(m)
}

// Remove deletes name. An absent name is not an error: it reports
// (false, nil) and leaves the file untouched.
func (s *Store) Remove(name string) (bool, error) {
	if !ValidName(name) {
		return false, ErrBadName
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return false, err
	}
	unlock, err := s.lock(syscall.LOCK_EX)
	if err != nil {
		return false, err
	}
	defer unlock()

	m, err := s.readLocked()
	if err != nil {
		return false, err
	}
	if _, ok := m[name]; !ok {
		return false, nil
	}
	delete(m, name)
	if err := s.writeLocked(m); err != nil {
		return false, err
	}
	return true, nil
}

// lock takes the flock, creating keys.lock 0600 as needed. The
// returned unlock is best-effort (defer it).
func (s *Store) lock(mode int) (func(), error) {
	f, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// OpenFile's mode applies only at creation; enforce it on a
	// pre-existing lock file too.
	_ = os.Chmod(s.lockPath, 0o600)
	if err := syscall.Flock(int(f.Fd()), mode); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// readLocked loads keys.json; caller holds the flock. Missing file →
// empty map. A corrupt file is returned as an error so callers decide
// whether it is fatal (mutations) or degraded (resolution).
func (s *Store) readLocked() (map[string]string, error) {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// writeLocked writes atomically INSIDE the same flock hold (no
// re-entrancy): temp in the store dir (same filesystem — rename
// cannot EXDEV), fsync, 0600, rename over keys.json.
func (s *Store) writeLocked(m map[string]string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, "keys-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}
