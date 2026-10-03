package surface

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

type TokenStore struct {
	path string
}

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

func NewToken() string {
	const tokenLen = 32
	b := make([]byte, tokenLen)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}

	for i := range b {
		b[i] = tokenAlphabet[b[i]%byte(len(tokenAlphabet))]
	}

	return tokenPrefix + string(b)
}

func (s *TokenStore) Create(label string) (string, error) {
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

func (s *TokenStore) Revoke(label string) (bool, error) {
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
	data, err := os.ReadFile(s.path)
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

	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}

	return os.Rename(tmpPath, s.path)
}
