package nuntius

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustMint(t *testing.T, s *PairStore, now time.Time, ttl time.Duration) string {
	t.Helper()
	code, err := s.Mint(now, ttl)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return code
}

// V1: pairing flow — mint prints once, /pair binds owner, second /pair
// answers already-paired, owners.json holds the hash not the code,
// mode 0600.
func TestPairingFlow(t *testing.T) {
	dir := t.TempDir()
	store, err := NewPairStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	code := mustMint(t, store, now, 15*time.Minute)
	if !strings.HasPrefix(code, pairCodePrefix) {
		t.Fatalf("code %q lacks prefix %q", code, pairCodePrefix)
	}
	if len(code) != len(pairCodePrefix)+pairCodeLen {
		t.Fatalf("code %q wrong length", code)
	}

	// Wrong code → invalid.
	res, err := store.Redeem("999", "pair1_wrongwrongwrongwron", now)
	if err != nil {
		t.Fatal(err)
	}
	if res != RedeemInvalid {
		t.Fatalf("wrong code: got %v, want RedeemInvalid", res)
	}

	// Correct code binds the sender as sole owner.
	res, err = store.Redeem("42", code, now)
	if err != nil {
		t.Fatal(err)
	}
	if res != RedeemPaired {
		t.Fatalf("redeem: got %v, want RedeemPaired", res)
	}
	o, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.OwnerIDs) != 1 || o.OwnerIDs[0] != "42" {
		t.Fatalf("owners after pairing: %v", o.OwnerIDs)
	}
	if len(o.Codes) != 0 {
		t.Fatalf("used code still present: %v", o.Codes)
	}

	// Once paired, no code path is reachable (§3).
	res, err = store.Redeem("42", code, now)
	if err != nil {
		t.Fatal(err)
	}
	if res != RedeemAlready {
		t.Fatalf("paired redeem: got %v, want RedeemAlready", res)
	}

	// At rest: hash, not plaintext; mode 0600.
	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), code) {
		t.Fatal("plaintext code found in owners.json")
	}
	st, err := os.Stat(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("owners.json mode %v, want 0600", st.Mode().Perm())
	}
}

// V2: expiry — with a 1s TTL, a code minted 2s ago answers invalid.
func TestPairCodeExpiry(t *testing.T) {
	dir := t.TempDir()
	store, err := NewPairStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	code := mustMint(t, store, now, time.Second)

	res, err := store.Redeem("42", code, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if res != RedeemInvalid {
		t.Fatalf("expired code: got %v, want RedeemInvalid", res)
	}
}

// V2: mute — 5 failed redemptions inside the window mute replies;
// logging (the fail records themselves) continues.
func TestPairMute(t *testing.T) {
	base := time.Now()
	clock := base
	m := NewMuter(time.Minute, 30*time.Second, 5, func() time.Time { return clock })

	for i := range 4 {
		m.RecordFail("7")
		if m.Muted("7") {
			t.Fatalf("muted after %d fails, want 5", i+1)
		}
	}
	m.RecordFail("7")
	if !m.Muted("7") {
		t.Fatal("not muted after 5 fails")
	}
	// Mute expires.
	clock = clock.Add(31 * time.Second)
	if m.Muted("7") {
		t.Fatal("mute outlived pair_mute")
	}
}

// V2: fails outside the window do not accumulate.
func TestPairMuteWindow(t *testing.T) {
	base := time.Now()
	clock := base
	m := NewMuter(time.Minute, time.Hour, 5, func() time.Time { return clock })

	for i := range 4 {
		m.RecordFail("7")
		clock = clock.Add(90 * time.Second) // each fail ages the previous out
		_ = i
	}
	m.RecordFail("7")
	if m.Muted("7") {
		t.Fatal("muted with all-but-one fail outside window")
	}
}

// Reply limiter: one reply per user per gap; gap 0 disables (CI).
func TestReplyLimiter(t *testing.T) {
	base := time.Now()
	clock := base
	l := NewLimiter(60*time.Second, func() time.Time { return clock })

	if !l.Allow("7") {
		t.Fatal("first reply should be allowed")
	}
	if l.Allow("7") {
		t.Fatal("second reply inside gap should be dropped")
	}
	if !l.Allow("8") {
		t.Fatal("other user unaffected")
	}
	clock = clock.Add(61 * time.Second)
	if !l.Allow("7") {
		t.Fatal("reply after gap should be allowed")
	}

	gap0 := NewLimiter(0, nil)
	for range 3 {
		if !gap0.Allow("7") {
			t.Fatal("gap 0 must allow every reply")
		}
	}
}

// N3: minted codes are uniform-ish base62 and unique across mints.
func TestMintUniqueness(t *testing.T) {
	dir := t.TempDir()
	store, err := NewPairStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	now := time.Now()
	for range 50 {
		code := mustMint(t, store, now, time.Minute)
		if seen[code] {
			t.Fatalf("duplicate mint: %s", code)
		}
		seen[code] = true
		body := strings.TrimPrefix(code, pairCodePrefix)
		if len(body) != pairCodeLen {
			t.Fatalf("body %q wrong length", body)
		}
		for _, ch := range body {
			if !strings.ContainsRune(pairCodeAlphabet, ch) {
				t.Fatalf("char %q outside base62 alphabet", ch)
			}
		}
	}
}

// RevokeAll and Unpair per §3.
func TestPairCLIPaths(t *testing.T) {
	dir := t.TempDir()
	store, err := NewPairStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	code := mustMint(t, store, now, time.Minute)

	if err := store.RevokeAll(); err != nil {
		t.Fatal(err)
	}
	res, err := store.Redeem("42", code, now)
	if err != nil {
		t.Fatal(err)
	}
	if res != RedeemInvalid {
		t.Fatal("revoked code still redeems")
	}

	// Pair, then unpair.
	if _, err := store.Redeem("42", mustMint(t, store, now, time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if err := store.Unpair("42"); err != nil {
		t.Fatal(err)
	}
	o, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.OwnerIDs) != 0 {
		t.Fatalf("owners after unpair: %v", o.OwnerIDs)
	}
}

// N8: corrupt owners.json refuses to load with the remedy line; never a
// silent reset to unpaired.
func TestCorruptOwnersRefuses(t *testing.T) {
	dir := t.TempDir()
	store, err := NewPairStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = store.Load()
	if !errors.Is(err, ErrCorruptState) {
		t.Fatalf("corrupt owners: err %v, want ErrCorruptState", err)
	}
	if !strings.Contains(err.Error(), RemedyN8) {
		t.Fatalf("error lacks remedy: %v", err)
	}
}

// N8: corrupt state.json likewise.
func TestCorruptStateRefuses(t *testing.T) {
	dir := t.TempDir()
	sf, err := NewStateFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sf.Path(), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = sf.Load()
	if !errors.Is(err, ErrCorruptState) {
		t.Fatalf("corrupt state: err %v, want ErrCorruptState", err)
	}
}

// State round-trip: offset + active_session, 0600, missing file is the
// fresh-start state.
func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sf, err := NewStateFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := sf.Load()
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Offset != 0 || fresh.ActiveSession != "main" {
		t.Fatalf("fresh state: %+v", fresh)
	}
	if err := sf.Save(State{Offset: 77, ActiveSession: "s_01ABC"}); err != nil {
		t.Fatal(err)
	}
	got, err := sf.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Offset != 77 || got.ActiveSession != "s_01ABC" {
		t.Fatalf("round-trip: %+v", got)
	}
	st, err := os.Stat(sf.Path())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("state.json mode %v", st.Mode().Perm())
	}
}

// Config: defaults, floors, queue_depth errors, strict unknown keys,
// token from env only.
func TestConfigNormalize(t *testing.T) {
	c := Config{}
	warns, err := c.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warns: %v", warns)
	}
	if c.BotTokenEnv != DefaultBotTokenEnv {
		t.Fatalf("token env: %s", c.BotTokenEnv)
	}
	if c.EditInterval != 2*time.Second || c.ApprovalTimeout != 5*time.Minute {
		t.Fatalf("defaults: %+v", c)
	}
	if c.QueueDepth == nil || *c.QueueDepth != 1 {
		t.Fatal("queue_depth default should be 1")
	}
	if c.PairCodeTTL != 15*time.Minute || c.PairFailWindow != 10*time.Minute ||
		c.PairMute != time.Hour || *c.PairReplyGap != 60*time.Second {
		t.Fatalf("pairing defaults: %+v", c)
	}

	// edit_interval below floor normalizes up WITH a warning.
	c2 := Config{EditInterval: 500 * time.Millisecond}
	warns, err = c2.Normalize()
	if err != nil || len(warns) != 1 || c2.EditInterval != time.Second {
		t.Fatalf("edit floor: warns=%v err=%v val=%v", warns, err, c2.EditInterval)
	}

	// queue_depth 2 is an error.
	bad := 2
	if _, err := (&Config{QueueDepth: &bad}).Normalize(); err == nil {
		t.Fatal("queue_depth 2 accepted")
	}

	// Explicit 0 survives (V10).
	zero := 0
	gap0 := time.Duration(0)
	c3 := Config{QueueDepth: &zero, PairReplyGap: &gap0}
	if _, err := c3.Normalize(); err != nil {
		t.Fatal(err)
	}
	if *c3.QueueDepth != 0 || *c3.PairReplyGap != 0 {
		t.Fatalf("explicit zeros not preserved: %+v", c3)
	}

	// Strict config: unknown key is an error (§4).
	if _, err := ParseConfig([]byte("enabled: true\nbogus_key: 1\n")); err == nil {
		t.Fatal("unknown key accepted")
	}
	ok, err := ParseConfig([]byte("enabled: true\npair_code_ttl: 1s\n"))
	if err != nil || !ok.Enabled || ok.PairCodeTTL != time.Second {
		t.Fatalf("parse: %+v %v", ok, err)
	}
}

func TestTokenFromEnv(t *testing.T) {
	c := Config{}
	if _, err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Token(func(string) string { return "" }); err == nil {
		t.Fatal("empty token accepted")
	}
	tok, err := c.Token(func(string) string { return "123:secret" })
	if err != nil || tok != "123:secret" {
		t.Fatalf("token: %q %v", tok, err)
	}
	// N4: startup logs only the hash prefix.
	prefix := TokenHashPrefix("123:secret")
	if len(prefix) != 8 || strings.Contains(prefix, "secret") {
		t.Fatalf("prefix leak: %q", prefix)
	}
}

// owners.json survives a JSON round-trip with the documented shape.
func TestOwnersShape(t *testing.T) {
	dir := t.TempDir()
	store, err := NewPairStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Mint(time.Unix(1000, 0), time.Minute); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["owner_ids"]; !ok {
		t.Fatal("missing owner_ids key")
	}
	if _, ok := raw["codes"]; !ok {
		t.Fatal("missing codes key")
	}
	var codes []OwnerCode
	if err := json.Unmarshal(raw["codes"], &codes); err != nil {
		t.Fatal(err)
	}
	if len(codes) != 1 || codes[0].Expires <= codes[0].Created {
		t.Fatalf("code window: %+v", codes[0])
	}
	// Hash is 64 hex chars (SHA-256), not the plaintext.
	if len(codes[0].Hash) != 64 || strings.HasPrefix(codes[0].Hash, pairCodePrefix) {
		t.Fatalf("stored hash suspicious: %q", codes[0].Hash)
	}
}

// The state dir is 0700 (spec §2).
func TestStateDirMode(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "nuntius")
	if _, err := NewPairStore(dir); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode %v, want 0700", st.Mode().Perm())
	}
}
