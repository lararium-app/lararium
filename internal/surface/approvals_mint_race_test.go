package surface

// issue #183 regression tests: the connector trust mint must not outlive a
// lost resolve, and a failed mint must leave the card exactly pending.

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/router"
)

func raceStub() *stubProvider {
	return &stubProvider{
		name: "stub",
		caps: router.Caps{SupportsTools: true, ContextLength: 100000, Source: "stub"},
		replies: func(n int, msgs []router.Message) (*router.Completion, error) {
			return &router.Completion{Text: "done", Model: "stub"}, nil
		},
	}
}

var errRaceMint = errors.New("simulated mint failure")

// A deny that arrives while an allow+connector resolve is mid-mint must get
// 410 — it can never settle the card denied while the rule gets minted.
func TestConnectorMintRaceDenyCannotWinMidMint(t *testing.T) {
	hub := setupHub(t, raceStub())
	id, ch := hub.ap.Register("main", "mcp__tool__race", `{}`, func(string) {})

	entered := make(chan struct{})
	release := make(chan struct{})
	var mints int32
	hub.SetConnectorTrustHook(func(string) error {
		atomic.AddInt32(&mints, 1)
		close(entered)
		<-release
		return nil
	})

	codeA := make(chan int, 1)
	go func() {
		code, _ := hub.ap.ResolveScopeVia("main", id, true, "cli", "phone", "connector")
		codeA <- code
	}()

	<-entered // mint in flight; card claimed but verdict not released yet

	if code, _ := hub.ap.ResolveScopeVia("main", id, false, "cli", "web", ""); code != 410 {
		t.Fatalf("deny mid-mint status = %d, want 410", code)
	}

	close(release)
	select {
	case code := <-codeA:
		if code != 200 {
			t.Fatalf("winning allow status = %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("allow resolve never returned after mint release")
	}

	select {
	case allow := <-ch:
		if !allow {
			t.Fatal("parked waiter got deny, want allow")
		}
	case <-time.After(time.Second):
		t.Fatal("parked waiter never released")
	}
	if n := atomic.LoadInt32(&mints); n != 1 {
		t.Fatalf("mints = %d, want 1", n)
	}
}

// A failed mint must roll the card back to pending: the waiter stays parked
// and a later resolve (allow or deny) still works normally.
func TestConnectorMintFailureRollsBackToPending(t *testing.T) {
	hub := setupHub(t, raceStub())
	id, ch := hub.ap.Register("main", "mcp__tool__rollback", `{}`, func(string) {})

	hub.SetConnectorTrustHook(func(string) error { return errRaceMint })

	code, err := hub.ap.ResolveScopeVia("main", id, true, "cli", "phone", "connector")
	if code != 500 {
		t.Fatalf("failed mint status = %d (err %v), want 500", code, err)
	}
	if !strings.Contains(err.Error(), "simulated") {
		t.Fatalf("error = %v, want simulated mint failure", err)
	}
	select {
	case <-ch:
		t.Fatal("waiter released despite mint failure")
	case <-time.After(100 * time.Millisecond):
	}

	// Card must be pending again: a plain deny now settles it.
	if code, _ := hub.ap.ResolveScopeVia("main", id, false, "cli", "web", ""); code != 200 {
		t.Fatalf("deny after rollback status = %d, want 200", code)
	}
	select {
	case allow := <-ch:
		if allow {
			t.Fatal("parked waiter got allow, want deny")
		}
	case <-time.After(time.Second):
		t.Fatal("deny after rollback did not release waiter")
	}
}
