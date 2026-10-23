package penatus

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// crockfordBase32 characters (no I, L, O, U to avoid confusion).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID generates a k-sortable ULID-ish ID: "s_" + 26 Crockford base32 chars.
// First 10 chars encode ms-epoch; remaining 16 are random.
func NewID() string {
	now := time.Now().UTC()
	ms := now.UnixMilli()

	// Encode ms as 10 chars Crockford base32
	var sb strings.Builder
	sb.WriteString("s_")

	// Millisecond timestamp in base32 (10 chars)
	// Max ms ~ 18446744073709551615 / 32^10 ≈ plenty of room
	msBig := big.NewInt(ms)
	var timeChars [10]byte
	for i := 9; i >= 0; i-- {
		mod := new(big.Int).Mod(msBig, big.NewInt(32))
		timeChars[i] = crockford[mod.Int64()]
		msBig.Div(msBig, big.NewInt(32))
	}
	sb.Write(timeChars[:])

	// 16 random chars
	var randChars [16]byte
	for i := range randChars {
		n, _ := rand.Int(rand.Reader, big.NewInt(32))
		randChars[i] = crockford[n.Int64()]
	}
	sb.Write(randChars[:])

	return sb.String()
}

// maxAutoTitle caps back-filled titles (display width, in runes).
const maxAutoTitle = 64

// SetTitleIfEmpty back-fills the header title of the session stored at
// sessDir, writing only when the header carries no title at all (a nil
// field; an explicit "" is the owner's placeholder, kept verbatim).
// Trims and collapses whitespace in the candidate and caps it at
// maxAutoTitle runes. Reports whether a write happened; an empty
// candidate after trimming never writes.
func SetTitleIfEmpty(sessDir, title string) (bool, error) {
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return false, nil
	}
	if r := []rune(title); len(r) > maxAutoTitle {
		title = string(r[:maxAutoTitle])
	}

	sessionPath := filepath.Join(sessDir, "session.json")
	data, err := os.ReadFile(sessionPath)
	if err != nil {
		return false, err
	}
	var header SessionHeader
	if err := json.Unmarshal(data, &header); err != nil {
		return false, fmt.Errorf("parse session header: %w", err)
	}
	if header.Title != nil {
		return false, nil
	}
	header.Title = &title
	out, err := json.MarshalIndent(header, "", "  ")
	if err != nil {
		return false, err
	}
	tmpPath := sessionPath + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmpPath, sessionPath); err != nil {
		return false, err
	}
	return true, nil
}

// SessionHeader represents the session.json file per spec §2.
type SessionHeader struct {
	Penatus  int     `json:"penatus"`
	ID       string  `json:"id"`
	Created  string  `json:"created"`
	Kind     string  `json:"kind"`
	Title    *string `json:"title"`
	Parent   *string `json:"parent"`
	ModelPin *string `json:"model_pin"`
}

// CreateSession creates a new session directory with session.json.
// kind must be "main" or "side". Returns the Log for the session.
func CreateSession(root, id, kind string) (*Log, error) {
	if kind != "main" && kind != "side" {
		return nil, fmt.Errorf("invalid session kind %q: must be 'main' or 'side'", kind)
	}

	dir := filepath.Join(root, "sessions", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// M7: transcripts are secret-class. MkdirAll keeps a pre-existing
	// dir's older, looser mode, so set it explicitly.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}

	header := SessionHeader{
		Penatus:  1,
		ID:       id,
		Created:  time.Now().UTC().Format(time.RFC3339Nano),
		Kind:     kind,
		Title:    nil,
		Parent:   nil,
		ModelPin: nil,
	}

	data, err := json.MarshalIndent(header, "", "  ")
	if err != nil {
		return nil, err
	}

	// Atomic write: write to temp, then rename
	tmpPath := filepath.Join(dir, "session.json.tmp")
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return nil, err
	}

	finalPath := filepath.Join(dir, "session.json")
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return nil, err
	}

	// Create empty events.jsonl so the session directory is complete.
	eventsPath := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(eventsPath, nil, 0o600); err != nil {
		return nil, err
	}

	return OpenLog(dir)
}
