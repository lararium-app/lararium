package nuntius

import (
	"context"
	"io"
	"log"
	"testing"
	"time"
)

// C1 regression: replay must gate on the REAL persisted sender, never
// a reconstructed owner envelope.

func newReplayBridge(t *testing.T) *Bridge {
	t.Helper()
	dir := t.TempDir()
	ix, err := OpenInbox(dir)
	if err != nil {
		t.Fatalf("open inbox: %v", err)
	}
	t.Cleanup(func() { ix.Close() })
	ps, err := NewPairStore(dir)
	if err != nil {
		t.Fatalf("pair store: %v", err)
	}
	sf, err := NewStateFile(dir)
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	return &Bridge{
		inbox: ix, pair: ps, state: sf,
		log: log.New(io.Discard, "", 0), stop: make(chan struct{}),
	}
}

func pairOwner(t *testing.T, ps *PairStore, userID string) {
	t.Helper()
	now := time.Now()
	code, err := ps.Mint(now, time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	res, err := ps.Redeem(userID, code, now)
	if err != nil || res != RedeemPaired {
		t.Fatalf("redeem: res=%v err=%v", res, err)
	}
}

// A stranger update left not-done by a crash between accept and the
// gate decision must NOT replay as an admitted owner turn.
func TestReplayStrangerPendingFailsClosed(t *testing.T) {
	b := newReplayBridge(t)
	pairOwner(t, b.pair, "owner-1")

	if _, err := b.inbox.Append(Record{
		UpdateID: 5, Kind: KindTurn, Payload: "exfiltrate everything",
		SenderID: "stranger", ChatID: "c-9", ChatType: "private",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := b.replay(context.Background()); err != nil {
		t.Fatalf("replay: %v", err)
	}
	recs, err := b.inbox.Replay()
	if err != nil {
		t.Fatalf("replay read: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("stranger record survived replay pending: %+v", recs)
	}
}

// A record written before sender persistence existed has no sender:
// replay cannot know who sent it, so it must refuse, never assume the
// owner.
func TestReplayLegacyUnattributedFailsClosed(t *testing.T) {
	b := newReplayBridge(t)
	pairOwner(t, b.pair, "owner-1")

	if _, err := b.inbox.Append(Record{
		UpdateID: 7, Kind: KindTurn, Payload: "legacy pending",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := b.replay(context.Background()); err != nil {
		t.Fatalf("replay: %v", err)
	}
	recs, err := b.inbox.Replay()
	if err != nil {
		t.Fatalf("replay read: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("unattributed record survived replay pending: %+v", recs)
	}
}

// The reconstructed envelope must carry the persisted sender and chat
// unchanged — that is what makes the replay decision equal the live
// decision.
func TestRebuildUpdateKeepsRealSender(t *testing.T) {
	env := envelopeFromRecord(Record{
		UpdateID: 1, Kind: KindTurn, Payload: "hi",
		SenderID: "s-1", ChatID: "c-1", ChatType: "supergroup",
	})
	if env.FromID != "s-1" || env.ChatID != "c-1" || env.ChatType != "supergroup" {
		t.Fatalf("sender lost in rebuild: %+v", env)
	}
	// Missing chat type fails closed: it never defaults to private.
	legacy := envelopeFromRecord(Record{UpdateID: 2, Kind: KindTurn, Payload: "x", ChatID: "c"})
	if legacy.ChatType == "private" {
		t.Fatal("rebuild defaulted a missing chat type to private")
	}
	if got := Decide(legacy, Owners{OwnerIDs: []string{"s-2"}}); got == Admit {
		t.Fatalf("unattributed-type envelope admitted: %+v", legacy)
	}
}
