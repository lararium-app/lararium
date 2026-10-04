package nuntius

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
	"strings"
	"time"
)

// ownersFileName is <hearth>/nuntius/owners.json (spec §3): the allowlist
// and the pending pairing codes, SHA-256-hashed at rest.
const ownersFileName = "owners.json"

// ownersMode is 0600 per §3/N3; the directory is 0700.
const ownersMode os.FileMode = 0o600

// pairCodeAlphabet is base62 (spec §3: `pair1_<20 base62>`).
const pairCodeAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// pairCodeLen is the random part of a minted code.
const pairCodeLen = 20

// pairCodePrefix marks a pairing code (spec §3).
const pairCodePrefix = "pair1_"

// ErrCorruptState is the refuse-to-start sentinel for corrupt nuntius
// state files (N8): the bridge refuses to start with a remedy line; it
// never silently re-pairs or resets.
var ErrCorruptState = errors.New("corrupt nuntius state")

// RemedyN8 is the remedy line appended to corrupt-state errors (N8).
const RemedyN8 = "nuntius: state file is corrupt — inspect or remove it and restart; refusing to start rather than silently reset"

// OwnerCode is one pending pairing code, stored hashed (spec §3).
// created/expires are Unix seconds (int64: no float round-trip drift).
type OwnerCode struct {
	Hash    string `json:"hash"`
	Created int64  `json:"created"`
	Expires int64  `json:"expires"`
}

// Owners is the on-disk shape of owners.json (spec §3). Owner entries
// are Telegram user IDs as decimal strings; the list shape keeps v2
// multi-owner additive.
type Owners struct {
	OwnerIDs []string    `json:"owner_ids"`
	Codes    []OwnerCode `json:"codes"`
}

// PairStore owns owners.json. Freshness doctrine (§3): every operation
// re-reads the file from disk — no cached allowlist, no signals. The
// CLI (pair create / unpair / revoke) writes this file concurrently by
// design; Redeem therefore read-back-verifies its write (see Redeem).
type PairStore struct {
	dir string
}

// NewPairStore prepares <dir> (0700) and returns its store.
func NewPairStore(dir string) (*PairStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("nuntius: create state dir: %w", err)
	}
	return &PairStore{dir: dir}, nil
}

// Path is the owners.json path.
func (s *PairStore) Path() string { return filepath.Join(s.dir, ownersFileName) }

// Load re-reads owners.json. A missing file is the unpaired initial
// state; corrupt JSON is ErrCorruptState with the N8 remedy (never a
// silent reset).
func (s *PairStore) Load() (Owners, error) {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return Owners{}, nil
	}
	if err != nil {
		return Owners{}, fmt.Errorf("nuntius: read owners: %w", err)
	}
	var o Owners
	if err := json.Unmarshal(data, &o); err != nil {
		return Owners{}, fmt.Errorf("%w: %s: %w — %s", ErrCorruptState, s.Path(), err, RemedyN8)
	}
	return o, nil
}

// save writes owners.json atomically (temp + rename, 0600, fsync).
func (s *PairStore) save(o Owners) error {
	data, err := json.Marshal(o)
	if err != nil {
		return fmt.Errorf("nuntius: encode owners: %w", err)
	}
	return atomicWrite(s.dir, ownersFileName, data, ownersMode)
}

// Mint creates a one-time pairing code valid for ttl and returns the
// plaintext exactly once (spec §3: printed once, hashed at rest).
func (s *PairStore) Mint(now time.Time, ttl time.Duration) (string, error) {
	code, err := randomPairCode()
	if err != nil {
		return "", err
	}
	o, err := s.Load()
	if err != nil {
		return "", err
	}
	o.Codes = append(o.Codes, OwnerCode{
		Hash:    hashPairCode(code),
		Created: now.Unix(),
		Expires: now.Add(ttl).Unix(),
	})
	if err := s.save(o); err != nil {
		return "", err
	}
	return code, nil
}

// RedeemResult classifies a /pair attempt (spec §3).
type RedeemResult int

const (
	// RedeemPaired means a correct code — sender is now the owner.
	RedeemPaired RedeemResult = iota
	// RedeemInvalid means a wrong or expired code (generic reply, no
	// hints).
	RedeemInvalid
	// RedeemAlready means a code was presented while already paired;
	// §3 routes this to the already-paired / non-owner paths, never
	// the code path.
	RedeemAlready
)

// Redeem evaluates /pair <code> for one sender. A correct code binds
// that sender as the single owner, marks the code used (deleted), and
// reports RedeemPaired. Codes are compared by SHA-256 hash in constant
// time; expiry is evaluated here, never by a sweeper.
func (s *PairStore) Redeem(userID, code string, now time.Time) (RedeemResult, error) {
	// A pairing binds a sender; an update with no sender is malformed
	// and must never bind the empty string as owner (that would lock
	// the real owner out of a bot "paired" to nobody).
	if strings.TrimSpace(userID) == "" {
		return RedeemInvalid, errors.New("nuntius: cannot pair an empty user id")
	}
	o, err := s.Load()
	if err != nil {
		return RedeemInvalid, err
	}
	if len(o.OwnerIDs) > 0 {
		return RedeemAlready, nil
	}
	want := hashPairCode(code)
	idx := -1
	for i, c := range o.Codes {
		if subtle.ConstantTimeCompare([]byte(c.Hash), []byte(want)) == 1 {
			idx = i
			break
		}
	}
	if idx == -1 || now.Unix() >= o.Codes[idx].Expires {
		return RedeemInvalid, nil
	}
	// One-time: the code is removed in the same atomic write that
	// binds the owner — redemption is all-or-nothing across crashes.
	o.Codes = append(o.Codes[:idx], o.Codes[idx+1:]...)
	o.OwnerIDs = []string{userID}
	if err := s.save(o); err != nil {
		return RedeemInvalid, err
	}
	// Read-back verification: the CLI writes this file concurrently by
	// design (§3 freshness doctrine). If another writer raced our
	// rename, the file no longer holds what we just wrote — surface it
	// instead of silently reverting the bind or resurrecting the code.
	after, err := s.Load()
	if err != nil {
		return RedeemInvalid, err
	}
	if len(after.OwnerIDs) != 1 || after.OwnerIDs[0] != userID || len(after.Codes) != len(o.Codes) {
		return RedeemInvalid, errors.New("nuntius: owners.json changed concurrently during pairing; retry")
	}
	return RedeemPaired, nil
}

// RevokeAll clears pending codes (hearthd pair revoke --all, §3).
func (s *PairStore) RevokeAll() error {
	o, err := s.Load()
	if err != nil {
		return err
	}
	o.Codes = nil
	return s.save(o)
}

// Unpair removes an owner (hearthd pair unpair <user_id>, §3).
func (s *PairStore) Unpair(userID string) error {
	o, err := s.Load()
	if err != nil {
		return err
	}
	kept := o.OwnerIDs[:0]
	for _, id := range o.OwnerIDs {
		if id != userID {
			kept = append(kept, id)
		}
	}
	o.OwnerIDs = kept
	return s.save(o)
}

// randomPairCode draws 20 base62 characters from crypto/rand with
// rejection sampling (uniform over the alphabet).
func randomPairCode() (string, error) {
	out := make([]byte, pairCodeLen)
	var buf [1]byte
	// 256 mod 62 = 8: reject the tail so every alphabet char is
	// equally likely.
	const limit = byte(256 - (256 % len(pairCodeAlphabet)))
	for i := range out {
		for {
			if _, err := rand.Read(buf[:]); err != nil {
				return "", fmt.Errorf("nuntius: rand: %w", err)
			}
			if buf[0] < limit {
				out[i] = pairCodeAlphabet[int(buf[0])%len(pairCodeAlphabet)]
				break
			}
		}
	}
	return pairCodePrefix + string(out), nil
}

func hashPairCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// TokenHashPrefix is the 8-hex SHA-256 prefix startup logs for "which
// token is loaded" (spec §4, N4) — the token itself is never logged.
func TokenHashPrefix(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:4])
}
