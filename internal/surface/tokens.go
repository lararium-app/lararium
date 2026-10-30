package surface

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	Expires string `json:"expires,omitempty"`
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

// Create appends a non-expiring hashed token under label and returns the
// plaintext exactly once, for the operator to copy into the ready-URL.
func (s *TokenStore) Create(label string) (string, error) {
	return s.CreateExpiring(label, 0)
}

// CreateExpiring appends a hashed token under label with an optional TTL.
// If ttl > 0, Expires is set to time.Now().UTC().Add(ttl).Format(time.RFC3339);
// if ttl <= 0, Expires is left empty. While holding the exclusive lock,
// CreateExpiring drops entries already past their expiry (opportunistic GC).
func (s *TokenStore) CreateExpiring(label string, ttl time.Duration) (string, error) {
	unlock, err := s.lock(syscall.LOCK_EX)
	if err != nil {
		return "", err
	}
	defer unlock()

	entries, err := s.readEntries()
	if err != nil {
		return "", err
	}

	now := time.Now().UTC()
	pruned := make([]tokenEntry, 0, len(entries))
	for _, e := range entries {
		if e.Expires != "" {
			if exp, err := time.Parse(time.RFC3339, e.Expires); err == nil {
				if !now.Before(exp) {
					continue
				}
			}
		}
		pruned = append(pruned, e)
	}
	entries = pruned

	plaintext := NewToken()
	hash := sha256.Sum256([]byte(plaintext))
	entry := tokenEntry{
		Hash:    hex.EncodeToString(hash[:]),
		Label:   label,
		Created: now.Format(time.RFC3339),
	}
	if ttl > 0 {
		entry.Expires = now.Add(ttl).Format(time.RFC3339)
	}

	entries = append(entries, entry)
	return plaintext, s.writeEntries(entries)
}

// RevokeLabel drops every entry with the label; reports the number of
// entries removed. In-flight turns are unaffected — revocation gates
// new requests. Unpair sweeps use it to clear all tokens matching the
// unpaired identity.
func (s *TokenStore) RevokeLabel(label string) (int, error) {
	unlock, err := s.lock(syscall.LOCK_EX)
	if err != nil {
		return 0, err
	}
	defer unlock()

	entries, err := s.readEntries()
	if err != nil {
		return 0, err
	}

	removed := 0
	newEntries := make([]tokenEntry, 0, len(entries))
	for _, e := range entries {
		if e.Label == label {
			removed++
		} else {
			newEntries = append(newEntries, e)
		}
	}

	if removed == 0 {
		return 0, nil
	}

	if err := s.writeEntries(newEntries); err != nil {
		return 0, err
	}
	return removed, nil
}

// RevokeHashPrefix drops the single entry whose stored SHA-256 hash
// begins with prefix. Prefix must be at least 8 lowercase hex characters.
// Reports 1 on success, 0 on not-found, or an error if the prefix is too short
// or matches multiple entries.
func (s *TokenStore) RevokeHashPrefix(prefix string) (int, error) {
	prefix = strings.ToLower(prefix)
	if len(prefix) < 8 {
		return 0, errors.New("token id prefix must be at least 8 hex characters (use the prefix from token list)")
	}

	unlock, err := s.lock(syscall.LOCK_EX)
	if err != nil {
		return 0, err
	}
	defer unlock()

	entries, err := s.readEntries()
	if err != nil {
		return 0, err
	}

	matchIndex := -1
	matchCount := 0
	for i, e := range entries {
		if strings.HasPrefix(e.Hash, prefix) {
			matchCount++
			matchIndex = i
		}
	}

	if matchCount == 0 {
		return 0, nil
	}
	if matchCount > 1 {
		return 0, fmt.Errorf("prefix matches %d tokens; extend the prefix", matchCount)
	}

	newEntries := make([]tokenEntry, 0, len(entries)-1)
	newEntries = append(newEntries, entries[:matchIndex]...)
	newEntries = append(newEntries, entries[matchIndex+1:]...)

	if err := s.writeEntries(newEntries); err != nil {
		return 0, err
	}
	return 1, nil
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
		if e.Expires != "" {
			exp, err := time.Parse(time.RFC3339, e.Expires)
			if err != nil || !time.Now().UTC().Before(exp) {
				continue
			}
		}
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

// TokenInfo is the display view of one issued token: label, creation
// date, and a short identifying prefix derived from the stored hash.
// The prefix comes from the one-way hash, never the plaintext, so
// listing tokens cannot leak credentials (L-7).
type TokenInfo struct {
	Label   string
	Created string
	Prefix  string
	Expires string `json:"expires,omitempty"`
	Expired bool   `json:"expired"`
}

// List returns every live token sorted by label (L-7, audit 2026-10-23):
// bearer tokens are a shared secret with no owner binding, so the
// operator must always be able to enumerate exactly what is live before
// multi-user work lands. Display metadata only — never plaintext.
func (s *TokenStore) List() ([]TokenInfo, error) {
	unlock, err := s.lock(syscall.LOCK_SH)
	if err != nil {
		return nil, err
	}
	defer unlock()

	entries, err := s.readEntries()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	infos := make([]TokenInfo, 0, len(entries))
	for _, e := range entries {
		prefix := e.Hash
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		expired := false
		if e.Expires != "" {
			exp, err := time.Parse(time.RFC3339, e.Expires)
			if err != nil || !now.Before(exp) {
				expired = true
			}
		}
		infos = append(infos, TokenInfo{
			Label:   e.Label,
			Created: e.Created,
			Prefix:  prefix,
			Expires: e.Expires,
			Expired: expired,
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Label < infos[j].Label })
	return infos, nil
}
