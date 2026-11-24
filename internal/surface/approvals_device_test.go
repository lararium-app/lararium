package surface

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/loop"
)

// T1: RegisterOn with deviceLive=true, webLive=false, bridge false -> pending
// (NOT denied), timeout == serve timeout (verified with deterministic clock).
func TestDeviceApprover_T1_RegisterOnPendingAndServeTimeout(t *testing.T) {
	clk := loop.NewFakeClock(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	serveTimeout := 30 * time.Second
	bridgeTimeout := 5 * time.Minute

	ap := NewApprovalHub(serveTimeout)
	ap.SetClock(clk)
	ap.SetBridgeTimeout(bridgeTimeout)
	ap.SetBridgeLive(func() bool { return false })
	ap.SetDeviceLive(func() bool { return true })
	t.Cleanup(ap.Shutdown)

	var gotCard *CustosCard
	cancelObs := ap.SubscribeCustos(func(card *CustosCard, _ *CustosGone) {
		if card != nil {
			c := *card
			gotCard = &c
		}
	})
	defer cancelObs()

	var eventID string
	id, decision := ap.RegisterOn("s_headless", "mcp__cal__create", `{"title":"standup"}`, false, func(aid string) {
		eventID = aid
	})
	if eventID != id {
		t.Fatalf("onEvent id = %q, want %q", eventID, id)
	}
	if ap.PendingCount("s_headless") != 1 {
		t.Fatalf("PendingCount = %d, want 1", ap.PendingCount("s_headless"))
	}
	if gotCard == nil || gotCard.ID != id || gotCard.Tool != "mcp__cal__create" {
		t.Fatalf("SubscribeCustos card = %+v, want id=%q", gotCard, id)
	}

	// Just before serve timeout: must remain pending.
	clk.Advance(serveTimeout - time.Second)
	select {
	case d := <-decision:
		t.Fatalf("settled early before serve timeout (decision=%v)", d)
	default:
	}

	// Cross serve timeout (well before bridge timeout): must expire timed_out.
	clk.Advance(time.Second)
	select {
	case d := <-decision:
		if d {
			t.Fatal("timeout decision = true, want false")
		}
	case <-time.After(time.Second):
		t.Fatal("device-live approval did not expire at serve timeout")
	}
	if r, s := ap.Reason("s_headless", id), ap.Source("s_headless", id); r != "timed_out" || s != "timer" {
		t.Fatalf("reason/source = %q/%q, want timed_out/timer", r, s)
	}
}

// T2: nothing live -> immediate denied:disconnected (S6/V6b regression guard).
func TestDeviceApprover_T2_NothingLiveDeniesImmediately(t *testing.T) {
	ap := NewApprovalHub(time.Minute)
	ap.SetBridgeLive(func() bool { return false })
	ap.SetDeviceLive(func() bool { return false })
	t.Cleanup(ap.Shutdown)

	fan := &recordingFanout{}
	ap.SetFanout(fan)

	id, decision := ap.RegisterOn("s_headless", "echo", "args", false, func(string) {
		t.Fatal("onEvent must not fire when no approver channel is live")
	})
	select {
	case d := <-decision:
		if d {
			t.Fatal("decision = true, want false")
		}
	default:
		t.Fatal("decision channel must be immediately ready when no channel is live")
	}
	if ap.PendingCount("s_headless") != 0 {
		t.Fatalf("PendingCount = %d, want 0", ap.PendingCount("s_headless"))
	}
	if r, s := ap.Reason("s_headless", id), ap.Source("s_headless", id); r != "disconnected" || s != "hub" {
		t.Fatalf("reason/source = %q/%q, want disconnected/hub", r, s)
	}
	term := fan.terminals()
	if len(term) != 1 || term[0][3] != "disconnected" || term[0][4] != "hub" {
		t.Fatalf("terminals = %v, want 1 disconnected/hub", term)
	}
}

// T3: DisconnectFor while deviceLive -> card survives; deviceLive=false ->
// settles denied:disconnected (source web).
func TestDeviceApprover_T3_DisconnectForSurvival(t *testing.T) {
	t.Run("survives when deviceLive is true", func(t *testing.T) {
		ap := NewApprovalHub(time.Hour)
		ap.SetBridgeLive(func() bool { return false })
		ap.SetDeviceLive(func() bool { return true })
		t.Cleanup(ap.Shutdown)

		id, decision := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
		ap.DisconnectFor("s1")

		if ap.PendingCount("s1") != 1 {
			t.Fatalf("PendingCount after DisconnectFor = %d, want 1", ap.PendingCount("s1"))
		}
		select {
		case d := <-decision:
			t.Fatalf("card should survive web disconnect while device is live (got %v)", d)
		default:
		}

		// Resolving via custody fallback still succeeds.
		if code := ap.ResolveFromVia("", id, true, "custos", "web"); code != 200 {
			t.Fatalf("ResolveFromVia = %d, want 200", code)
		}
		if !<-decision {
			t.Fatal("decision = false, want true")
		}
	})

	t.Run("settles denied:disconnected when deviceLive is false", func(t *testing.T) {
		ap := NewApprovalHub(time.Hour)
		ap.SetBridgeLive(func() bool { return false })
		ap.SetDeviceLive(func() bool { return false })
		t.Cleanup(ap.Shutdown)

		id, decision := ap.RegisterOn("s1", "echo", "args", true, func(string) {})
		ap.DisconnectFor("s1")

		if ap.PendingCount("s1") != 0 {
			t.Fatalf("PendingCount after DisconnectFor = %d, want 0", ap.PendingCount("s1"))
		}
		select {
		case d := <-decision:
			if d {
				t.Fatal("decision = true, want false")
			}
		default:
			t.Fatal("expected immediate denial on DisconnectFor with no remaining approver")
		}
		// A connection-loss denial is machine-caused: the source must name the
		// cause, not a human channel, or the ledger reads as if the dashboard
		// user pressed Deny (#338). "listener_closed" folds to hub on the
		// frozen §6 audit wire, matching its A9 device-channel-loss twin.
		if r, s := ap.Reason("s1", id), ap.Source("s1", id); r != "disconnected" || s != "listener_closed" {
			t.Fatalf("reason/source = %q/%q, want disconnected/listener_closed", r, s)
		}
	})
}

// T4: DeviceChannelLost settles orphans (no web/bridge) denied:disconnected
// source device; leaves webLive card pending; leaves bridge-covered
// (bridgeLive && !cardDead) card pending; delivers CustosGone to
// SubscribeCustos observers.
func TestDeviceApprover_T4_DeviceChannelLost(t *testing.T) {
	t.Run("settles orphans and leaves webLive and custos cards pending", func(t *testing.T) {
		ap := NewApprovalHub(time.Hour)
		ap.SetBridgeLive(func() bool { return false })
		var deviceUp atomic.Bool
		deviceUp.Store(true)
		ap.SetDeviceLive(deviceUp.Load)
		t.Cleanup(ap.Shutdown)

		var obsMu sync.Mutex
		var gones []CustosGone
		cancelObs := ap.SubscribeCustos(func(_ *CustosCard, gone *CustosGone) {
			if gone != nil {
				obsMu.Lock()
				gones = append(gones, *gone)
				obsMu.Unlock()
			}
		})
		defer cancelObs()

		orphan1, dec1 := ap.RegisterOn("s_orphan1", "tool_a", "args", false, func(string) {})
		orphan2, dec2 := ap.RegisterOn("s_orphan2", "tool_b", "args", false, func(string) {})
		webID, webDec := ap.RegisterOn("s_web", "tool_web", "args", true, func(string) {})
		custosID, custosDec := ap.RegisterCustosNamed("", "", "connector_gate", "", "review", time.Hour, func(string) {})

		// The detach event implies the probe now reports false (TOCTOU
		// guard in DeviceChannelLost keeps it live otherwise).
		deviceUp.Store(false)
		ap.DeviceChannelLost()

		for _, tc := range []struct {
			sid, id string
			dec     <-chan bool
		}{
			{"s_orphan1", orphan1, dec1},
			{"s_orphan2", orphan2, dec2},
		} {
			select {
			case d := <-tc.dec:
				if d {
					t.Fatalf("%s: decision = true, want false", tc.sid)
				}
			default:
				t.Fatalf("%s: orphan did not settle on DeviceChannelLost", tc.sid)
			}
			if r, s := ap.Reason(tc.sid, tc.id), ap.Source(tc.sid, tc.id); r != "disconnected" || s != "device" {
				t.Fatalf("%s: reason/source = %q/%q, want disconnected/device", tc.sid, r, s)
			}
			// A8 audit wire confinement: internal source "device" maps to "hub".
			obj, ok := auditApprovalFor(ap, tc.sid, "denied:"+tc.id, "cron")
			if !ok {
				t.Fatalf("%s: auditApprovalFor returned ok=false", tc.sid)
			}
			if !strings.Contains(string(obj), `"reason":"disconnected"`) || !strings.Contains(string(obj), `"source":"hub"`) {
				t.Fatalf("%s: audit envelope = %s, want disconnected + source:hub", tc.sid, string(obj))
			}
		}

		// Web-live and RegisterCustosNamed cards must stay pending.
		if ap.PendingCount("s_web") != 1 {
			t.Fatalf("s_web PendingCount = %d, want 1", ap.PendingCount("s_web"))
		}
		select {
		case d := <-webDec:
			t.Fatalf("webLive card settled unexpectedly: %v", d)
		default:
		}
		if ap.Reason("s_web", webID) != "" {
			t.Fatalf("webLive card reason = %q, want empty", ap.Reason("s_web", webID))
		}
		select {
		case d := <-custosDec:
			t.Fatalf("RegisterCustosNamed card %s settled unexpectedly: %v", custosID, d)
		default:
		}

		// CustosGone (State "denied", Reason "disconnected") reached SubscribeCustos observers.
		obsMu.Lock()
		got := append([]CustosGone(nil), gones...)
		obsMu.Unlock()
		if len(got) != 2 {
			t.Fatalf("CustosGone notifications = %d, want exactly 2 (no duplicates): %+v", len(got), got)
		}
		found := map[string]CustosGone{}
		for _, g := range got {
			found[g.ID] = g
		}
		for _, id := range []string{orphan1, orphan2} {
			g, ok := found[id]
			if !ok {
				t.Fatalf("missing CustosGone for orphan %s (got %+v)", id, got)
			}
			if g.State != "denied" || g.Reason != "disconnected" {
				t.Fatalf("CustosGone[%s] = %+v, want State=denied Reason=disconnected", id, g)
			}
		}
		if _, leaked := found[webID]; leaked {
			t.Fatalf("unexpected CustosGone for still-pending web card %s", webID)
		}
	})

	t.Run("leaves bridge-covered card pending and settles cardDead card", func(t *testing.T) {
		ap := NewApprovalHub(time.Hour)
		ap.SetBridgeLive(func() bool { return true })
		var deviceUp atomic.Bool
		deviceUp.Store(true)
		ap.SetDeviceLive(deviceUp.Load)
		t.Cleanup(ap.Shutdown)

		bridgeID, bridgeDec := ap.RegisterOn("s_bridge", "tool_bridge", "args", false, func(string) {})
		// Card starts with web+bridge+device; Telegram card dies while web is
		// live (cardDead=true), then web drops while device is live -> only
		// device covers it.
		deadID, deadDec := ap.RegisterOn("s_dead", "tool_dead", "args", true, func(string) {})
		ap.MarkUndeliverable(deadID)
		ap.DisconnectFor("s_dead")
		if ap.PendingCount("s_dead") != 1 {
			t.Fatalf("s_dead should remain pending while device is live")
		}

		deviceUp.Store(false)
		ap.DeviceChannelLost()

		// Bridge-covered card (!cardDead) stays pending.
		if ap.PendingCount("s_bridge") != 1 {
			t.Fatalf("s_bridge PendingCount = %d, want 1", ap.PendingCount("s_bridge"))
		}
		select {
		case d := <-bridgeDec:
			t.Fatalf("bridge-covered card %s settled unexpectedly: %v", bridgeID, d)
		default:
		}

		// cardDead card has no remaining live approver -> settles denied:disconnected source device.
		select {
		case d := <-deadDec:
			if d {
				t.Fatal("cardDead decision = true, want false")
			}
		default:
			t.Fatal("cardDead card did not settle on DeviceChannelLost")
		}
		if r, s := ap.Reason("s_dead", deadID), ap.Source("s_dead", deadID); r != "disconnected" || s != "device" {
			t.Fatalf("cardDead reason/source = %q/%q, want disconnected/device", r, s)
		}
	})
}

// T5: timer rank regressions (web > device > telegram) using deterministic fake clock.
func TestDeviceApprover_T5_TimerRankRegressions(t *testing.T) {
	cases := []struct {
		name          string
		serveTimeout  time.Duration
		bridgeTimeout time.Duration
		webLive       bool
		bridgeLive    bool
		deviceLive    bool
		wantTimeout   time.Duration
	}{
		{
			name:          "telegram-only binds bridge timeout (bridge > serve)",
			serveTimeout:  30 * time.Second,
			bridgeTimeout: 5 * time.Minute,
			webLive:       false,
			bridgeLive:    true,
			deviceLive:    false,
			wantTimeout:   5 * time.Minute,
		},
		{
			name:          "telegram-only binds bridge timeout (bridge < serve)",
			serveTimeout:  5 * time.Minute,
			bridgeTimeout: 30 * time.Second,
			webLive:       false,
			bridgeLive:    true,
			deviceLive:    false,
			wantTimeout:   30 * time.Second,
		},
		{
			name:          "web+device binds serve timeout",
			serveTimeout:  30 * time.Second,
			bridgeTimeout: 5 * time.Minute,
			webLive:       true,
			bridgeLive:    false,
			deviceLive:    true,
			wantTimeout:   30 * time.Second,
		},
		{
			name:          "device+telegram binds serve timeout (serve < bridge)",
			serveTimeout:  30 * time.Second,
			bridgeTimeout: 5 * time.Minute,
			webLive:       false,
			bridgeLive:    true,
			deviceLive:    true,
			wantTimeout:   30 * time.Second,
		},
		{
			name:          "device+telegram binds serve timeout (serve > bridge)",
			serveTimeout:  5 * time.Minute,
			bridgeTimeout: 30 * time.Second,
			webLive:       false,
			bridgeLive:    true,
			deviceLive:    true,
			wantTimeout:   5 * time.Minute,
		},
		{
			name:          "web+device+telegram binds serve timeout",
			serveTimeout:  45 * time.Second,
			bridgeTimeout: 5 * time.Minute,
			webLive:       true,
			bridgeLive:    true,
			deviceLive:    true,
			wantTimeout:   45 * time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := loop.NewFakeClock(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
			ap := NewApprovalHub(tc.serveTimeout)
			ap.SetClock(clk)
			ap.SetBridgeTimeout(tc.bridgeTimeout)
			ap.SetBridgeLive(func() bool { return tc.bridgeLive })
			ap.SetDeviceLive(func() bool { return tc.deviceLive })
			t.Cleanup(ap.Shutdown)

			id, dec := ap.RegisterOn("s1", "echo", "args", tc.webLive, func(string) {})

			// 1s before expected timeout: must still be pending.
			clk.Advance(tc.wantTimeout - time.Second)
			select {
			case d := <-dec:
				t.Fatalf("settled early before %v (decision=%v, reason=%q)", tc.wantTimeout, d, ap.Reason("s1", id))
			default:
			}

			// At expected timeout: must settle timed_out.
			clk.Advance(time.Second)
			select {
			case d := <-dec:
				if d {
					t.Fatal("timeout decision = true, want false")
				}
			case <-time.After(time.Second):
				t.Fatalf("did not expire at expected timeout %v", tc.wantTimeout)
			}
			if r := ap.Reason("s1", id); r != "timed_out" {
				t.Fatalf("reason = %q, want timed_out", r)
			}
		})
	}
}

// T6: server-side presence accounting wired via SetApprovalHub.
// Tests:
// - no sub -> probe false, RegisterOn denies immediately
// - authenticated WS conn without custos.cards.subscribe -> probe still false
// - custos.cards.subscribe -> probe true, RegisterOn mints pending + delivers custos.card
// - two subscribers, first drops -> probe still true, card survives
// - resubscribe on sole subscriber -> card survives (no transient drop)
// - last subscriber disconnects -> probe false, orphan settles denied:disconnected
// - overflow teardown on last subscriber (removeCustosRPCSub) -> settles orphan.
func TestDeviceApprover_T6_ServerPresenceAccounting(t *testing.T) {
	t.Run("subscribe, multi-client, resubscribe, and last disconnect", func(t *testing.T) {
		srv, tokStore, ts := newTestRPCServer(t, 2*time.Second)
		hub := NewApprovalHub(30 * time.Second)
		t.Cleanup(hub.Shutdown)
		srv.SetApprovalHub(hub)

		// Initially no subscriber: probe false, headless RegisterOn denies.
		if srv.DeviceApproverLive() {
			t.Fatal("DeviceApproverLive = true before any subscription")
		}
		preID, preDec := hub.RegisterOn("s_headless", "pre_tool", "args", false, func(string) {
			t.Fatal("onEvent must not fire when no device is subscribed")
		})
		if <-preDec {
			t.Fatal("pre-subscribe decision = true, want false")
		}
		if r := hub.Reason("s_headless", preID); r != "disconnected" {
			t.Fatalf("pre-subscribe reason = %q, want disconnected", r)
		}

		tok1, err := tokStore.Create("phone-1")
		if err != nil {
			t.Fatal(err)
		}
		tok2, err := tokStore.Create("phone-2")
		if err != nil {
			t.Fatal(err)
		}

		// Authenticated WS connection WITHOUT custos.cards.subscribe must NOT count.
		client1 := dialAuthenticatedWS(t, ts.URL, tok1)
		if srv.DeviceApproverLive() {
			t.Fatal("DeviceApproverLive = true for WS conn without custos.cards.subscribe")
		}

		// Subscribe client1 -> probe becomes true, headless RegisterOn mints pending.
		if resp := client1.callRPC(t, 1, "custos.cards.subscribe", map[string]any{}); resp.Error != nil {
			t.Fatalf("client1 subscribe error: %+v", resp.Error)
		}
		if !srv.DeviceApproverLive() {
			t.Fatal("DeviceApproverLive = false after custos.cards.subscribe")
		}

		cardID, decision := hub.RegisterOn("s_headless", "mcp__mail__send", "to=alice", false, func(string) {})
		if hub.PendingCount("s_headless") != 1 {
			t.Fatalf("PendingCount = %d, want 1", hub.PendingCount("s_headless"))
		}
		notif := client1.readNotification(t)
		if notif.Method != "custos.card" {
			t.Fatalf("notif method = %q, want custos.card", notif.Method)
		}
		var card CustosCard
		if err := json.Unmarshal(notif.Params, &card); err != nil {
			t.Fatalf("unmarshal card: %v", err)
		}
		if card.ID != cardID || card.Tool != "mcp__mail__send" {
			t.Fatalf("card = %+v, want id=%q tool=mcp__mail__send", card, cardID)
		}

		// Second subscriber attaches; first disconnects -> count stays 1, card survives.
		client2 := dialAuthenticatedWS(t, ts.URL, tok2)
		if resp := client2.callRPC(t, 1, "custos.cards.subscribe", map[string]any{}); resp.Error != nil {
			t.Fatalf("client2 subscribe error: %+v", resp.Error)
		}
		_ = client1.conn.Close()

		deadline := time.Now().Add(2 * time.Second)
		detached := false
		for time.Now().Before(deadline) {
			srv.rpcCustosMu.Lock()
			n := len(srv.rpcCustosSubs)
			srv.rpcCustosMu.Unlock()
			if n == 1 {
				detached = true
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !detached {
			t.Fatal("client1 custody sub did not detach within 2s; the survival assertions below would be vacuous")
		}
		if !srv.DeviceApproverLive() {
			t.Fatal("DeviceApproverLive = false while client2 is still subscribed")
		}
		if hub.PendingCount("s_headless") != 1 {
			t.Fatalf("PendingCount = %d after non-last detach, want 1", hub.PendingCount("s_headless"))
		}

		// Re-subscribe on sole remaining client2 -> card survives (no transient drop to 0).
		if resp := client2.callRPC(t, 2, "custos.cards.subscribe", map[string]any{}); resp.Error != nil {
			t.Fatalf("client2 resubscribe error: %+v", resp.Error)
		}
		if hub.PendingCount("s_headless") != 1 {
			t.Fatalf("PendingCount = %d after resubscribe, want 1", hub.PendingCount("s_headless"))
		}

		// Last subscriber disconnects -> orphan settles denied:disconnected, decision yields false.
		_ = client2.conn.Close()
		select {
		case d := <-decision:
			if d {
				t.Fatal("orphan decision = true, want false")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("orphan did not settle after last custody subscriber disconnected")
		}
		if srv.DeviceApproverLive() {
			t.Fatal("DeviceApproverLive = true after last subscriber disconnected")
		}
		if r, s := hub.Reason("s_headless", cardID), hub.Source("s_headless", cardID); r != "disconnected" || s != "device" {
			t.Fatalf("reason/source = %q/%q, want disconnected/device", r, s)
		}
	})

	t.Run("overflow teardown on last subscriber settles orphan via removeCustosRPCSub", func(t *testing.T) {
		srv, tokStore, ts := newTestRPCServer(t, 2*time.Second)
		reg := newRPCTestCustosRegistry()
		hub := NewApprovalHub(30 * time.Second)
		t.Cleanup(hub.Shutdown)
		srv.SetCustosRegistry(reg)
		srv.SetApprovalHub(hub)

		// Subscribe, then synchronously hand the minted pair from the
		// handler goroutine back to this test goroutine (no data race).
		type mintedApproval struct {
			id  string
			dec <-chan bool
		}
		minted := make(chan mintedApproval, 1)
		reg.onSubscribe = func(push func(*CustosCard, *CustosGone)) {
			id, dec := hub.RegisterOn("s_overflow", "tool_ovf", "args", false, func(string) {})
			minted <- mintedApproval{id, dec}
			for i := range custosRPCBufferCap + 1 {
				push(&CustosCard{ID: fmt.Sprintf("a_%016d", i), Tool: "burst"}, nil)
			}
		}

		tok, err := tokStore.Create("phone-overflow")
		if err != nil {
			t.Fatal(err)
		}
		client := dialAuthenticatedWS(t, ts.URL, tok)
		if resp := client.callRPC(t, 1, "custos.cards.subscribe", map[string]any{}); resp.Error != nil {
			t.Fatalf("subscribe error: %+v", resp.Error)
		}
		notif := client.readNotification(t)
		if notif.Method != "custos.resync" {
			t.Fatalf("notif method = %q, want custos.resync", notif.Method)
		}

		// Overflow drops the last custody sub via removeCustosRPCSub -> settles orphan.
		var m mintedApproval
		select {
		case m = <-minted:
		case <-time.After(3 * time.Second):
			t.Fatal("onSubscribe never handed back the minted approval")
		}
		select {
		case d := <-m.dec:
			if d {
				t.Fatal("overflow orphan decision = true, want false")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("orphan did not settle after last custody sub overflowed")
		}
		if srv.DeviceApproverLive() {
			t.Fatal("DeviceApproverLive = true after last sub overflowed")
		}
		if r, s := hub.Reason("s_overflow", m.id), hub.Source("s_overflow", m.id); r != "disconnected" || s != "device" {
			t.Fatalf("reason/source = %q/%q, want disconnected/device", r, s)
		}
	})
}

// T7: hub without SetDeviceLive (probe nil) behaves byte-identically to pre-A9.
func TestDeviceApprover_T7_NilDeviceProbeRegression(t *testing.T) {
	clk := loop.NewFakeClock(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	ap := NewApprovalHub(30 * time.Second)
	ap.SetClock(clk)
	ap.SetBridgeTimeout(5 * time.Minute)
	t.Cleanup(ap.Shutdown)

	// No SetDeviceLive called: webLive=false, no bridge -> immediate denied:disconnected (source hub).
	id1, dec1 := ap.RegisterOn("s1", "echo", "args", false, func(string) {
		t.Fatal("onEvent must not fire with nil device probe and no web/bridge")
	})
	if <-dec1 {
		t.Fatal("decision = true, want false")
	}
	if r, s := ap.Reason("s1", id1), ap.Source("s1", id1); r != "disconnected" || s != "hub" {
		t.Fatalf("reason/source = %q/%q, want disconnected/hub", r, s)
	}

	// webLive=true -> pending, DisconnectFor settles denied:disconnected (source web).
	id2, dec2 := ap.RegisterOn("s2", "echo", "args", true, func(string) {})
	if ap.PendingCount("s2") != 1 {
		t.Fatalf("PendingCount = %d, want 1", ap.PendingCount("s2"))
	}
	ap.DisconnectFor("s2")
	if <-dec2 {
		t.Fatal("decision = true, want false")
	}
	// A connection-loss denial is machine-caused: the source must name the
	// cause, not a human channel, or the ledger reads as if the dashboard
	// user pressed Deny (#338). "listener_closed" folds to hub on the
	// frozen §6 audit wire, matching its A9 device-channel-loss twin.
	if r, s := ap.Reason("s2", id2), ap.Source("s2", id2); r != "disconnected" || s != "listener_closed" {
		t.Fatalf("reason/source = %q/%q, want disconnected/listener_closed", r, s)
	}

	// SetApprovalHub(nil) stays nil-safe.
	srv := &Server{}
	srv.SetApprovalHub(nil)
}

// T8: DeviceChannelLost re-checks the probe under the hub lock (TOCTOU
// guard): a stale detach event that arrives after another device has
// (re)attached must NOT settle live approvals.
func TestDeviceApprover_T8_StaleDetachGuard(t *testing.T) {
	ap := NewApprovalHub(time.Hour)
	ap.SetBridgeLive(func() bool { return false })
	var deviceUp atomic.Bool
	deviceUp.Store(true)
	ap.SetDeviceLive(deviceUp.Load)
	t.Cleanup(ap.Shutdown)

	id, dec := ap.RegisterOn("s_headless", "echo", "args", false, func(string) {})

	// Stale detach while a device is (again) live: no-op.
	ap.DeviceChannelLost()
	select {
	case d := <-dec:
		t.Fatalf("approval settled by stale detach despite live device (got %v)", d)
	default:
	}
	if ap.PendingCount("s_headless") != 1 {
		t.Fatalf("PendingCount = %d, want 1", ap.PendingCount("s_headless"))
	}

	// True detach: settles denied:disconnected source device.
	deviceUp.Store(false)
	ap.DeviceChannelLost()
	select {
	case d := <-dec:
		if d {
			t.Fatal("decision = true, want false")
		}
	default:
		t.Fatal("approval did not settle on true device detach")
	}
	if r, s := ap.Reason("s_headless", id), ap.Source("s_headless", id); r != "disconnected" || s != "device" {
		t.Fatalf("reason/source = %q/%q, want disconnected/device", r, s)
	}
}

// T9: A7/A9 interop — MarkUndeliverable never resolves denied:undeliverable
// while a live paired-device subscription holds the card; it sets
// card_dead and the approval keeps its timer. With no other live
// approver the pre-A9 immediate deny stands.
func TestDeviceApprover_T9_MarkUndeliverableDeviceInterop(t *testing.T) {
	t.Run("device live -> card_dead, stays pending, later detach settles", func(t *testing.T) {
		ap := NewApprovalHub(time.Hour)
		ap.SetBridgeLive(func() bool { return true })
		var deviceUp atomic.Bool
		deviceUp.Store(true)
		ap.SetDeviceLive(deviceUp.Load)
		t.Cleanup(ap.Shutdown)

		id, dec := ap.RegisterOn("s_headless", "echo", "args", false, func(string) {})
		ap.MarkUndeliverable(id)
		select {
		case d := <-dec:
			t.Fatalf("undeliverable killed device-covered card (got %v)", d)
		default:
		}
		if ap.PendingCount("s_headless") != 1 {
			t.Fatalf("PendingCount = %d, want 1 (card_dead survives on device)", ap.PendingCount("s_headless"))
		}
		// card_dead now means the bridge no longer covers it: when the
		// device drops, the orphan settles denied:disconnected.
		deviceUp.Store(false)
		ap.DeviceChannelLost()
		select {
		case d := <-dec:
			if d {
				t.Fatal("decision = true, want false")
			}
		default:
			t.Fatal("card_dead approval did not settle after device detach")
		}
		if r, s := ap.Reason("s_headless", id), ap.Source("s_headless", id); r != "disconnected" || s != "device" {
			t.Fatalf("reason/source = %q/%q, want disconnected/device", r, s)
		}
	})

	t.Run("no other approver -> immediate denied:undeliverable (pre-A9)", func(t *testing.T) {
		ap := NewApprovalHub(time.Hour)
		ap.SetBridgeLive(func() bool { return true })
		ap.SetDeviceLive(func() bool { return false })
		t.Cleanup(ap.Shutdown)

		id, dec := ap.RegisterOn("s_headless", "echo", "args", false, func(string) {})
		ap.MarkUndeliverable(id)
		select {
		case d := <-dec:
			if d {
				t.Fatal("decision = true, want false")
			}
		default:
			t.Fatal("undeliverable with sole telegram channel must deny immediately")
		}
		if r, s := ap.Reason("s_headless", id), ap.Source("s_headless", id); r != "undeliverable" || s != "hub" {
			t.Fatalf("reason/source = %q/%q, want undeliverable/hub", r, s)
		}
	})
}

// T10: the timer is bound at creation and never re-attributed. A
// device+telegram approval minted on the serve timeout keeps that
// timeout across a mid-flight device drop: it survives on Telegram
// (A1) but still expires at serveTimeout, not bridgeTimeout.
func TestDeviceApprover_T10_TimerNotReattributed(t *testing.T) {
	clk := loop.NewFakeClock(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC))
	ap := NewApprovalHub(30 * time.Second)
	ap.SetClock(clk)
	ap.SetBridgeTimeout(5 * time.Minute)
	ap.SetBridgeLive(func() bool { return true })
	var deviceUp atomic.Bool
	deviceUp.Store(true)
	ap.SetDeviceLive(deviceUp.Load)
	t.Cleanup(ap.Shutdown)

	id, dec := ap.RegisterOn("s_headless", "echo", "args", false, func(string) {})

	// Mid-flight device drop: bridge covers the card, it stays pending.
	deviceUp.Store(false)
	ap.DeviceChannelLost()
	select {
	case d := <-dec:
		t.Fatalf("bridge-covered card settled on device drop (got %v)", d)
	default:
	}

	// Still pending at serveTimeout-1s...
	clk.Advance(29 * time.Second)
	select {
	case d := <-dec:
		t.Fatalf("settled early before serve timeout (got %v)", d)
	default:
	}
	// ...and expires at the creation-bound serve timeout, not the
	// (longer) bridge timeout, and never as disconnected.
	clk.Advance(time.Second)
	select {
	case d := <-dec:
		if d {
			t.Fatal("timeout decision = true, want false")
		}
	case <-time.After(time.Second):
		t.Fatal("card did not expire at creation-bound serve timeout")
	}
	if r := ap.Reason("s_headless", id); r != "timed_out" {
		t.Fatalf("reason = %q, want timed_out", r)
	}
}
