package nuntius

import (
	"strings"
	"testing"
	"time"
)

// Security: an update with no sender must never bind the empty string
// as owner — the gate refuses the attempt and Redeem refuses the bind.
func TestEmptySenderNeverPairs(t *testing.T) {
	env := Envelope{FromID: "", ChatType: "private", Text: "/pair pair1_ABCDEFGHIJKLMNOPQRST"}
	if got := Decide(env, unpairedOwners()); got != ReplyPairingRequired {
		t.Fatalf("empty sender: %v, want ReplyPairingRequired", got)
	}

	dir := t.TempDir()
	store, err := NewPairStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	code, err := store.Mint(time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Redeem("", code, time.Now()); err == nil {
		t.Fatal("Redeem bound an empty user id")
	}
	o, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.OwnerIDs) != 0 {
		t.Fatalf("owners after empty redeem: %v", o.OwnerIDs)
	}
	if len(o.Codes) != 1 {
		t.Fatal("rejected redeem consumed the code")
	}
}

// N3 backstop: a pair1_-shaped token is redacted wherever it appears,
// independent of command shape (leading space, trailing text, prose).
func TestSanitizeShapeIndependent(t *testing.T) {
	code := "pair1_ABCDEFGHIJKLMNOPQRST"
	cases := []string{
		" " + "/pair " + code,
		"/pair " + code + " extra words",
		"hey " + code + " there",
		"/PAIR@" + code,
	}
	for _, in := range cases {
		got := SanitizePayload(in)
		if strings.Contains(got, code) {
			t.Fatalf("code survived sanitize: %q -> %q", in, got)
		}
	}
	// A near-miss token (19 chars) is not a code and stays verbatim.
	near := "pair1_ABCDEFGHIJKLMNOPQRS"
	if got := SanitizePayload(near); got != near {
		t.Fatalf("near-miss altered: %q", got)
	}
}

// Token(nil) honors the doc: nil means os.Getenv (reviewer probe).
func TestTokenNilUsesEnv(t *testing.T) {
	t.Setenv("LARARIUM_TELEGRAM_BOT_TOKEN", "123:secret")
	c := Config{}
	if _, err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	tok, err := c.Token(nil)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "123:secret" {
		t.Fatalf("token %q", tok)
	}
}

// Negative durations floor to the 1s floor / 0 gap, not stay negative.
func TestNormalizeNegativeFloors(t *testing.T) {
	c := Config{
		EditInterval:   -5 * time.Second,
		PairCodeTTL:    -time.Minute,
		PairFailWindow: -time.Minute,
		PairMute:       -time.Hour,
	}
	neg := time.Duration(-1)
	c.PairReplyGap = &neg
	if _, err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.EditInterval < time.Second || c.PairCodeTTL < time.Second ||
		c.PairFailWindow < time.Second || c.PairMute < time.Second {
		t.Fatalf("floors missed: %+v", c)
	}
	if *c.PairReplyGap != 0 {
		t.Fatalf("gap %v, want 0", *c.PairReplyGap)
	}
}

// The durable-inbox gate: if the append fails, the offset must NOT
// advance (a failed write must never look like an ack).
func TestAppendFailureBlocksOffset(t *testing.T) {
	_, in := openTestInbox(t)
	if _, err := in.Append(Record{UpdateID: 1, Kind: KindTurn, Payload: "ok"}); err != nil {
		t.Fatal(err)
	}
	// Break the handle: writes now fail (closed file).
	if err := in.f.Close(); err != nil {
		t.Fatal(err)
	}
	in.f = nil // avoid double-close in cleanup
	// Reopen a broken handle: nil file makes appendLine fail.
	if _, err := in.Append(Record{UpdateID: 2, Kind: KindTurn, Payload: "fail"}); err == nil {
		t.Fatal("append to closed inbox succeeded")
	}
	if in.LastSeenUpdateID() != 1 {
		t.Fatalf("offset advanced past a failed append: %d", in.LastSeenUpdateID())
	}
	if in.Seen(2) {
		t.Fatal("failed append marked as seen")
	}
}

// Orphan tombstones are loud (reviewer: a typo'd id must not silently
// mark nothing done).
func TestMarkDoneOrphanErrors(t *testing.T) {
	_, in := openTestInbox(t)
	if err := in.MarkDone(4242, nil); err == nil {
		t.Fatal("orphan tombstone accepted")
	}
}

// Compaction prunes done records, so the inbox watermark is only a
// hint — the test pins the documented regression window so T11b can
// never mistake it for the getUpdates offset (state.json is §2's).
func TestCompactedWatermarkIsHintOnly(t *testing.T) {
	dir, in := openTestInbox(t)
	for i := range 300 {
		id := int64(10_000 + i)
		if _, err := in.Append(Record{UpdateID: id, Kind: KindTurn, Payload: strings.Repeat("y", 4096)}); err != nil {
			t.Fatal(err)
		}
		if err := in.MarkDone(id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := in.Append(Record{UpdateID: 20_000, Kind: KindTurn, Payload: "live"}); err != nil {
		t.Fatal(err)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	in2, err := OpenInbox(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer in2.Close()
	// After compaction only the live record survives: the hint sits at
	// 20000 here, but with zero survivors it would sit at 0 — proof it
	// is not the authoritative offset.
	if in2.LastSeenUpdateID() != 20_000 {
		t.Fatalf("hint %d", in2.LastSeenUpdateID())
	}
	recs, err := in2.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Payload != "live" {
		t.Fatalf("replay: %+v", recs)
	}
	// The authoritative offset lives in state.json and is untouched by
	// compaction (independent file, §2).
	sf, err := NewStateFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := sf.Save(State{Offset: 30_000, ActiveSession: "main"}); err != nil {
		t.Fatal(err)
	}
	st, err := sf.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Offset != 30_000 {
		t.Fatal("state offset lost")
	}
}
