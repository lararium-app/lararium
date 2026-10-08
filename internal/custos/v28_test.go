package custos_test

import (
	"encoding/json"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/surface"
)

// mustShortDir returns a fresh state dir whose path stays under the
// 104-byte unix-socket sun_path cap even for long test names (t.TempDir()
// under a deep TMPDIR breaks doors.sock binds; same workaround class as
// v27_test.go's /tmp usage).
func mustShortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "lrd") //nolint:usetesting // UDS path cap: t.TempDir() under deep TMPDIR exceeds 104 bytes
	if err != nil {
		t.Fatalf("short tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// TestV28_FanOutDoor verifies CUSTOS-SPEC §6.4b, §10 V28 fan-out door channel (doors.sock).
//
//nolint:maintidx // V-suites are single-spec walkthroughs: one scenario family, sequential subtests, shared harness state.
func TestV28_FanOutDoor(t *testing.T) {
	// Subtest 1: door.token auto-generation and upgrade path
	t.Run("TokenAutoGenerationAndUpgrade", func(t *testing.T) {
		dir := mustShortDir(t)
		sockPath, tokenPath := custos.DoorPaths(dir)

		// 1. Absent token: auto-generated, 64 lowercase hex, mode 0600
		tok1, err := custos.EnsureDoorToken(dir)
		if err != nil {
			t.Fatalf("ensure door token: %v", err)
		}
		if len(tok1) != 64 {
			t.Fatalf("token len = %d, want 64", len(tok1))
		}
		matched, _ := regexp.MatchString(`^[0-9a-f]{64}$`, tok1)
		if !matched {
			t.Fatalf("token %q is not 64 lowercase hex chars", tok1)
		}

		fi, err := os.Stat(tokenPath)
		if err != nil {
			t.Fatalf("stat token file: %v", err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("token file perm = %o, want 0600", fi.Mode().Perm())
		}

		// 2. Present token: untouched
		tok2, err := custos.EnsureDoorToken(dir)
		if err != nil {
			t.Fatalf("ensure door token 2: %v", err)
		}
		if tok2 != tok1 {
			t.Fatalf("second ensure changed token: got %q, want %q", tok2, tok1)
		}

		// 3. Upgrade path: run server twice on the same directory
		hub := surface.NewApprovalHub(5 * time.Second)
		srv1, err := custos.StartDoorServer(dir, hub)
		if err != nil {
			t.Fatalf("start door server 1: %v", err)
		}
		if _, err := os.Stat(sockPath); err != nil {
			t.Fatalf("doors.sock missing: %v", err)
		}
		_ = srv1.Close()

		srv2, err := custos.StartDoorServer(dir, hub)
		if err != nil {
			t.Fatalf("start door server 2 (upgrade path): %v", err)
		}
		defer srv2.Close()

		readTok, err := custos.ReadDoorToken(dir)
		if err != nil {
			t.Fatalf("read door token: %v", err)
		}
		if readTok != tok1 {
			t.Fatalf("token changed across restart: got %q, want %q", readTok, tok1)
		}
	})

	// Subtest 2: bad token and pre-HELLO validation
	t.Run("BadTokenAndPreHello", func(t *testing.T) {
		dir := mustShortDir(t)
		hub := surface.NewApprovalHub(5 * time.Second)
		srv, err := custos.StartDoorServer(dir, hub)
		if err != nil {
			t.Fatalf("start door server: %v", err)
		}
		defer srv.Close()

		sockPath, _ := custos.DoorPaths(dir)
		token, err := custos.ReadDoorToken(dir)
		if err != nil {
			t.Fatalf("read door token: %v", err)
		}

		// A. Verb sent before HELLO -> ERR bad_token + close
		conn1, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatalf("dial 1: %v", err)
		}
		defer conn1.Close()

		_, _ = conn1.Write([]byte("APPROVE a_123 once via web\n"))
		buf := make([]byte, 128)
		n, _ := conn1.Read(buf)
		resp1 := strings.TrimSpace(string(buf[:n]))
		if resp1 != "ERR bad_token" {
			t.Fatalf("verb before HELLO want ERR bad_token, got %q", resp1)
		}
		// Assert connection was closed by server
		_ = conn1.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, err = conn1.Read(buf)
		if err == nil {
			t.Fatalf("expected connection to be closed after ERR bad_token")
		}

		// B. HELLO with invalid token -> ERR bad_token + close
		conn2, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatalf("dial 2: %v", err)
		}
		defer conn2.Close()

		_, _ = conn2.Write([]byte("HELLO badtoken123456789012345678901234567890123456789012345678901234 door1\n"))
		n, _ = conn2.Read(buf)
		resp2 := strings.TrimSpace(string(buf[:n]))
		if resp2 != "ERR bad_token" {
			t.Fatalf("bad token want ERR bad_token, got %q", resp2)
		}
		_ = conn2.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, err = conn2.Read(buf)
		if err == nil {
			t.Fatalf("expected connection closed after bad token")
		}

		// C. HELLO with invalid name -> ERR bad_name + close
		conn3, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatalf("dial 3: %v", err)
		}
		defer conn3.Close()

		_, _ = conn3.Write([]byte("HELLO " + token + " INVALID@NAME!\n"))
		n, _ = conn3.Read(buf)
		resp3 := strings.TrimSpace(string(buf[:n]))
		if resp3 != "ERR bad_name" {
			t.Fatalf("bad name want ERR bad_name, got %q", resp3)
		}
		_ = conn3.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		_, err = conn3.Read(buf)
		if err == nil {
			t.Fatalf("expected connection closed after bad name")
		}
	})

	// Subtest 3: CARDS wire parity between ctl CARDS and HELLO snapshot
	t.Run("CardsWireParity", func(t *testing.T) {
		fake := &fakeOAuth{}
		h := setupWorkerHarness(t, fake)
		h.storeGmail(t)

		if _, err := h.v.Policy().AddCredentialRule("gmail/send", "ask", false, "cli", "cli"); err != nil {
			t.Fatalf("policy add ask: %v", err)
		}

		ctlSrv, err := custos.StartCtlServer(h.v.StateDir(), h.v, nil)
		if err != nil {
			t.Fatalf("start ctl server: %v", err)
		}
		defer ctlSrv.Close()
		ctlSrv.SetWorkers(h.ws)
		ctlSrv.SetHub(h.hub)

		doorSrv, err := custos.StartDoorServer(h.v.StateDir(), h.hub,
			custos.WithVault(h.v),
			custos.WithWorkers(h.ws),
		)
		if err != nil {
			t.Fatalf("start door server: %v", err)
		}
		defer doorSrv.Close()

		ctlClient, err := custos.NewCtlClient(h.v.StateDir(), 5*time.Second)
		if err != nil {
			t.Fatalf("new ctl client: %v", err)
		}

		// Construct parked flow via workers.parkMgr
		cardID, _ := h.hub.RegisterCustos("cell-1", "gmail", "gmail.googleapis.com:send", "to:parity@example.com", 5*time.Second, nil)
		h.ws.RegisterParkForTest(cardID, "cell-1", "gmail", "send", "to:parity@example.com")

		ctlCards, cerr := ctlClient.Cards()
		if cerr != nil || len(ctlCards) != 1 {
			t.Fatalf("ctl cards want 1: %v, %+v", cerr, ctlCards)
		}

		// Dial door server and check HELLO snapshot
		sockPath, _ := custos.DoorPaths(h.v.StateDir())
		doorToken, err := custos.ReadDoorToken(h.v.StateDir())
		if err != nil {
			t.Fatalf("read door token: %v", err)
		}

		doorConn, err := custos.DialDoor(sockPath, doorToken, "door_parity")
		if err != nil {
			t.Fatalf("dial door: %v", err)
		}
		defer doorConn.Close()

		doorCards := doorConn.Cards()
		if len(doorCards) != 1 {
			t.Fatalf("door cards len = %d, want 1", len(doorCards))
		}

		cCard := ctlCards[0]
		dCard := doorCards[0]
		if dCard.ID != cCard.ID || dCard.Cell != cCard.Cell || dCard.Cred != cCard.Cred ||
			dCard.Tool != cCard.Tool || dCard.Dest != cCard.Dest || dCard.Review != cCard.Review {
			t.Fatalf("card wire mismatch:\n ctl:  %+v\n door: %+v", cCard, dCard)
		}

		// Clean up parked call
		_, _ = ctlClient.Approve(cCard.ID)
	})

	// Subtest 4: Attach/replay atomicity (no loss/no dup across HELLO attach window)
	t.Run("AttachReplayAtomicity", func(t *testing.T) {
		dir := mustShortDir(t)
		hub := surface.NewApprovalHub(5 * time.Second)

		doorSrv, err := custos.StartDoorServer(dir, hub)
		if err != nil {
			t.Fatalf("start door server: %v", err)
		}
		defer doorSrv.Close()

		sockPath, _ := custos.DoorPaths(dir)
		token, err := custos.ReadDoorToken(dir)
		if err != nil {
			t.Fatalf("read door token: %v", err)
		}

		// Register card 1 BEFORE attach
		id1, _ := hub.RegisterCustos("cell-1", "cred1", "dest1", "summary1", 5*time.Second, nil)

		// Stalled fanout channel to test atomicity under racing events
		var (
			stalledRegistered = make(chan struct{})
			releaseStall      = make(chan struct{})
			id2               string
		)

		// Goroutine that registers card 2 and blocks in fanout
		go func() {
			id2, _ = hub.RegisterCustos("cell-1", "cred2", "dest2", "summary2", 5*time.Second, nil)
			close(stalledRegistered)
			<-releaseStall
		}()

		<-stalledRegistered

		// Dial door during attach window
		doorConn, err := custos.DialDoor(sockPath, token, "atomicity_door")
		if err != nil {
			t.Fatalf("dial door: %v", err)
		}
		defer doorConn.Close()

		close(releaseStall)

		// Register card 3 AFTER attach
		id3, _ := hub.RegisterCustos("cell-1", "cred3", "dest3", "summary3", 5*time.Second, nil)

		// Check snapshot cards
		snapshotCards := doorConn.Cards()
		snapshotIDs := make(map[string]bool)
		for _, c := range snapshotCards {
			snapshotIDs[c.ID] = true
		}

		if !snapshotIDs[id1] {
			t.Fatalf("card 1 missing from snapshot: %+v", snapshotCards)
		}
		if !snapshotIDs[id2] {
			t.Fatalf("card 2 missing from snapshot: %+v", snapshotCards)
		}
		if snapshotIDs[id3] {
			t.Fatalf("card 3 should not be in snapshot (registered post-attach): %+v", snapshotCards)
		}

		// Read event frame: must receive CARD for card 3 only, NEVER duplicate for card 1 or 2
		frame, err := doorConn.NextFrame(500 * time.Millisecond)
		if err != nil {
			t.Fatalf("read frame for card 3: %v", err)
		}
		if !strings.HasPrefix(frame, "CARD ") {
			t.Fatalf("expected CARD frame, got %q", frame)
		}
		if !strings.Contains(frame, id3) {
			t.Fatalf("expected CARD for %s, got %q", id3, frame)
		}

		// Verify no duplicate frames arrived
		_ = doorConn.RawSend("PING\n")
		// Read with short timeout: should get ERR bad_source for PING, proving no stray CARD was queued
		pingResp, err := doorConn.NextFrame(200 * time.Millisecond)
		if err != nil || pingResp != "ERR bad_source" {
			t.Fatalf("expected ERR bad_source for ping (no dupes), got %q (err: %v)", pingResp, err)
		}
	})

	// Subtest 5: CARD push while attached
	t.Run("CardPushWhileAttached", func(t *testing.T) {
		dir := mustShortDir(t)
		hub := surface.NewApprovalHub(5 * time.Second)
		doorSrv, err := custos.StartDoorServer(dir, hub)
		if err != nil {
			t.Fatalf("start door server: %v", err)
		}
		defer doorSrv.Close()

		sockPath, _ := custos.DoorPaths(dir)
		token, err := custos.ReadDoorToken(dir)
		if err != nil {
			t.Fatalf("read door token: %v", err)
		}

		conn, err := custos.DialDoor(sockPath, token, "listener_door")
		if err != nil {
			t.Fatalf("dial door: %v", err)
		}
		defer conn.Close()

		cardID, _ := hub.RegisterCustos("cell-test", "gmail", "gmail.googleapis.com:send", "to:bob@example.com", 5*time.Second, nil)

		frame, err := conn.NextFrame(500 * time.Millisecond)
		if err != nil {
			t.Fatalf("read CARD frame: %v", err)
		}
		if !strings.HasPrefix(frame, "CARD ") {
			t.Fatalf("expected CARD frame, got %q", frame)
		}

		var card custos.ApprovalCardWire
		rawJSON := strings.TrimPrefix(frame, "CARD ")
		if err := json.Unmarshal([]byte(rawJSON), &card); err != nil {
			t.Fatalf("unmarshal CARD payload: %v", err)
		}
		if card.ID != cardID || card.Cred != "gmail" {
			t.Fatalf("card payload mismatch: %+v", card)
		}
	})

	// Subtest 6: GONE on all terminal states incl. the three cancels with state:"cancelled"
	t.Run("GoneAllTerminalStates", func(t *testing.T) {
		dir := mustShortDir(t)
		hub := surface.NewApprovalHub(10 * time.Second)
		doorSrv, err := custos.StartDoorServer(dir, hub)
		if err != nil {
			t.Fatalf("start door server: %v", err)
		}
		defer doorSrv.Close()

		sockPath, _ := custos.DoorPaths(dir)
		token, err := custos.ReadDoorToken(dir)
		if err != nil {
			t.Fatalf("read door token: %v", err)
		}

		conn, err := custos.DialDoor(sockPath, token, "gone_tester")
		if err != nil {
			t.Fatalf("dial door: %v", err)
		}
		defer conn.Close()

		assertGone := func(t *testing.T, trigger func(id string), wantState, wantReason string) {
			t.Helper()
			cid, _ := hub.RegisterCustos("cell-1", "testcred", "dest", "summary", 5*time.Second, nil)
			// Discard the CARD push
			_, err := conn.NextFrame(500 * time.Millisecond)
			if err != nil {
				t.Fatalf("drain CARD: %v", err)
			}
			trigger(cid)
			frame, err := conn.NextFrame(500 * time.Millisecond)
			if err != nil {
				t.Fatalf("read GONE: %v", err)
			}
			if !strings.HasPrefix(frame, "GONE ") {
				t.Fatalf("expected GONE frame, got %q", frame)
			}
			var gone custos.GoneWire
			if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "GONE ")), &gone); err != nil {
				t.Fatalf("unmarshal GONE: %v", err)
			}
			if gone.ID != cid || gone.State != wantState || gone.Reason != wantReason {
				t.Fatalf("GONE mismatch: got %+v, want ID=%s State=%s Reason=%s", gone, cid, wantState, wantReason)
			}
		}

		// 1. Approved via web -> state: approved, reason: web
		assertGone(t, func(id string) {
			_ = hub.ResolveFromVia("cell-1", id, true, "surface", "web")
		}, "approved", "web")

		// 2. Denied via telegram -> state: denied, reason: telegram
		assertGone(t, func(id string) {
			_ = hub.ResolveFromVia("cell-1", id, false, "surface", "telegram")
		}, "denied", "telegram")

		// 3. Approved via ctl -> state: approved, reason: cli
		assertGone(t, func(id string) {
			_ = hub.ResolveFrom("cell-1", id, true, "ctl")
		}, "approved", "cli")

		// 4. Denied via ctl -> state: denied, reason: cli
		assertGone(t, func(id string) {
			_ = hub.ResolveFrom("cell-1", id, false, "ctl")
		}, "denied", "cli")

		// 5. Timed out -> state: timed_out, reason: timer
		t.Run("Timeout", func(t *testing.T) {
			shortHub := surface.NewApprovalHub(50 * time.Millisecond)
			sShort, serr := custos.StartDoorServer(mustShortDir(t), shortHub)
			if serr != nil {
				t.Fatalf("start short door: %v", serr)
			}
			defer sShort.Close()

			sp, _ := custos.DoorPaths(sShort.StateDir())
			stok, _ := custos.ReadDoorToken(sShort.StateDir())
			cShort, cerr := custos.DialDoor(sp, stok, "short_door")
			if cerr != nil {
				t.Fatalf("dial short: %v", cerr)
			}
			defer cShort.Close()

			cid, _ := shortHub.RegisterCustos("cell-1", "cred", "dest", "summary", 50*time.Millisecond, nil)
			_, _ = cShort.NextFrame(200 * time.Millisecond) // drain CARD
			goneFrame, err := cShort.NextFrame(500 * time.Millisecond)
			if err != nil {
				t.Fatalf("read timeout GONE: %v", err)
			}
			var gone custos.GoneWire
			_ = json.Unmarshal([]byte(strings.TrimPrefix(goneFrame, "GONE ")), &gone)
			if gone.ID != cid || gone.State != "timed_out" || gone.Reason != "timer" {
				t.Fatalf("timeout GONE mismatch: got %+v, want timed_out/timer", gone)
			}
		})

		// 6. Cancel flow_gone -> state: cancelled, reason: flow_gone
		assertGone(t, func(id string) {
			_ = hub.CancelCard(id, "flow_gone", "custos")
		}, "cancelled", "flow_gone")

		// 7. Cancel credential_revoked -> state: cancelled, reason: credential_revoked
		assertGone(t, func(id string) {
			_ = hub.CancelByCredential("testcred", "cell-1")
		}, "cancelled", "credential_revoked")

		// 8. Cancel custos_locked -> state: cancelled, reason: custos_locked
		assertGone(t, func(id string) {
			_ = hub.SettleCustosLocked("custos")
		}, "cancelled", "custos_locked")
	})

	// Subtest 7: Cross-door first-settle-wins, already_answered, and stale_verdict
	t.Run("CrossDoorFirstSettleWins_AlreadyAnswered_StaleVerdict", func(t *testing.T) {
		fake := &fakeOAuth{}
		h := setupWorkerHarness(t, fake)
		h.storeGmail(t)

		doorSrv, err := custos.StartDoorServer(h.v.StateDir(), h.hub,
			custos.WithVault(h.v),
			custos.WithWorkers(h.ws),
		)
		if err != nil {
			t.Fatalf("start door server: %v", err)
		}
		defer doorSrv.Close()

		sockPath, _ := custos.DoorPaths(h.v.StateDir())
		token, err := custos.ReadDoorToken(h.v.StateDir())
		if err != nil {
			t.Fatalf("read token: %v", err)
		}

		doorA, err := custos.DialDoor(sockPath, token, "door_a")
		if err != nil {
			t.Fatalf("dial door A: %v", err)
		}
		defer doorA.Close()

		doorB, err := custos.DialDoor(sockPath, token, "door_b")
		if err != nil {
			t.Fatalf("dial door B: %v", err)
		}
		defer doorB.Close()

		// Wire stale verdict hook to fake
		var (
			staleHookMu    sync.Mutex
			staleHookFired bool
			staleCardID    string
		)
		h.hub.SetStaleVerdictHook(func(id, sessionID, cred, cell, reason string) {
			staleHookMu.Lock()
			defer staleHookMu.Unlock()
			staleHookFired = true
			staleCardID = id
		})

		// 1. Cross-door first-settle-wins: Door A approves, Door B receives already_answered
		cardID, _ := h.hub.RegisterCustos("cell-1", "gmail", "gmail.googleapis.com:send", "summary", 5*time.Second, nil)
		_, _ = doorA.NextFrame(200 * time.Millisecond) // drain CARD
		_, _ = doorB.NextFrame(200 * time.Millisecond) // drain CARD

		respA, err := doorA.Approve(cardID, false, "web")
		if err != nil || respA != "OK" {
			t.Fatalf("door A approve: got %q, err=%v", respA, err)
		}

		respB, err := doorB.Approve(cardID, false, "telegram")
		if err != nil || respB != "ERR already_answered" {
			t.Fatalf("door B second approve want ERR already_answered, got %q, err=%v", respB, err)
		}

		// Door B receives GONE approved/web
		goneB, err := doorB.NextFrame(500 * time.Millisecond)
		if err != nil || !strings.Contains(goneB, `"state":"approved"`) || !strings.Contains(goneB, `"reason":"web"`) {
			t.Fatalf("door B GONE frame mismatch: got %q, err=%v", goneB, err)
		}

		// 2. Stale verdict path: late Allow on a timed-out/cancelled card
		cardID2, _ := h.hub.RegisterCustos("cell-1", "gmail", "gmail.googleapis.com:send", "summary2", 5*time.Second, nil)
		_, _ = doorA.NextFrame(200 * time.Millisecond)
		_, _ = doorB.NextFrame(200 * time.Millisecond)

		// Cancel card with flow_gone
		h.hub.CancelCard(cardID2, "flow_gone", "custos")
		_, _ = doorA.NextFrame(200 * time.Millisecond) // drain GONE
		_, _ = doorB.NextFrame(200 * time.Millisecond) // drain GONE

		staleResp, err := doorA.Approve(cardID2, false, "web")
		if err != nil || staleResp != "ERR stale_verdict" {
			t.Fatalf("late approve want ERR stale_verdict, got %q, err=%v", staleResp, err)
		}

		staleHookMu.Lock()
		fired := staleHookFired && staleCardID == cardID2
		staleHookMu.Unlock()
		if !fired {
			t.Fatalf("stale verdict hook did not fire on late allow")
		}
	})

	// Subtest 8: Bad source (missing via, unknown via) never defaults
	t.Run("BadSourceRejection", func(t *testing.T) {
		dir := mustShortDir(t)
		hub := surface.NewApprovalHub(5 * time.Second)
		doorSrv, err := custos.StartDoorServer(dir, hub)
		if err != nil {
			t.Fatalf("start door server: %v", err)
		}
		defer doorSrv.Close()

		sockPath, _ := custos.DoorPaths(dir)
		token, err := custos.ReadDoorToken(dir)
		if err != nil {
			t.Fatalf("read token: %v", err)
		}

		door, err := custos.DialDoor(sockPath, token, "source_tester")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer door.Close()

		cid, _ := hub.RegisterCustos("cell-1", "cred", "dest", "summary", 5*time.Second, nil)
		_, _ = door.NextFrame(200 * time.Millisecond) // drain CARD

		// A. Unknown via "ctl" -> ERR bad_source (never defaults to ctl!)
		r1, err := door.Approve(cid, false, "ctl")
		if err != nil || r1 != "ERR bad_source" {
			t.Fatalf("approve via ctl want ERR bad_source, got %q, err=%v", r1, err)
		}

		// B. Unknown via "random" -> ERR bad_source
		r2, err := door.Approve(cid, false, "random")
		if err != nil || r2 != "ERR bad_source" {
			t.Fatalf("approve via random want ERR bad_source, got %q, err=%v", r2, err)
		}

		// C. Missing via -> ERR bad_source
		if err := door.RawSend("APPROVE " + cid + " once\n"); err != nil {
			t.Fatalf("send raw: %v", err)
		}
		r3, err := door.RawReadLine(500 * time.Millisecond)
		if err != nil || r3 != "ERR bad_source" {
			t.Fatalf("approve missing via want ERR bad_source, got %q, err=%v", r3, err)
		}

		// D. DENY missing via -> ERR bad_source
		if err := door.RawSend("DENY " + cid + "\n"); err != nil {
			t.Fatalf("send raw: %v", err)
		}
		r4, err := door.RawReadLine(500 * time.Millisecond)
		if err != nil || r4 != "ERR bad_source" {
			t.Fatalf("deny missing via want ERR bad_source, got %q, err=%v", r4, err)
		}
	})

	// Subtest 9: Always derives canonical exact-match rule from parked request memory
	t.Run("AlwaysDerivesCanonicalRule", func(t *testing.T) {
		fake := &fakeOAuth{}
		h := setupWorkerHarness(t, fake)
		h.storeGmail(t)

		doorSrv, err := custos.StartDoorServer(h.v.StateDir(), h.hub,
			custos.WithVault(h.v),
			custos.WithWorkers(h.ws),
		)
		if err != nil {
			t.Fatalf("start door server: %v", err)
		}
		defer doorSrv.Close()

		sockPath, _ := custos.DoorPaths(h.v.StateDir())
		token, err := custos.ReadDoorToken(h.v.StateDir())
		if err != nil {
			t.Fatalf("read token: %v", err)
		}

		door, err := custos.DialDoor(sockPath, token, "always_door")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer door.Close()

		// Register a parked worker flow
		cardID, _ := h.hub.RegisterCustos("cell-1", "gmail", "gmail.googleapis.com:send", "review_digest", 5*time.Second, nil)
		h.ws.RegisterParkForTest(cardID, "cell-1", "gmail", "send", "review_digest")
		_, _ = door.NextFrame(200 * time.Millisecond) // drain CARD

		// Approve always via web
		resp, err := door.Approve(cardID, true, "web")
		if err != nil || resp != "OK" {
			t.Fatalf("approve always want OK, got %q, err=%v", resp, err)
		}

		// Verify canonical rule gmail/send auto was written to policy
		rules := h.v.Policy().ListRules()
		foundRule := false
		for _, r := range rules {
			if r.Pattern == "gmail/send" && r.Verdict == "auto" && r.Always {
				foundRule = true
				break
			}
		}
		if !foundRule {
			t.Fatalf("canonical rule gmail/send auto always not found in policy: %+v", rules)
		}

		// Verify audit log has always_rule_added and approval_answered with via:web and actor:surface
		records, err := h.v.Audit().ReadTailRecords(10)
		if err != nil {
			t.Fatalf("read audit: %v", err)
		}
		var foundAuditRule, foundAuditAnswered bool
		for _, r := range records {
			if r.Kind == custos.AuditKindAlwaysRuleAdded {
				foundAuditRule = true
			}
			if r.Kind == custos.AuditKindApprovalAnswered && r.Actor == "surface" && r.Via == "web" && r.Reason == "ok" {
				foundAuditAnswered = true
			}
		}
		if !foundAuditRule {
			t.Fatalf("audit missing always_rule_added: %+v", records)
		}
		if !foundAuditAnswered {
			t.Fatalf("audit missing approval_answered with via:web and actor:surface: %+v", records)
		}
	})

	// Subtest 10: ip_ask_only on IP-literal targets
	t.Run("IPAskOnlyOnIPLiteral", func(t *testing.T) {
		hProxy := setupProxyHarness(t, nil)
		defer hProxy.proxy.Close()

		doorSrv, err := custos.StartDoorServer(hProxy.v.StateDir(), hProxy.hub,
			custos.WithVault(hProxy.v),
			custos.WithProxy(hProxy.proxy),
		)
		if err != nil {
			t.Fatalf("start door server: %v", err)
		}
		defer doorSrv.Close()

		sockPath, _ := custos.DoorPaths(hProxy.v.StateDir())
		token, err := custos.ReadDoorToken(hProxy.v.StateDir())
		if err != nil {
			t.Fatalf("read token: %v", err)
		}

		door, err := custos.DialDoor(sockPath, token, "ip_door")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer door.Close()

		// Register an IP-literal parked egress card
		ipCardID, _ := hProxy.hub.RegisterCustos("cell-1", "203.0.113.7:80/api", "203.0.113.7:80/api", "rev", 5*time.Second, nil)
		hProxy.proxy.RegisterProxyParkForTest(ipCardID, "cell-1", "", "203.0.113.7", 80, "/api", "rev")
		_, _ = door.NextFrame(200 * time.Millisecond) // drain CARD

		// APPROVE ... always -> ERR ip_ask_only
		respAlways, err := door.Approve(ipCardID, true, "web")
		if err != nil || respAlways != "ERR ip_ask_only" {
			t.Fatalf("approve always on IP literal want ERR ip_ask_only, got %q, err=%v", respAlways, err)
		}

		// Verify NO rule was added to policy
		rules := hProxy.v.Policy().ListRules()
		for _, r := range rules {
			if strings.Contains(r.Pattern, "203.0.113.7") {
				t.Fatalf("unexpected rule written for IP literal: %+v", r)
			}
		}

		// APPROVE ... once is still allowed!
		respOnce, err := door.Approve(ipCardID, false, "web")
		if err != nil || respOnce != "OK" {
			t.Fatalf("approve once on IP literal want OK, got %q, err=%v", respOnce, err)
		}
	})

	// Subtest 11: Locked vault parity
	t.Run("LockedVaultParity", func(t *testing.T) {
		hProxy := setupProxyHarness(t, nil)
		defer hProxy.proxy.Close()

		doorSrv, err := custos.StartDoorServer(hProxy.v.StateDir(), hProxy.hub,
			custos.WithVault(hProxy.v),
			custos.WithProxy(hProxy.proxy),
		)
		if err != nil {
			t.Fatalf("start door server: %v", err)
		}
		defer doorSrv.Close()

		sockPath, _ := custos.DoorPaths(hProxy.v.StateDir())
		token, err := custos.ReadDoorToken(hProxy.v.StateDir())
		if err != nil {
			t.Fatalf("read token: %v", err)
		}

		// Lock vault
		if err := hProxy.v.Lock(); err != nil {
			t.Fatalf("lock vault: %v", err)
		}
		defer func() { _ = hProxy.v.Unlock(hProxy.passphrase, false) }()

		// HELLO while locked: returns OK []
		door, err := custos.DialDoor(sockPath, token, "locked_door")
		if err != nil {
			t.Fatalf("dial locked door: %v", err)
		}
		defer door.Close()

		if len(door.Cards()) != 0 {
			t.Fatalf("expected empty cards snapshot while locked, got %+v", door.Cards())
		}

		// Verdicts while locked answer ERR locked
		respA, err := door.Approve("a_nonexistent", false, "web")
		if err != nil || respA != "ERR locked" {
			t.Fatalf("approve while locked want ERR locked, got %q, err=%v", respA, err)
		}

		respD, err := door.Deny("a_nonexistent", "telegram")
		if err != nil || respD != "ERR locked" {
			t.Fatalf("deny while locked want ERR locked, got %q, err=%v", respD, err)
		}
	})

	// Subtest 12: Lock flush order (GONE(custos_locked) reaches door BEFORE vault drops state)
	t.Run("LockFlushOrder", func(t *testing.T) {
		hProxy := setupProxyHarness(t, nil)
		defer hProxy.proxy.Close()

		doorSrv, err := custos.StartDoorServer(hProxy.v.StateDir(), hProxy.hub,
			custos.WithVault(hProxy.v),
			custos.WithProxy(hProxy.proxy),
		)
		if err != nil {
			t.Fatalf("start door server: %v", err)
		}
		defer doorSrv.Close()

		sockPath, _ := custos.DoorPaths(hProxy.v.StateDir())
		token, err := custos.ReadDoorToken(hProxy.v.StateDir())
		if err != nil {
			t.Fatalf("read token: %v", err)
		}

		door, err := custos.DialDoor(sockPath, token, "flush_door")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer door.Close()

		cid, _ := hProxy.hub.RegisterCustos("cell-1", "cred_flush", "dest", "summary", 10*time.Second, nil)
		_, _ = door.NextFrame(200 * time.Millisecond) // drain CARD

		var (
			lockReturned                   atomic.Bool
			goneReceivedBeforeLockReturned atomic.Bool
			goneReceived                   sync.WaitGroup
		)
		goneReceived.Add(1)

		go func() {
			defer goneReceived.Done()
			frame, err := door.NextFrame(1 * time.Second)
			if err != nil {
				return
			}
			if strings.Contains(frame, "custos_locked") {
				// Observe that GONE reached door before vault reported locked
				if !lockReturned.Load() {
					goneReceivedBeforeLockReturned.Store(true)
				}
			}
		}()

		time.Sleep(10 * time.Millisecond)

		// Call Lock
		if err := hProxy.v.Lock(); err != nil {
			t.Fatalf("lock: %v", err)
		}
		lockReturned.Store(true)
		defer func() { _ = hProxy.v.Unlock(hProxy.passphrase, false) }()

		goneReceived.Wait()

		if !goneReceivedBeforeLockReturned.Load() {
			t.Fatalf("lock flush violated: GONE(custos_locked) was NOT received before vault reported locked!")
		}
		if hProxy.v.IsUnlocked() {
			t.Fatalf("vault should be locked after Lock() completes")
		}
		_ = cid
	})

	// Subtest 13: Slow-door eviction (queue bounded at 64 frames, drop on overflow, recover on re-HELLO)
	t.Run("SlowDoorEviction", func(t *testing.T) {
		dir := mustShortDir(t)
		hub := surface.NewApprovalHub(10 * time.Second)
		doorSrv, err := custos.StartDoorServer(dir, hub)
		if err != nil {
			t.Fatalf("start door server: %v", err)
		}
		defer doorSrv.Close()

		sockPath, _ := custos.DoorPaths(dir)
		token, err := custos.ReadDoorToken(dir)
		if err != nil {
			t.Fatalf("read token: %v", err)
		}

		slowDoor, err := custos.DialDoor(sockPath, token, "slow_door")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer slowDoor.Close()

		// Push > 64 events without reading (slow door stops reading)
		for i := 0; i < 70; i++ {
			hub.RegisterCustos("cell-1", "spam", "dest", "sum", 10*time.Second, nil)
		}

		// Wait briefly for buffer overflow to trigger drop
		time.Sleep(50 * time.Millisecond)

		// Assert connection was dropped by server
		var closedErr error
		for j := 0; j < 100; j++ {
			_, err = slowDoor.NextFrame(200 * time.Millisecond)
			if err != nil {
				closedErr = err
				break
			}
		}
		if closedErr == nil {
			t.Fatalf("expected slow door connection to be closed by server on queue overflow")
		}

		// Reconnect: recovers via HELLO snapshot
		recoveredDoor, err := custos.DialDoor(sockPath, token, "recovered_door")
		if err != nil {
			t.Fatalf("dial recovered door: %v", err)
		}
		defer recoveredDoor.Close()

		if len(recoveredDoor.Cards()) != 70 {
			t.Fatalf("recovered door snapshot len = %d, want 70", len(recoveredDoor.Cards()))
		}
	})

	// Subtest 14: Audit via-field honesty
	t.Run("AuditViaFieldHonesty", func(t *testing.T) {
		fake := &fakeOAuth{}
		h := setupWorkerHarness(t, fake)
		h.storeGmail(t)

		doorSrv, err := custos.StartDoorServer(h.v.StateDir(), h.hub,
			custos.WithVault(h.v),
			custos.WithWorkers(h.ws),
		)
		if err != nil {
			t.Fatalf("start door server: %v", err)
		}
		defer doorSrv.Close()

		ctlSrv, err := custos.StartCtlServer(h.v.StateDir(), h.v, nil)
		if err != nil {
			t.Fatalf("start ctl server: %v", err)
		}
		defer ctlSrv.Close()
		ctlSrv.SetWorkers(h.ws)
		ctlSrv.SetHub(h.hub)

		sockPath, _ := custos.DoorPaths(h.v.StateDir())
		token, _ := custos.ReadDoorToken(h.v.StateDir())
		door, err := custos.DialDoor(sockPath, token, "honesty_door")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer door.Close()

		ctlClient, _ := custos.NewCtlClient(h.v.StateDir(), 5*time.Second)

		// 1. Settle via door (web)
		cWeb, _ := h.hub.RegisterCustos("cell-1", "gmail", "dest", "rev", 5*time.Second, nil)
		h.ws.RegisterParkForTest(cWeb, "cell-1", "gmail", "send", "rev")
		_, _ = door.NextFrame(200 * time.Millisecond)
		_, _ = door.Approve(cWeb, false, "web")

		// 2. Settle via door (telegram)
		cTg, _ := h.hub.RegisterCustos("cell-1", "gmail", "dest", "rev", 5*time.Second, nil)
		h.ws.RegisterParkForTest(cTg, "cell-1", "gmail", "send", "rev")
		_, _ = door.NextFrame(200 * time.Millisecond)
		_, _ = door.Deny(cTg, "telegram")

		// 3. Settle via CLI door (ctl)
		cCtl, _ := h.hub.RegisterCustos("cell-1", "gmail", "dest", "rev", 5*time.Second, nil)
		h.ws.RegisterParkForTest(cCtl, "cell-1", "gmail", "send", "rev")
		_, _ = ctlClient.Approve(cCtl)

		// Audit verification
		records, err := h.v.Audit().ReadTailRecords(20)
		if err != nil {
			t.Fatalf("read audit: %v", err)
		}

		var foundWeb, foundTg, foundCli bool
		for _, r := range records {
			if r.Kind == custos.AuditKindApprovalAnswered {
				if r.Actor == "surface" && r.Via == "web" && r.Reason == "ok" {
					foundWeb = true
				}
				if r.Actor == "surface" && r.Via == "telegram" && r.Reason == "denied" {
					foundTg = true
				}
				if r.Actor == "cli" && r.Via == "" {
					foundCli = true
				}
			}
		}

		if !foundWeb {
			t.Fatalf("missing audit approval_answered actor:surface via:web")
		}
		if !foundTg {
			t.Fatalf("missing audit approval_answered actor:surface via:telegram")
		}
		if !foundCli {
			t.Fatalf("missing audit approval_answered actor:cli via:empty")
		}

		// Verify chain hashing integrity with Verify()
		verifyRes, err := h.v.Audit().Verify(nil)
		if err != nil {
			t.Fatalf("audit verify error: %v", err)
		}
		if verifyRes.IsBroken {
			t.Fatalf("audit chain broken at %s:%d", verifyRes.BrokenFile, verifyRes.BrokenLine)
		}
	})
}
