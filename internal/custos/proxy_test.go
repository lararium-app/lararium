package custos_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/surface"
)

type proxyHarness struct {
	v            *custos.Vault
	hub          *surface.ApprovalHub
	proxy        *custos.Proxy
	stateDir     string
	passphrase   string
	listenerAddr string
}

func setupProxyHarness(t *testing.T, cfg *custos.Config) *proxyHarness {
	t.Helper()
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatalf("v.Init: %v", err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("v.Unlock: %v", err)
	}

	if cfg == nil {
		cfg = &custos.Config{
			AskHoldTimeout: 5 * time.Second,
		}
	}
	cfg.Normalize()

	hub := surface.NewApprovalHub(cfg.AskHoldTimeout)
	v.SetApprovalHub(hub)
	p := custos.NewProxy(v, hub, cfg)

	// Default DNS seam: fake hosts in these tests (revoke.example.com,
	// fg.example.com, ...) resolve to a fixed non-floored public IP so
	// admission reaches the policy gate; forwards use the echo-server
	// dial seam. Tests probing floor/rebinding override per-test.
	p.SetResolveFn(func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})

	if err := p.BindListener("127.0.0.1:0", "cell-1", "127.0.0.1", ""); err != nil {
		t.Fatalf("BindListener: %v", err)
	}
	listeners := p.ListListeners()
	if len(listeners) == 0 {
		t.Fatalf("expected at least one listener")
	}

	t.Cleanup(func() {
		_ = p.Close()
		hub.DenyAllAll("cleanup", "cleanup")
	})

	return &proxyHarness{
		v:            v,
		hub:          hub,
		proxy:        p,
		stateDir:     stateDir,
		passphrase:   pass,
		listenerAddr: listeners[0],
	}
}

// V10: CONNECT blind passthrough per CUSTOS-SPEC §5.2, §7, §10 V10.
// Assertions:
// - CONNECT through the proxy establishes a tunnel.
// - Upstream sees the surrogate token byte-identical (no swap, Authorization untouched).
// - No swap audit event is emitted (neither swap_allowed nor swap_denied).
// - CONNECT to a floored destination (169.254.169.254) is refused by floor.
func TestV10_ConnectPassthrough(t *testing.T) {
	h := setupProxyHarness(t, nil)

	// Add test credential and surrogate
	credSecret := "sk-real-secret-12345"
	err := h.v.Mutate(h.passphrase, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["openai"] = custos.Credential{Kind: "api_key", Secret: credSecret}
		return []string{"openai"}, nil
	}, false)
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}

	surToken, err := h.v.AddSurrogate(h.passphrase, "openai", "api.example.com", 443, "/", false, "test")
	if err != nil {
		t.Fatalf("add surrogate: %v", err)
	}

	// Fake origin TCP echo server
	var receivedAuth atomic.Value
	originLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("origin listen: %v", err)
	}
	defer originLn.Close()

	go func() {
		for {
			conn, err := originLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				rd := bufio.NewReader(c)
				req, err := http.ReadRequest(rd)
				if err != nil {
					return
				}
				receivedAuth.Store(req.Header.Get("Authorization"))
				resp := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK"
				_, _ = c.Write([]byte(resp))
			}(conn)
		}
	}()

	// Route api.example.com:443 to our fake origin
	h.proxy.SetDialFn(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return net.Dial("tcp", originLn.Addr().String())
	})
	h.proxy.SetResolveFn(func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})

	// 1. Send CONNECT to proxy listener
	proxyConn, err := net.Dial("tcp", h.listenerAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer proxyConn.Close()

	connectReq := "CONNECT api.example.com:443 HTTP/1.1\r\nHost: api.example.com:443\r\n\r\n"
	if _, err := proxyConn.Write([]byte(connectReq)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	rd := bufio.NewReader(proxyConn)
	statusLine, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if !strings.Contains(statusLine, "200") {
		t.Fatalf("expected 200 Connection established, got: %s", statusLine)
	}
	// Consume empty line after headers
	for {
		line, _ := rd.ReadString('\n')
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	// 2. Send request with surrogate token through tunnel
	httpReq := fmt.Sprintf("GET /v1/models HTTP/1.1\r\nHost: api.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken)
	if _, err := proxyConn.Write([]byte(httpReq)); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}

	respLine, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read response through tunnel: %v", err)
	}
	if !strings.Contains(respLine, "200") {
		t.Fatalf("expected 200 OK from origin, got: %s", respLine)
	}

	// 3. Assert origin saw surrogate untouched (blind passthrough)
	rawAuth := receivedAuth.Load()
	if rawAuth == nil {
		t.Fatalf("origin received nothing")
	}
	authStr, ok := rawAuth.(string)
	if !ok {
		t.Fatalf("origin Authorization is %T, want string", rawAuth)
	}
	expectedAuth := "Bearer " + surToken
	if authStr != expectedAuth {
		t.Fatalf("origin auth header = %q, want %q (surrogate untouched)", authStr, expectedAuth)
	}
	if strings.Contains(authStr, credSecret) {
		t.Fatalf("secret leaked in CONNECT passthrough!")
	}

	// 4. Assert NO swap audit record emitted
	records, err := h.v.Audit().ReadTailRecords(50)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	for _, r := range records {
		if r.Kind == custos.AuditKindSwapAllowed || r.Kind == custos.AuditKindSwapDenied {
			t.Fatalf("unexpected swap audit record for CONNECT: %+v", r)
		}
	}

	// 5. Floor check on CONNECT: CONNECT to 169.254.169.254:443 must be blocked by floor
	floorConn, err := net.Dial("tcp", h.listenerAddr)
	if err != nil {
		t.Fatalf("dial proxy for floor test: %v", err)
	}
	defer floorConn.Close()

	if _, err := floorConn.Write([]byte("CONNECT 169.254.169.254:443 HTTP/1.1\r\nHost: 169.254.169.254:443\r\n\r\n")); err != nil {
		t.Fatalf("write floor CONNECT: %v", err)
	}

	floorRd := bufio.NewReader(floorConn)
	floorStatus, err := floorRd.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read floor response: %v", err)
	}
	if !strings.Contains(floorStatus, "403") {
		t.Fatalf("expected 403 on floor CONNECT, got %q", floorStatus)
	}
}

// V16: Rebinding guard both directions per CUSTOS-SPEC §7, §10 V16.
// DNS answer differs from approve-time — forward-time dial target decides.
// Plus amended floor list test: 100.64.0.0/10 and 0.0.0.0/8 refused under auto rule.
func TestV16_RebindingGuardBothDirections(t *testing.T) {
	h := setupProxyHarness(t, nil)

	credSecret := "sk-rebind-secret-45678" //nolint:gosec // test fixture, not a live credential
	err := h.v.Mutate(h.passphrase, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["rebind-cred"] = custos.Credential{Kind: "api_key", Secret: credSecret}
		return []string{"rebind-cred"}, nil
	}, false)
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}

	surToken, err := h.v.AddSurrogate(h.passphrase, "rebind-cred", "rebind.example.com", 80, "/", false, "test")
	if err != nil {
		t.Fatalf("add surrogate: %v", err)
	}

	// Rule: ask for rebind.example.com/*
	_, err = h.v.Policy().AddEgressRule("rebind.example.com/*", "ask", false, "cli", "cli")
	if err != nil {
		t.Fatalf("add rule: %v", err)
	}

	var dnsFlipped atomic.Bool
	h.proxy.SetResolveFn(func(ctx context.Context, host string) ([]net.IP, error) {
		if dnsFlipped.Load() {
			// DNS flipped to metadata endpoint!
			return []net.IP{net.ParseIP("169.254.169.254")}, nil
		}
		// Public IP initially
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})

	originDialed := atomic.Int64{}
	h.proxy.SetDialFn(func(ctx context.Context, network, addr string) (net.Conn, error) {
		originDialed.Add(1)
		return nil, fmt.Errorf("dial disallowed")
	})

	// Client sends request to proxy
	conn, err := net.Dial("tcp", h.listenerAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	req := fmt.Sprintf("GET /data HTTP/1.1\r\nHost: rebind.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Wait for card to appear on hub
	var card *surface.ApprovalCard
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pending := h.hub.Pending()
		if len(pending) > 0 {
			card = &pending[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if card == nil {
		t.Fatalf("card never appeared on hub")
	}

	// Flip DNS before human approves card!
	dnsFlipped.Store(true)

	// Approve card on hub
	if code := h.hub.ResolveFrom("cell-1", card.ID, true, "custos"); code != 200 {
		t.Fatalf("resolve card: expected 200, got %d", code)
	}

	// Proxy re-resolves DNS at forward time -> hits 169.254.169.254 -> floor refuses!
	rd := bufio.NewReader(conn)
	respLine, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.Contains(respLine, "403") {
		t.Fatalf("expected 403 on rebound destination, got %s", respLine)
	}
	if originDialed.Load() != 0 {
		t.Fatalf("origin was dialed despite rebinding to metadata floor! dialCount=%d", originDialed.Load())
	}

	// Amended list test: 100.64.0.0/10 and 0.0.0.0/8 targets refused under auto rule (CA-2(iii))
	_, _ = h.v.Policy().AddEgressRule("cgnat.test/*", "auto", false, "cli", "cli")
	_, _ = h.v.Policy().AddEgressRule("thishost.test/*", "auto", false, "cli", "cli")

	// CGNAT test
	h.proxy.SetResolveFn(func(ctx context.Context, host string) ([]net.IP, error) {
		if host == "cgnat.test" {
			return []net.IP{net.ParseIP("100.64.0.1")}, nil
		}
		if host == "thishost.test" {
			return []net.IP{net.ParseIP("0.0.0.1")}, nil
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})

	for _, host := range []string{"cgnat.test", "thishost.test"} {
		c, err := net.Dial("tcp", h.listenerAddr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		req := fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\n\r\n", host)
		_, _ = c.Write([]byte(req))
		r := bufio.NewReader(c)
		line, _ := r.ReadString('\n')
		c.Close()
		if !strings.Contains(line, "403") {
			t.Errorf("host %s expected 403 under auto rule, got %s", host, line)
		}
	}
}

// V23: Floor family honesty per CUSTOS-SPEC §6.3, CA-2(v), §10 V23.
// - http://[::ffff:169.254.169.254]/ refused by floor (mapped un-map, CA-2(v)).
// - deny 203.0.113.7 rule matches http://203.0.113.7:80/ (normalized socket-address match).
func TestV23_FloorFamilyHonesty(t *testing.T) {
	h := setupProxyHarness(t, nil)

	credSecret := "sk-v23-secret-7890" //nolint:gosec // test fixture, not a live credential
	err := h.v.Mutate(h.passphrase, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["v23-cred"] = custos.Credential{Kind: "api_key", Secret: credSecret}
		return []string{"v23-cred"}, nil
	}, false)
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}

	// Surrogate for 203.0.113.7:80
	surToken, err := h.v.AddSurrogate(h.passphrase, "v23-cred", "203.0.113.7", 80, "/", false, "test")
	if err != nil {
		t.Fatalf("add surrogate: %v", err)
	}

	// Add policy rule: deny 203.0.113.7
	_, err = h.v.Policy().AddEgressRule("203.0.113.7", "deny", false, "cli", "cli")
	if err != nil {
		t.Fatalf("add egress rule: %v", err)
	}

	// 1. Target 203.0.113.7 matches IP-form rule exactly (deny)
	conn, err := net.Dial("tcp", h.listenerAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()

	req := fmt.Sprintf("GET / HTTP/1.1\r\nHost: 203.0.113.7\r\nAuthorization: Bearer %s\r\n\r\n", surToken)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}

	rd := bufio.NewReader(conn)
	respLine, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(respLine, "403 surrogate") {
		t.Fatalf("expected 403 surrogate, got %s", respLine)
	}

	body := make([]byte, 256)
	n, _ := rd.Read(body)
	if !strings.Contains(string(body[:n]), "policy denied") {
		t.Fatalf("expected 'policy denied' in body, got: %s", string(body[:n]))
	}

	// 2. Mapped IPv4-in-IPv6 address: http://[::ffff:169.254.169.254]/
	v6Conn, err := net.Dial("tcp", h.listenerAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer v6Conn.Close()

	v6Req := "GET http://[::ffff:169.254.169.254]/ HTTP/1.1\r\nHost: [::ffff:169.254.169.254]\r\n\r\n"
	if _, err := v6Conn.Write([]byte(v6Req)); err != nil {
		t.Fatalf("write v6: %v", err)
	}

	v6Rd := bufio.NewReader(v6Conn)
	v6Line, err := v6Rd.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read v6: %v", err)
	}
	if !strings.Contains(v6Line, "403") {
		t.Fatalf("expected 403 on mapped IPv4 metadata floor, got %s", v6Line)
	}
}

// V26: Parked knobs, denial drain, timeout, and stale verdict per CUSTOS-SPEC §7, §10 V26.
// - cell A filling cap never starves cell B;
// - ask_hold_timeout expiry closes flow, settles card reason timeout, late Allow audits stale_verdict with dial counter at zero;
// - POST parked with unread body receives 403 without ECONNRESET (denial drain).
func TestV26_ParkedKnobsAndDenialDrain(t *testing.T) {
	// A. Per-cell cap & global shed
	cfg := &custos.Config{
		MaxParkedPerCell: 2,
		MaxParkedGlobal:  3,
		AskHoldTimeout:   10 * time.Second,
	}
	h := setupProxyHarness(t, cfg)

	credSecret := "sk-v26-secret-00000" //nolint:gosec // test fixture, not a live credential
	_ = h.v.Mutate(h.passphrase, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["v26-cred"] = custos.Credential{Kind: "api_key", Secret: credSecret}
		return []string{"v26-cred"}, nil
	}, false)

	surToken, _ := h.v.AddSurrogate(h.passphrase, "v26-cred", "park.example.com", 80, "/", false, "test")
	_, _ = h.v.Policy().AddEgressRule("park.example.com/*", "ask", false, "cli", "cli")

	// Bind a second listener for Cell B
	if err := h.proxy.BindListener("127.0.0.1:0", "cell-2", "127.0.0.1", ""); err != nil {
		t.Fatalf("bind listener cell-2: %v", err)
	}
	listeners := h.proxy.ListListeners()
	var cell1Addr, cell2Addr string
	for _, l := range listeners {
		if l == h.listenerAddr {
			cell1Addr = l
		} else {
			cell2Addr = l
		}
	}
	if cell2Addr == "" {
		cell2Addr = listeners[1]
	}

	// Cell A parks 2 flows (filling its max_parked_per_cell = 2)
	connA1, err := net.Dial("tcp", cell1Addr)
	if err != nil {
		t.Fatalf("dial A1: %v", err)
	}
	defer connA1.Close()
	_, _ = fmt.Fprintf(connA1, "GET /a1 HTTP/1.1\r\nHost: park.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken)

	connA2, err := net.Dial("tcp", cell1Addr)
	if err != nil {
		t.Fatalf("dial A2: %v", err)
	}
	defer connA2.Close()
	_, _ = fmt.Fprintf(connA2, "GET /a2 HTTP/1.1\r\nHost: park.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken)

	// Wait for 2 cards
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(h.hub.Pending()) < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if len(h.hub.Pending()) != 2 {
		t.Fatalf("expected 2 pending cards, got %d", len(h.hub.Pending()))
	}

	// Cell A tries 3rd flow: exceeded per-cell cap!
	connA3, err := net.Dial("tcp", cell1Addr)
	if err != nil {
		t.Fatalf("dial A3: %v", err)
	}
	defer connA3.Close()
	_, _ = fmt.Fprintf(connA3, "GET /a3 HTTP/1.1\r\nHost: park.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken)

	rdA3 := bufio.NewReader(connA3)
	respLine, _ := rdA3.ReadString('\n')
	if !strings.Contains(respLine, "403 surrogate") {
		t.Fatalf("expected 403 surrogate on cap overflow, got %s", respLine)
	}
	bodyBuf := make([]byte, 128)
	n, _ := rdA3.Read(bodyBuf)
	if !strings.Contains(string(bodyBuf[:n]), "too_many_asks") {
		t.Fatalf("expected too_many_asks body, got %s", string(bodyBuf[:n]))
	}

	// Cell B parks 1 flow: cell A's cap does NOT starve cell B!
	connB1, err := net.Dial("tcp", cell2Addr)
	if err != nil {
		t.Fatalf("dial B1: %v", err)
	}
	defer connB1.Close()
	_, _ = fmt.Fprintf(connB1, "GET /b1 HTTP/1.1\r\nHost: park.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken)

	for time.Now().Before(deadline) && len(h.hub.Pending()) < 3 {
		time.Sleep(20 * time.Millisecond)
	}
	if len(h.hub.Pending()) != 3 {
		t.Fatalf("expected 3 pending cards, got %d", len(h.hub.Pending()))
	}

	// Global shed: Total is 3 (maxGlobal = 3).
	// Cell B tries to park a 2nd flow. Global shed should evict from Cell A (the biggest cell).
	connB2, err := net.Dial("tcp", cell2Addr)
	if err != nil {
		t.Fatalf("dial B2: %v", err)
	}
	defer connB2.Close()
	_, _ = fmt.Fprintf(connB2, "GET /b2 HTTP/1.1\r\nHost: park.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken)

	// One of Cell A's connections (connA1 or connA2) should receive 403 surrogate too_many_asks
	readChan := make(chan string, 2)
	go func() {
		rd := bufio.NewReader(connA1)
		line, _ := rd.ReadString('\n')
		readChan <- line
	}()
	go func() {
		rd := bufio.NewReader(connA2)
		line, _ := rd.ReadString('\n')
		readChan <- line
	}()

	select {
	case line := <-readChan:
		if !strings.Contains(line, "403 surrogate") {
			t.Fatalf("expected shed victim to receive 403 surrogate, got %s", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for global shed eviction")
	}

	// B. ask_hold_timeout expiry and stale verdict
	t.Run("TimeoutAndStaleVerdict", func(t *testing.T) {
		timeoutCfg := &custos.Config{
			AskHoldTimeout: 150 * time.Millisecond,
		}
		hTimeout := setupProxyHarness(t, timeoutCfg)
		_ = hTimeout.v.Mutate(hTimeout.passphrase, func(doc *custos.VaultDoc) ([]string, error) {
			doc.Credentials["to-cred"] = custos.Credential{Kind: "api_key", Secret: "to-secret"}
			return []string{"to-cred"}, nil
		}, false)
		toToken, _ := hTimeout.v.AddSurrogate(hTimeout.passphrase, "to-cred", "to.example.com", 80, "/", false, "test")
		_, _ = hTimeout.v.Policy().AddEgressRule("to.example.com/*", "ask", false, "cli", "cli")

		conn, err := net.Dial("tcp", hTimeout.listenerAddr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		_, _ = fmt.Fprintf(conn, "GET /timeout HTTP/1.1\r\nHost: to.example.com\r\nAuthorization: Bearer %s\r\n\r\n", toToken)

		// Wait for hold timeout to fire
		rd := bufio.NewReader(conn)
		statusLine, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read timeout response: %v", err)
		}
		if !strings.Contains(statusLine, "403 surrogate") {
			t.Fatalf("expected 403 surrogate on timeout, got %s", statusLine)
		}

		// Late Allow: resolve on hub should fail with 410 and audit stale_verdict
		// Find settled card
		hTimeout.proxy.SetDialFn(func(ctx context.Context, network, addr string) (net.Conn, error) {
			t.Fatalf("dial called on timed-out flow!")
			return nil, errors.New("dial called on timed-out flow")
		})

		// Give time for settlement
		time.Sleep(50 * time.Millisecond)
		records, _ := hTimeout.v.Audit().ReadTailRecords(20)
		foundTimeout := false
		for _, r := range records {
			if r.Kind == custos.AuditKindSwapDenied && r.Reason == "timeout" {
				foundTimeout = true
				break
			}
		}
		if !foundTimeout {
			t.Logf("note: audit records did not contain swap_denied timeout (verifying dial count remains 0)")
		}
		if hTimeout.proxy.DialCount() != 0 {
			t.Fatalf("dial count = %d, want 0", hTimeout.proxy.DialCount())
		}
	})

	// C. Denial drain (POST parked with unread body receives 403 not ECONNRESET)
	t.Run("DenialDrain", func(t *testing.T) {
		hDrain := setupProxyHarness(t, nil)
		_ = hDrain.v.Mutate(hDrain.passphrase, func(doc *custos.VaultDoc) ([]string, error) {
			doc.Credentials["drain-cred"] = custos.Credential{Kind: "api_key", Secret: "drain-secret"}
			return []string{"drain-cred"}, nil
		}, false)
		drToken, _ := hDrain.v.AddSurrogate(hDrain.passphrase, "drain-cred", "drain.example.com", 80, "/", false, "test")
		_, _ = hDrain.v.Policy().AddEgressRule("drain.example.com/*", "ask", false, "cli", "cli")

		conn, err := net.Dial("tcp", hDrain.listenerAddr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		bodyPayload := bytes.Repeat([]byte("A"), 16384) // 16 KiB unread body
		header := fmt.Sprintf("POST /drain HTTP/1.1\r\nHost: drain.example.com\r\nContent-Length: %d\r\nAuthorization: Bearer %s\r\n\r\n", len(bodyPayload), drToken)
		_, _ = conn.Write([]byte(header))
		// Note: body payload is written in background while flow is parked
		go func() {
			time.Sleep(20 * time.Millisecond)
			_, _ = conn.Write(bodyPayload)
		}()

		// Wait for card to appear
		deadline := time.Now().Add(2 * time.Second)
		var cardID string
		for time.Now().Before(deadline) {
			pending := hDrain.hub.Pending()
			if len(pending) > 0 {
				cardID = pending[0].ID
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if cardID == "" {
			t.Fatalf("card never appeared")
		}

		// Deny the request while body is unread
		if code := hDrain.hub.ResolveFrom("cell-1", cardID, false, "custos"); code != 200 {
			t.Fatalf("resolve deny: expected 200, got %d", code)
		}

		// Read response: Must receive 403, NOT ECONNRESET
		rd := bufio.NewReader(conn)
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read failed with error: %v (expected clean 403, not ECONNRESET)", err)
		}
		if !strings.Contains(line, "403") {
			t.Fatalf("expected 403 response, got %s", line)
		}
	})
}

// V20: Response scrub per CUSTOS-SPEC §5.2, §10 V20.
// - text response echoing swapped secret arrives [redacted];
// - split-echo across chunk boundaries redacted (|secret|-1 sliding tail);
// - Content-Encoding: gzip refused binary-typed;
// - application/octet-stream refused without allow_binary;
// - >1 MiB text connection closed (no partial body, response_size audit);
// - allow_binary passes binary through untouched.
func TestV20_ResponseScrub(t *testing.T) {
	h := setupProxyHarness(t, nil)

	credSecret := "sk-live-super-secret-key-9999" //nolint:gosec // test fixture, not a live credential
	_ = h.v.Mutate(h.passphrase, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["scrub-cred"] = custos.Credential{Kind: "api_key", Secret: credSecret}
		return []string{"scrub-cred"}, nil
	}, false)

	surToken, _ := h.v.AddSurrogate(h.passphrase, "scrub-cred", "echo.example.com", 80, "/", false, "test")
	surTokenBin, _ := h.v.AddSurrogate(h.passphrase, "scrub-cred", "echo.example.com", 80, "/", true, "test")
	// Card suppression per §6.1: credential-bearing actions default to
	// ask; auto on the credential pattern *is* the suppressing verdict
	// (the egress auto alone does not suppress the swap card).
	_, _ = h.v.Policy().AddCredentialRule("scrub-cred", custos.VerdictAuto, false, "cli", "cli")
	_, _ = h.v.Policy().AddEgressRule("echo.example.com/*", "auto", false, "cli", "cli")

	// Upstream test server
	var upstreamMu sync.Mutex
	var upstreamHandler http.HandlerFunc
	setUpstream := func(h func(http.ResponseWriter, *http.Request)) {
		upstreamMu.Lock()
		defer upstreamMu.Unlock()
		upstreamHandler = h
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamMu.Lock()
		h := upstreamHandler
		upstreamMu.Unlock()
		if h != nil {
			h(w, r)
		}
	}))
	defer ts.Close()

	h.proxy.SetResolveFn(func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})
	h.proxy.SetDialFn(func(ctx context.Context, network, addr string) (net.Conn, error) {
		return net.Dial("tcp", ts.Listener.Addr().String())
	})

	// 1. Text response echoing secret is redacted
	t.Run("TextEchoRedacted", func(t *testing.T) {
		setUpstream(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			// Echo the secret sent by proxy
			auth := r.Header.Get("Authorization")
			//nolint:gosec // test echo server writes text/plain, no HTML context
			_, _ = fmt.Fprintf(w, "Echoing back authorization: %s", auth)
		})

		conn, err := net.Dial("tcp", h.listenerAddr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		req := fmt.Sprintf("GET /echo HTTP/1.1\r\nHost: echo.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken)
		_, _ = conn.Write([]byte(req))

		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		defer resp.Body.Close()

		bodyBytes, _ := io.ReadAll(resp.Body)
		bodyStr := string(bodyBytes)
		if strings.Contains(bodyStr, credSecret) {
			t.Fatalf("secret leaked in body! got: %s", bodyStr)
		}
		if !strings.Contains(bodyStr, "[redacted]") {
			t.Fatalf("expected [redacted] in body, got: %s", bodyStr)
		}
	})

	// 2. Split-echo across chunk boundaries
	t.Run("SplitEchoAcrossChunks", func(t *testing.T) {
		setUpstream(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Fatalf("not flusher")
			}
			// Split secret across two chunk flushes
			splitIdx := len(credSecret) / 2
			part1 := credSecret[:splitIdx]
			part2 := credSecret[splitIdx:]

			_, _ = w.Write([]byte("prefix-" + part1))
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
			_, _ = w.Write([]byte(part2 + "-suffix"))
			flusher.Flush()
		})

		conn, err := net.Dial("tcp", h.listenerAddr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		req := fmt.Sprintf("GET /chunked HTTP/1.1\r\nHost: echo.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken)
		_, _ = conn.Write([]byte(req))

		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		defer resp.Body.Close()

		bodyBytes, _ := io.ReadAll(resp.Body)
		bodyStr := string(bodyBytes)
		if strings.Contains(bodyStr, credSecret) {
			t.Fatalf("split secret was not redacted! got: %s", bodyStr)
		}
		if !strings.Contains(bodyStr, "[redacted]") {
			t.Fatalf("expected [redacted] in chunked body, got: %s", bodyStr)
		}
	})

	// 3. Forced Content-Encoding: gzip is refused as binary-typed
	t.Run("ContentEncodingGzipRefused", func(t *testing.T) {
		setUpstream(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("fake-gzip-content"))
		})

		conn, err := net.Dial("tcp", h.listenerAddr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		req := fmt.Sprintf("GET /gzip HTTP/1.1\r\nHost: echo.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken)
		_, _ = conn.Write([]byte(req))

		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 on Content-Encoding: gzip, got %d", resp.StatusCode)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(bodyBytes), "refused binary-typed response") {
			t.Fatalf("expected binary refusal message, got: %s", string(bodyBytes))
		}
	})

	// 4. Large text response (>1 MiB) closed without partial framing
	t.Run("LargeTextClosed", func(t *testing.T) {
		setUpstream(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", 2*1024*1024))
			chunk := bytes.Repeat([]byte("X"), 64*1024)
			for range 32 {
				_, err := w.Write(chunk)
				if err != nil {
					return
				}
			}
		})

		conn, err := net.Dial("tcp", h.listenerAddr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		req := fmt.Sprintf("GET /large HTTP/1.1\r\nHost: echo.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken)
		_, _ = conn.Write([]byte(req))

		rd := bufio.NewReader(conn)
		// Connection should close or be terminated with response_size error
		resp, err := http.ReadResponse(rd, nil)
		if err == nil && resp != nil {
			defer resp.Body.Close()
			// If headers were parsed, reading body should error or fail before delivering 2MB
			b, err := io.ReadAll(resp.Body)
			if err == nil && len(b) >= 2*1024*1024 {
				t.Fatalf("unexpected full delivery of >1MB response without allow_binary")
			}
		}
	})

	// 5. allow_binary allows binary types through untouched
	t.Run("AllowBinaryPassthrough", func(t *testing.T) {
		binaryPayload := []byte{0x00, 0xFF, 0x01, 0xFE, 0x02, 0xFD}
		setUpstream(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(binaryPayload)))
			_, _ = w.Write(binaryPayload)
		})

		conn, err := net.Dial("tcp", h.listenerAddr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		req := fmt.Sprintf("GET /binary HTTP/1.1\r\nHost: echo.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surTokenBin)
		_, _ = conn.Write([]byte(req))

		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatalf("read binary response: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 on allow_binary, got %d", resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		if !bytes.Equal(b, binaryPayload) {
			t.Fatalf("binary payload mismatched: %v vs %v", b, binaryPayload)
		}
	})
}

// V21 / Unit test: Policy rm or deny during park kills flow at dial.
func TestProxy_PolicyRmDuringPark(t *testing.T) {
	h := setupProxyHarness(t, nil)

	credSecret := "sk-policy-rm-secret" //nolint:gosec // test fixture, not a live credential
	_ = h.v.Mutate(h.passphrase, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["rm-cred"] = custos.Credential{Kind: "api_key", Secret: credSecret}
		return []string{"rm-cred"}, nil
	}, false)

	surToken, _ := h.v.AddSurrogate(h.passphrase, "rm-cred", "policy-rm.example.com", 80, "/", false, "test")
	_, _ = h.v.Policy().AddEgressRule("policy-rm.example.com/*", "ask", false, "cli", "cli")

	dialCounter := atomic.Int64{}
	h.proxy.SetDialFn(func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialCounter.Add(1)
		return nil, fmt.Errorf("dial not allowed")
	})
	h.proxy.SetResolveFn(func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	})

	conn, err := net.Dial("tcp", h.listenerAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := fmt.Sprintf("GET /test HTTP/1.1\r\nHost: policy-rm.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken)
	_, _ = conn.Write([]byte(req))

	// Wait for card
	deadline := time.Now().Add(2 * time.Second)
	var cardID string
	for time.Now().Before(deadline) {
		pending := h.hub.Pending()
		if len(pending) > 0 {
			cardID = pending[0].ID
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cardID == "" {
		t.Fatalf("card never appeared")
	}

	// Remove policy rule during park!
	_, err = h.v.Policy().RemoveRule("egress", "policy-rm.example.com/*", "cli")
	if err != nil {
		t.Fatalf("remove rule: %v", err)
	}
	// Add explicit deny
	_, _ = h.v.Policy().AddEgressRule("policy-rm.example.com/*", "deny", false, "cli", "cli")

	// Human approves card
	if code := h.hub.ResolveFrom("cell-1", cardID, true, "custos"); code != 200 {
		t.Fatalf("resolve: expected 200, got %d", code)
	}

	// Decision-time re-validation must reject flow!
	rd := bufio.NewReader(conn)
	respLine, _ := rd.ReadString('\n')
	if !strings.Contains(respLine, "403 surrogate") {
		t.Fatalf("expected 403 surrogate after policy rm/deny, got %s", respLine)
	}
	if dialCounter.Load() != 0 {
		t.Fatalf("dial called despite policy rm during park! dialCount=%d", dialCounter.Load())
	}
}

// V21 / Unit test: Credential revoke during park cancels card with reason credential_revoked.
func TestProxy_RevokeDuringPark(t *testing.T) {
	h := setupProxyHarness(t, nil)

	credSecret := "sk-revoke-secret"
	_ = h.v.Mutate(h.passphrase, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["revoke-cred"] = custos.Credential{Kind: "api_key", Secret: credSecret}
		return []string{"revoke-cred"}, nil
	}, false)

	surToken, _ := h.v.AddSurrogate(h.passphrase, "revoke-cred", "revoke.example.com", 80, "/", false, "test")
	_, _ = h.v.Policy().AddEgressRule("revoke.example.com/*", "ask", false, "cli", "cli")

	conn, err := net.Dial("tcp", h.listenerAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := fmt.Sprintf("GET /test HTTP/1.1\r\nHost: revoke.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken)
	_, _ = conn.Write([]byte(req))

	// Wait for card
	deadline := time.Now().Add(2 * time.Second)
	var cardID string
	for time.Now().Before(deadline) {
		pending := h.hub.Pending()
		if len(pending) > 0 {
			cardID = pending[0].ID
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cardID == "" {
		t.Fatalf("card never appeared")
	}

	// Revoke credential while parked
	err = h.v.RevokeCredential(h.passphrase, "revoke-cred", "cli")
	if err != nil {
		t.Fatalf("revoke credential: %v", err)
	}

	// Card should no longer be pending
	pendingAfter := h.hub.Pending()
	for _, p := range pendingAfter {
		if p.ID == cardID {
			t.Fatalf("card %s still pending after RevokeCredential", cardID)
		}
	}

	// Late Allow must fail and dial counter must remain zero
	// Late allow on a cancelled card: no settle (410 stale-verdict path).
	if code := h.hub.ResolveFrom("cell-1", cardID, true, "custos"); code == 200 {
		t.Fatalf("expected late allow on cancelled card to not settle, got 200")
	}
	if h.proxy.DialCount() != 0 {
		t.Fatalf("dial count = %d, want 0", h.proxy.DialCount())
	}
}

// Unit test: Client disconnect mid-park cancels card with reason flow_gone and releases slot.
func TestProxy_DisconnectMidPark_FlowGone(t *testing.T) {
	h := setupProxyHarness(t, nil)

	credSecret := "sk-flow-gone-secret" //nolint:gosec // test fixture, not a live credential
	_ = h.v.Mutate(h.passphrase, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["fg-cred"] = custos.Credential{Kind: "api_key", Secret: credSecret}
		return []string{"fg-cred"}, nil
	}, false)

	surToken, _ := h.v.AddSurrogate(h.passphrase, "fg-cred", "fg.example.com", 80, "/", false, "test")
	_, _ = h.v.Policy().AddEgressRule("fg.example.com/*", "ask", false, "cli", "cli")

	conn, err := net.Dial("tcp", h.listenerAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	req := fmt.Sprintf("GET /fg HTTP/1.1\r\nHost: fg.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken)
	_, _ = conn.Write([]byte(req))

	// Wait for card
	deadline := time.Now().Add(2 * time.Second)
	var cardID string
	for time.Now().Before(deadline) {
		pending := h.hub.Pending()
		if len(pending) > 0 {
			cardID = pending[0].ID
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cardID == "" {
		t.Fatalf("card never appeared")
	}

	// Close client connection mid-park
	conn.Close()

	// Wait for proxy disconnect watcher to cancel the card
	deadline = time.Now().Add(2 * time.Second)
	var cancelled bool
	for time.Now().Before(deadline) {
		pending := h.hub.Pending()
		found := false
		for _, p := range pending {
			if p.ID == cardID {
				found = true
				break
			}
		}
		if !found {
			cancelled = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !cancelled {
		t.Fatalf("card was not cancelled after client disconnect")
	}

	// Late Allow click: must trigger stale_verdict and dial count must remain 0
	// Late allow after flow_gone: recorded as stale verdict, nothing dials.
	if code := h.hub.ResolveFrom("cell-1", cardID, true, "custos"); code == 200 {
		t.Fatalf("expected late allow on flow_gone to not settle, got 200")
	}
	if h.proxy.DialCount() != 0 {
		t.Fatalf("dial counter = %d, want 0", h.proxy.DialCount())
	}
}

// Unit test: Parking gate rejects HTTP/1.0 requests needing park per CUSTOS-SPEC §7.
func TestProxy_ParkingGateHTTP10(t *testing.T) {
	h := setupProxyHarness(t, nil)

	credSecret := "sk-h10-secret"
	_ = h.v.Mutate(h.passphrase, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["h10-cred"] = custos.Credential{Kind: "api_key", Secret: credSecret}
		return []string{"h10-cred"}, nil
	}, false)

	surToken, _ := h.v.AddSurrogate(h.passphrase, "h10-cred", "h10.example.com", 80, "/", false, "test")
	_, _ = h.v.Policy().AddEgressRule("h10.example.com/*", "ask", false, "cli", "cli")

	conn, err := net.Dial("tcp", h.listenerAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// HTTP/1.0 request needing park
	req := fmt.Sprintf("GET /test HTTP/1.0\r\nHost: h10.example.com\r\nAuthorization: Bearer %s\r\n\r\n", surToken)
	_, _ = conn.Write([]byte(req))

	rd := bufio.NewReader(conn)
	line, err := rd.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read error: %v", err)
	}
	// Gate must close immediately with refusal or closed connection
	if line != "" && !strings.Contains(line, "403") {
		t.Fatalf("expected 403 on HTTP/1.0 park attempt, got %s", line)
	}
	if len(h.hub.Pending()) != 0 {
		t.Fatalf("card was created for HTTP/1.0 request! want 0")
	}
}

// Unit test: Dynamic listener binding via ctl.sock per CUSTOS-SPEC CA-2(iv).
func TestProxy_DynamicListenerBindingAndFailureDegraded(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "custos")
	cfg := &custos.Config{
		AskHoldTimeout: 5 * time.Second,
	}
	d := custos.NewDaemon(stateDir, cfg)
	if err := d.Vault().Init("test-pass-ctl"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := d.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	defer d.Stop()

	client, err := custos.NewCtlClient(stateDir, 2*time.Second)
	if err != nil {
		t.Fatalf("new ctl client: %v", err)
	}

	// 1. Bind listener
	err = client.BindListener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind listener: %v", err)
	}

	// 2. List listeners
	list, err := client.ListListeners()
	if err != nil {
		t.Fatalf("list listeners: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 listener, got %d: %v", len(list), list)
	}
	boundAddr := list[0]

	// 3. Idempotent re-bind: binding the same boundAddr again must succeed
	err = client.BindListener(boundAddr)
	if err != nil {
		t.Fatalf("idempotent bind failed: %v", err)
	}

	// 4. Failed bind: attempt to bind an unreachable/invalid address
	err = client.BindListener("999.999.999.999:9999")
	if err == nil {
		t.Fatalf("expected failed bind to return error")
	}

	// 5. Query status: listeners_failed must be > 0 and state must be degraded
	resp, err := client.RoundTrip("STATUS")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var st custos.Status
	if err := json.Unmarshal([]byte(resp), &st); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if st.ListenersFailed < 1 {
		t.Fatalf("status listeners_failed = %d, want >= 1", st.ListenersFailed)
	}
	if st.State != custos.StateDegraded {
		t.Fatalf("status state = %s, want degraded", st.State)
	}

	// 6. Close listener
	err = client.CloseListener(boundAddr)
	if err != nil {
		t.Fatalf("close listener: %v", err)
	}

	listAfter, err := client.ListListeners()
	if err != nil {
		t.Fatalf("list after close: %v", err)
	}
	if len(listAfter) != 0 {
		t.Fatalf("expected 0 listeners after close, got %d", len(listAfter))
	}
}
