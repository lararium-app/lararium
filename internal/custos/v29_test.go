package custos_test

import (
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/surface"
)

// V29: park-path fan-out must never block on parkMgr.mu.
//
// registerParkFlow / parkAndServeEgress / the worker ask path all hold
// parkMgr.mu while calling hub.RegisterCustos (register-inside-the-lock
// is the anti-orphan-card invariant). RegisterCustos fans the pending
// event out synchronously on the caller's goroutine, so the door server's
// buildCardWire enrichment lookups MUST be non-blocking (TryLock): with a
// plain Lock the very first parked custody card self-deadlocked the whole
// park manager (dogfood 2026-10-08, SIGQUIT-confirmed).
func TestV29ParkFanoutNoDeadlock(t *testing.T) {
	dir := mustShortDir(t)
	hub := surface.NewApprovalHub(5 * time.Second)
	p := custos.NewProxy(nil, hub, nil)

	doorSrv, err := custos.StartDoorServer(dir, hub, custos.WithProxy(p))
	if err != nil {
		t.Fatalf("start door server: %v", err)
	}
	defer doorSrv.Close()

	// Attach a door client BEFORE the register so the fan-out frames it.
	sockPath, _ := custos.DoorPaths(dir)
	doorToken, err := custos.ReadDoorToken(dir)
	if err != nil {
		t.Fatalf("read door token: %v", err)
	}
	doorConn, err := custos.DialDoor(sockPath, doorToken, "door_v29")
	if err != nil {
		t.Fatalf("dial door: %v", err)
	}
	defer doorConn.Close()

	// Hold the proxy park-manager lock exactly as the custody park path
	// does around RegisterCustos.
	release := custos.LockProxyParkManagerForTest(p)

	registered := make(chan string, 1)
	go func() {
		id, _ := hub.RegisterCustos(
			"cell-v29", "dogfood-test", "example.com:80/",
			"sha256:deadbeef", 5*time.Second, nil)
		registered <- id
	}()

	var cardID string
	select {
	case cardID = <-registered:
	case <-time.After(3 * time.Second):
		release()
		t.Fatal("RegisterCustos deadlocked while park manager lock held: " +
			"fan-out is blocking on parkMgr.mu (buildCardWire must TryLock)")
	}
	release()

	// The CARD frame must still reach the attached door client, with the
	// argsSummary fallback (the flow is by ordering not yet in the park
	// map on this path, so dest comes from the register call).
	frame, err := doorConn.NextFrame(3 * time.Second)
	if err != nil {
		t.Fatalf("next frame: %v", err)
	}
	if !strings.HasPrefix(frame, "CARD ") {
		t.Fatalf("want CARD frame, got %q", frame)
	}
	if !strings.Contains(frame, `"dest":"example.com:80/"`) {
		t.Fatalf("CARD dest fallback missing: %q", frame)
	}
	if !strings.Contains(frame, cardID) {
		t.Fatalf("CARD id mismatch, want %s in %q", cardID, frame)
	}

	// Park manager must still be operable: acquiring its lock afterwards
	// must succeed (in the live dogfood everything downstream hung).
	release2 := custos.LockProxyParkManagerForTest(p)
	release2()
	if n := hub.PendingCount("cell-v29"); n != 1 {
		t.Fatalf("hub pending for cell-v29 = %d, want 1", n)
	}
}
