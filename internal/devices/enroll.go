package devices

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

const (
	// DefaultEnrollTTL is the fixed 24-hour validity window for device enrollment codes (DEVICE-RAILS-SPEC D-ENROLL).
	DefaultEnrollTTL = 24 * time.Hour

	enrollFileName     = "enroll.json"
	enrollLockName     = "enroll.lock"
	enrollCodePrefix   = "dev1_"
	enrollCodeLen      = 20
	enrollCodeAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// ErrCorruptEnrollState is returned when enroll.json cannot be parsed; callers
// must refuse rather than silently resetting state.
var ErrCorruptEnrollState = errors.New("corrupt device enrollment state")

// EnrollCode is one pending device enrollment code stored SHA-256-hashed at rest.
type EnrollCode struct {
	Hash    string `json:"hash"`
	Created int64  `json:"created"`
	Expires int64  `json:"expires"`
}

// EnrollDoc is the serialized form of <custosDir>/enroll.json.
type EnrollDoc struct {
	Codes []EnrollCode `json:"codes"`
}

// EnrollStore manages one-time per-device enrollment codes in <dir>/enroll.json
// with mode 0600, sibling enroll.lock (0600) under flock, and atomic replace.
type EnrollStore struct {
	dir   string
	clock Clock
}

// NewEnrollStore prepares dir (0700) and returns an EnrollStore.
// A nil clock defaults to the system UTC clock.
func NewEnrollStore(dir string, clock Clock) (*EnrollStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("devices: create enroll dir: %w", err)
	}
	if clock == nil {
		clock = realClock{}
	}
	return &EnrollStore{
		dir:   dir,
		clock: clock,
	}, nil
}

// Path returns the enroll.json path.
func (s *EnrollStore) Path() string {
	return filepath.Join(s.dir, enrollFileName)
}

func (s *EnrollStore) load() (EnrollDoc, error) {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return EnrollDoc{}, nil
	}
	if err != nil {
		return EnrollDoc{}, fmt.Errorf("devices: read enroll store: %w", err)
	}
	var doc EnrollDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return EnrollDoc{}, fmt.Errorf("%w: %s: %w — inspect or remove it and retry; refusing to silently reset", ErrCorruptEnrollState, s.Path(), err)
	}
	return doc, nil
}

func (s *EnrollStore) save(doc EnrollDoc) error {
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("devices: encode enroll store: %w", err)
	}
	return writeAtomic(s.dir, enrollFileName, data)
}

// lock takes syscall.Flock LOCK_EX over sibling enroll.lock (0600).
// Uses os.OpenFile with explicit 0600 (never os.Create, which respects umask).
func (s *EnrollStore) lock() (func(), error) {
	lockPath := filepath.Join(s.dir, enrollLockName)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("devices: open enroll lock: %w", err)
	}
	_ = os.Chmod(lockPath, 0o600)
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("devices: flock enroll: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// MintCode creates a one-time device enrollment code valid for ttl and returns
// the plaintext once. Only the SHA-256 hash is persisted to disk.
func (s *EnrollStore) MintCode(now time.Time, ttl time.Duration) (string, error) {
	if now.IsZero() {
		now = s.clock.Now()
	}
	if ttl <= 0 {
		ttl = DefaultEnrollTTL
	}
	code, err := randomEnrollCode()
	if err != nil {
		return "", err
	}
	unlock, err := s.lock()
	if err != nil {
		return "", err
	}
	defer unlock()

	doc, err := s.load()
	if err != nil {
		return "", err
	}
	doc.Codes = append(doc.Codes, EnrollCode{
		Hash:    hashEnrollCode(code),
		Created: now.Unix(),
		Expires: now.Add(ttl).Unix(),
	})
	if err := s.save(doc); err != nil {
		return "", err
	}
	return code, nil
}

// Redeem consumes code once if it matches a non-expired entry.
// All stored entries are compared in constant time before deciding.
// After deleting the entry atomically, Redeem read-back verifies the store.
func (s *EnrollStore) Redeem(code string, now time.Time) (bool, error) {
	if now.IsZero() {
		now = s.clock.Now()
	}
	unlock, err := s.lock()
	if err != nil {
		return false, err
	}
	defer unlock()

	doc, err := s.load()
	if err != nil {
		return false, err
	}

	want := hashEnrollCode(code)
	idx := -1
	for i, c := range doc.Codes {
		if subtle.ConstantTimeCompare([]byte(c.Hash), []byte(want)) == 1 && idx == -1 {
			idx = i
		}
	}
	if idx == -1 || now.Unix() >= doc.Codes[idx].Expires {
		return false, nil
	}

	doc.Codes = append(doc.Codes[:idx], doc.Codes[idx+1:]...)
	if err := s.save(doc); err != nil {
		return false, err
	}

	after, err := s.load()
	if err != nil {
		return false, err
	}
	if len(after.Codes) != len(doc.Codes) {
		return false, errors.New("devices: enroll.json changed concurrently during enrollment; retry")
	}
	for _, c := range after.Codes {
		if subtle.ConstantTimeCompare([]byte(c.Hash), []byte(want)) == 1 {
			return false, errors.New("devices: enroll.json changed concurrently during enrollment; retry")
		}
	}
	return true, nil
}

// RevokeAll clears all stored enrollment codes and returns the count of
// non-expired codes that were pending at now.
func (s *EnrollStore) RevokeAll(now time.Time) (int, error) {
	if now.IsZero() {
		now = s.clock.Now()
	}
	unlock, err := s.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()

	doc, err := s.load()
	if err != nil {
		return 0, err
	}
	nowUnix := now.Unix()
	live := 0
	for _, c := range doc.Codes {
		if nowUnix < c.Expires {
			live++
		}
	}
	if len(doc.Codes) == 0 {
		return 0, nil
	}
	doc.Codes = nil
	if err := s.save(doc); err != nil {
		return 0, err
	}
	return live, nil
}

// Pending returns the expiry timestamps of all non-expired enrollment codes at
// now, ordered oldest-created first. No hashes or plaintext codes are returned.
func (s *EnrollStore) Pending(now time.Time) ([]time.Time, error) {
	if now.IsZero() {
		now = s.clock.Now()
	}
	doc, err := s.load()
	if err != nil {
		return nil, err
	}
	nowUnix := now.Unix()
	var live []EnrollCode
	for _, c := range doc.Codes {
		if nowUnix < c.Expires {
			live = append(live, c)
		}
	}
	sort.SliceStable(live, func(i, j int) bool {
		return live[i].Created < live[j].Created
	})
	out := make([]time.Time, len(live))
	for i, c := range live {
		out[i] = time.Unix(c.Expires, 0).UTC()
	}
	return out, nil
}

// randomEnrollCode draws 20 base62 characters from crypto/rand with rejection
// sampling so every alphabet character is uniformly distributed.
func randomEnrollCode() (string, error) {
	out := make([]byte, enrollCodeLen)
	var buf [1]byte
	const limit = byte(256 - (256 % len(enrollCodeAlphabet)))
	for i := range out {
		for {
			if _, err := rand.Read(buf[:]); err != nil {
				return "", fmt.Errorf("devices: rand: %w", err)
			}
			if buf[0] < limit {
				out[i] = enrollCodeAlphabet[int(buf[0])%len(enrollCodeAlphabet)]
				break
			}
		}
	}
	return enrollCodePrefix + string(out), nil
}

func hashEnrollCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}
