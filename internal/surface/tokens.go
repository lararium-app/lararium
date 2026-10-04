package surface

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	tokenPrefix   = "lar1_"
	tokenAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

type tokenEntry struct {
	Hash    string `json:"hash"`
	Label   string `json:"label"`
	Created string `json:"created"`
}

// TokenStore is the bearer-token registry on disk: JSON of SHA-256
// hashes (never plaintext), file mode 0600, re-read on every Verify so
// `token revoke` takes effect on the next request (spec §3).
type TokenStore struct {
	path string
}

// OpenTokenStore ensures the store file exists with mode 0600.
func OpenTokenStore(path string) (*TokenStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()

	return &TokenStore{path: path}, nil
}

// NewToken mints a lar1_-prefixed 32-char token from crypto/rand. The
// plaintext exists only in memory: it is printed once by `token create`
// and never written to disk (spec §3).
func NewToken() string {
	const tokenLen = 32
	alphabetLen := big.NewInt(int64(len(tokenAlphabet)))
	b := make([]byte, tokenLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, alphabetLen)
		if err != nil {
			panic(err)
		}
		b[i] = tokenAlphabet[n.Int64()]
	}

	return tokenPrefix + string(b)
}

// lock takes the flock on the token store file itself (0600), matching
// the keystore flock helper shape. The returned unlock is best-effort (defer it).
func (s *TokenStore) lock(mode int) (func(), error) {
	for {
		f, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		_ = os.Chmod(s.path, 0o600)
		if err := syscall.Flock(int(f.Fd()), mode); err != nil {
			f.Close()
			return nil, err
		}
		fi1, err1 := f.Stat()
		fi2, err2 := os.Stat(s.path)
		if err1 == nil && err2 == nil && os.SameFile(fi1, fi2) {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

// Create appends a hashed token under label and returns the plaintext
// exactly once, for the operator to copy into the ready-URL.
func (s *TokenStore) Create(label string) (string, error) {
	unlock, err := s.lock(syscall.LOCK_EX)
	if err != nil {
		return "", err
	}
	defer unlock()

	entries, err := s.readEntries()
	if err != nil {
		return "", err
	}

	plaintext := NewToken()
	hash := sha256.Sum256([]byte(plaintext))
	entry := tokenEntry{
		Hash:    hex.EncodeToString(hash[:]),
		Label:   label,
		Created: time.Now().UTC().Format(time.RFC3339),
	}

	entries = append(entries, entry)
	return plaintext, s.writeEntries(entries)
}

// Revoke drops every entry with the label; reports whether one existed.
// In-flight turns are unaffected — revocation gates new requests.
func (s *TokenStore) Revoke(label string) (bool, error) {
	unlock, err := s.lock(syscall.LOCK_EX)
	if err != nil {
		return false, err
	}
	defer unlock()

	entries, err := s.readEntries()
	if err != nil {
		return false, err
	}

	found := false
	newEntries := make([]tokenEntry, 0, len(entries))
	for _, e := range entries {
		if e.Label == label {
			found = true
		} else {
			newEntries = append(newEntries, e)
		}
	}

	if !found {
		return false, nil
	}

	return true, s.writeEntries(newEntries)
}

// Verify constant-time compares SHA-256(token) against every stored
// hash, re-reading the file each call (revoke = effective immediately).
func (s *TokenStore) Verify(token string) bool {
	entries, err := s.readEntries()
	if err != nil {
		return false
	}

	candidateHash := sha256.Sum256([]byte(token))
	candidateBytes := candidateHash[:]

	for _, e := range entries {
		storedBytes, err := hex.DecodeString(e.Hash)
		if err != nil {
			continue
		}
		if len(storedBytes) != 32 {
			continue
		}
		if subtle.ConstantTimeCompare(candidateBytes, storedBytes) == 1 {
			return true
		}
	}

	return false
}

func (s *TokenStore) readEntries() ([]tokenEntry, error) {
	data, err := os.ReadFile(s.path) //nolint:gosec // operator-configured path, not request data
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	if len(data) == 0 {
		return nil, nil
	}

	var entries []tokenEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}

	return entries, nil
}

func (s *TokenStore) writeEntries(entries []tokenEntry) error {
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "tokens-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(append(data, '\n')); err != nil {
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
