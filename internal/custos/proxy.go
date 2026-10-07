// Egress proxy custody and bearer lane per CUSTOS-SPEC §7, CA-2
// amendments, §5.2 (bearer lane pipeline), §6.4 (ask wiring for
// egress), and §8.1 (audit events).

package custos

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lararium-app/lararium/internal/proxy"
	"github.com/lararium-app/lararium/internal/surface"
)

// Frozen proxy and relay limits per CUSTOS-SPEC §7, CA-2, CELL-SPEC §4.
const (
	// MaxConnsPerCell caps simultaneous proxied connections per cell listener.
	MaxConnsPerCell = proxy.MaxConnsPerCell
	// IdleTimeout bounds one direction's silence during relay.
	IdleTimeout = proxy.IdleTimeout
	// RequestPhaseTimeout bounds the request line and headers read phase.
	RequestPhaseTimeout = proxy.RequestPhaseTimeout

	// maxScannedBody is the 1 MiB scan ceiling for response secret scrub per CUSTOS-SPEC §5.2.
	maxScannedBody = 1024 * 1024
	// drainWindowBytes is the 64 KiB read cap for denial delivery per CUSTOS-SPEC §7.
	drainWindowBytes = 64 * 1024
	// drainWindowTimeout is the 250 ms bounded drain window per CUSTOS-SPEC §7.
	drainWindowTimeout = 250 * time.Millisecond
)

// DialUpstreamFunc represents the dial function signature for upstream connections.
type DialUpstreamFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// ResolveIPsFunc represents the DNS resolution function signature.
type ResolveIPsFunc func(ctx context.Context, host string) ([]net.IP, error)

// Proxy manages per-cell HTTP/1.1 forward proxy listeners and executes the
// bearer lane surrogate swap and egress custody pipeline per CUSTOS-SPEC §7, CA-2.
type Proxy struct {
	vault           *Vault
	hub             *surface.ApprovalHub
	cfg             *Config
	mu              sync.RWMutex
	listeners       map[string]*cellListener
	listenersFailed int
	parkMgr         *parkManager
	dialFn          DialUpstreamFunc
	resolveFn       ResolveIPsFunc
	dialCounter     int64
	closed          bool
	extraHeaders    []string
}

// cellListener represents one dynamic per-cell gateway proxy listener (10.91.<n>.1:<port>).
type cellListener struct {
	proxy   *Proxy
	addr    string
	cellID  string
	peer    string
	logPath string
	ln      net.Listener
	done    chan struct{}
	conns   int
	mu      sync.Mutex
	stopped bool
	wg      sync.WaitGroup
}

// parkedFlow tracks an in-flight HTTP request parked in ask state per CUSTOS-SPEC §7.
type parkedFlow struct {
	id         string
	cellID     string
	cred       string
	host       string
	port       int
	path       string
	surToken   string
	review     string
	decisionCh <-chan bool
	outcomeCh  chan string
	createdAt  time.Time
	cancelled  atomic.Bool
}

// parkManager tracks parked flows with per-cell limits and fair global shedding per CUSTOS-SPEC §7.
type parkManager struct {
	mu          sync.Mutex
	maxPerCell  int
	maxGlobal   int
	holdTimeout time.Duration
	byCell      map[string]map[string]*parkedFlow
	totalParked int
}

func newParkManager(maxPerCell, maxGlobal int, holdTimeout time.Duration) *parkManager {
	if maxPerCell <= 0 {
		maxPerCell = DefaultMaxParkedPerCell
	}
	if maxGlobal <= 0 {
		maxGlobal = DefaultMaxParkedGlobal
	}
	if holdTimeout <= 0 {
		holdTimeout = DefaultAskHoldTimeout
	}
	return &parkManager{
		maxPerCell:  maxPerCell,
		maxGlobal:   maxGlobal,
		holdTimeout: holdTimeout,
		byCell:      make(map[string]map[string]*parkedFlow),
	}
}

// NewProxy creates a new custody Proxy instance.
func NewProxy(vault *Vault, hub *surface.ApprovalHub, cfg *Config) *Proxy {
	if cfg == nil {
		cfg = &Config{}
	}
	cfg.Normalize()

	pm := newParkManager(cfg.MaxParkedPerCell, cfg.MaxParkedGlobal, cfg.AskHoldTimeout)
	p := &Proxy{
		vault:        vault,
		hub:          hub,
		cfg:          cfg,
		listeners:    make(map[string]*cellListener),
		parkMgr:      pm,
		extraHeaders: cfg.HeaderExtras,
	}

	if hub != nil {
		// Wire stale verdict hook per CUSTOS-SPEC §7:
		// "An Allow that arrives after the flow is gone is recorded on the hub as a settle against
		// a dead flow (audit kind stale_verdict; nothing dials; the decision is recorded, not re-judged)."
		hub.SetStaleVerdictHook(func(id, sessionID, cred, cell, reason string) {
			if p.vault != nil && p.vault.Audit() != nil {
				_ = p.vault.Audit().Append(AuditRecord{
					Kind:   AuditKindStaleVerdict,
					Cred:   cred,
					Actor:  cell,
					Reason: reason,
				})
			}
		})
	}

	return p
}

// SetDialFn overrides the network dialer seam (used in testing for fake upstreams).
func (p *Proxy) SetDialFn(fn DialUpstreamFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dialFn = fn
}

// SetResolveFn overrides the DNS resolution seam (used in testing for rebinding and fake origins).
func (p *Proxy) SetResolveFn(fn ResolveIPsFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resolveFn = fn
}

// DialCount returns the count of upstream dials attempted.
func (p *Proxy) DialCount() int64 {
	return atomic.LoadInt64(&p.dialCounter)
}

// ListenersFailed returns the count of failed listener binds per CUSTOS-SPEC §11.
func (p *Proxy) ListenersFailed() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.listenersFailed
}

// ListenerAddr returns the actual bound net.Addr string for the given address key.
func (p *Proxy) ListenerAddr(addr string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if cl, ok := p.listeners[addr]; ok && cl.ln != nil {
		return cl.ln.Addr().String()
	}
	return ""
}

// ListListeners returns a sorted list of currently bound cell listener addresses.
func (p *Proxy) ListListeners() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	seen := make(map[string]struct{})
	var out []string
	for _, cl := range p.listeners {
		if cl.ln != nil {
			a := cl.ln.Addr().String()
			if _, ok := seen[a]; !ok {
				seen[a] = struct{}{}
				out = append(out, a)
			}
		}
	}
	sort.Strings(out)
	return out
}

// derivePeerAndCell determines the expected peer address and cellID from the listen address.
// Cell gateway is 10.91.<n>.1:<port>; peer is 10.91.<n>.2 per CELL-SPEC §4, CA-2(iv).
func derivePeerAndCell(listenAddr, cellID, peer string) (derivedCellID, derivedPeer string) {
	if cellID != "" && peer != "" {
		return cellID, peer
	}
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		host = listenAddr
	}
	if peer == "" {
		switch {
		case strings.HasPrefix(host, "10.91."):
			parts := strings.Split(host, ".")
			if len(parts) == 4 && parts[3] == "1" {
				peer = fmt.Sprintf("10.91.%s.2", parts[2])
			} else {
				peer = host
			}
		case host == "127.0.0.1" || host == "localhost":
			peer = "127.0.0.1"
		default:
			peer = host
		}
	}
	if cellID == "" {
		if strings.HasPrefix(host, "10.91.") {
			parts := strings.Split(host, ".")
			if len(parts) == 4 {
				cellID = fmt.Sprintf("cell-%s", parts[2])
			} else {
				cellID = "cell-default"
			}
		} else {
			cellID = "cell-default"
		}
	}
	return cellID, peer
}

// BindListener binds a per-cell proxy listener dynamically per CUSTOS-SPEC CA-2(iv).
// The bind handler is idempotent: already bound addresses ack success without a second bind.
// Failed binds are retried briefly, then audited listener_bind_failed and report listeners_failed.
func (p *Proxy) BindListener(addr string, cellID, peer, logPath string) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("proxy closed")
	}
	if !strings.HasSuffix(addr, ":0") {
		if _, exists := p.listeners[addr]; exists {
			// Idempotent: ack success without a second bind per CA-2(iv)
			p.mu.Unlock()
			return nil
		}
	}
	p.mu.Unlock()

	cID, pAddr := derivePeerAndCell(addr, cellID, peer)
	if logPath == "" && p.vault != nil {
		logPath = filepath.Join(p.vault.StateDir(), "access.log")
	}

	// Retry the bind briefly before auditing listener_bind_failed per CA-2(iv)
	var ln net.Listener
	var bindErr error
	for range 3 {
		var lc net.ListenConfig
		ln, bindErr = lc.Listen(context.Background(), "tcp", addr)
		if bindErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if bindErr != nil {
		p.mu.Lock()
		p.listenersFailed++
		p.mu.Unlock()

		if p.vault != nil && p.vault.Audit() != nil {
			_ = p.vault.Audit().Append(AuditRecord{
				Kind:   AuditKindListenerBindFailed,
				Host:   addr,
				Reason: bindErr.Error(),
				Actor:  "daemon",
			})
		}
		return fmt.Errorf("proxy bind %s: %w", addr, bindErr)
	}

	boundAddr := ln.Addr().String()
	cl := &cellListener{
		proxy:   p,
		addr:    boundAddr,
		cellID:  cID,
		peer:    pAddr,
		logPath: logPath,
		ln:      ln,
		done:    make(chan struct{}),
	}

	p.mu.Lock()
	p.listeners[boundAddr] = cl
	if addr != boundAddr && !strings.HasSuffix(addr, ":0") {
		p.listeners[addr] = cl
	}
	p.mu.Unlock()

	go cl.serve()
	return nil
}

// CloseListener closes and removes a per-cell listener by address per CUSTOS-SPEC CA-2(iv).
func (p *Proxy) CloseListener(addr string) error {
	p.mu.Lock()
	cl, exists := p.listeners[addr]
	if !exists {
		p.mu.Unlock()
		return nil
	}
	for k, v := range p.listeners {
		if v == cl {
			delete(p.listeners, k)
		}
	}
	p.mu.Unlock()

	return cl.close()
}

// Close terminates all listeners and releases resources.
func (p *Proxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	seen := make(map[*cellListener]struct{})
	var listeners []*cellListener
	for _, l := range p.listeners {
		if _, ok := seen[l]; !ok {
			seen[l] = struct{}{}
			listeners = append(listeners, l)
		}
	}
	p.listeners = make(map[string]*cellListener)
	p.mu.Unlock()

	var firstErr error
	for _, l := range listeners {
		if err := l.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (cl *cellListener) close() error {
	err := cl.ln.Close()
	cl.mu.Lock()
	if !cl.stopped {
		cl.stopped = true
		close(cl.done)
	}
	cl.mu.Unlock()
	// Wait for in-flight handlers so their final audit writes land
	// before the process/caller tears state down (temp dirs, log tails).
	cl.wg.Wait()
	return err
}

func (cl *cellListener) serve() {
	for {
		conn, err := cl.ln.Accept()
		if err != nil {
			select {
			case <-cl.done:
				return
			default:
				return
			}
		}
		cl.mu.Lock()
		if cl.stopped {
			cl.mu.Unlock()
			_ = conn.Close()
			return
		}
		cl.wg.Add(1)
		cl.mu.Unlock()
		if !cl.admit(conn) {
			cl.wg.Done()
			continue
		}
		go func() {
			defer cl.wg.Done()
			cl.handle(conn)
		}()
	}
}

// admit validates the peer address and per-cell connection limit per CELL-SPEC §4.
func (cl *cellListener) admit(conn net.Conn) bool {
	peerIP := proxy.IPOnly(conn.RemoteAddr().String())
	if cl.peer != "" && peerIP != cl.peer {
		proxy.RstHangup(conn)
		_ = conn.Close()
		return false
	}

	cl.mu.Lock()
	full := cl.conns >= MaxConnsPerCell
	if !full {
		cl.conns++
	}
	cl.mu.Unlock()

	if full {
		proxy.RstHangup(conn)
		_ = conn.Close()
		return false
	}
	return true
}

func (cl *cellListener) release() {
	cl.mu.Lock()
	cl.conns--
	cl.mu.Unlock()
}

// logEntry appends one line to the access log per CELL-SPEC §4.
func (cl *cellListener) logEntry(remote fmt.Stringer, method, host, outcome string) {
	if cl.logPath == "" {
		return
	}
	f, err := os.OpenFile(cl.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()

	_, _ = fmt.Fprintf(f, "%s %s %s %s %s\n",
		time.Now().UTC().Format(time.RFC3339), proxy.IPOnly(remote.String()), method, host, outcome)
}

// handle coordinates request processing on an accepted connection.
func (cl *cellListener) handle(conn net.Conn) {
	defer cl.release()

	br := bufio.NewReaderSize(conn, proxy.MaxConnsPerCell*32)
	_ = conn.SetReadDeadline(time.Now().Add(RequestPhaseTimeout))

	line, err := proxy.ReadLineLimited(br)
	if err != nil {
		proxy.RstHangup(conn)
		_ = conn.Close()
		return
	}

	method, target, version, ok := proxy.ParseRequestLine(line)
	if !ok {
		proxy.RstHangup(conn)
		_ = conn.Close()
		return
	}

	headers, err := proxy.ReadHeaders(br)
	if err != nil {
		proxy.RstHangup(conn)
		_ = conn.Close()
		return
	}

	// Relax read deadline to idle window
	_ = conn.SetReadDeadline(time.Now().Add(IdleTimeout))

	ctx := context.Background()

	// 1. CONNECT blind passthrough per CUSTOS-SPEC §5.2, §7, §10 V10:
	// "TLS (CONNECT) is blind passthrough: no swap, documented, asserted by V10 so the honesty
	// claim can't rot. Terminal for v1: CONNECT gets no card, no policy consult, and no terminal
	// audit event beyond the floor check".
	if method == http.MethodConnect {
		if !proxy.ValidHostPort(target) {
			proxy.RstHangup(conn)
			_ = conn.Close()
			return
		}
		cl.forwardCONNECT(ctx, conn, br, target)
		return
	}

	// 2. Plaintext HTTP validation: absolute-form URI or origin-form with Host header per §7 parking gate
	var u *url.URL
	if strings.HasPrefix(target, "/") {
		// Origin-form request: take authority from Host header
		var hostHdr string
		for _, h := range headers {
			colon := strings.IndexByte(h, ':')
			if colon < 0 {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(h[:colon]), "host") {
				hostHdr = strings.TrimSpace(h[colon+1:])
				break
			}
		}
		if hostHdr == "" {
			cl.logEntry(conn.RemoteAddr(), method, target, "refused-missing-host")
			proxy.RstHangup(conn)
			_ = conn.Close()
			return
		}
		// Validate host[:port]
		if _, _, _, err := NormalizeAuthority(hostHdr); err != nil {
			cl.logEntry(conn.RemoteAddr(), method, target, "refused-invalid-host")
			proxy.RstHangup(conn)
			_ = conn.Close()
			return
		}
		reqURL, err := url.ParseRequestURI(target)
		if err != nil {
			cl.logEntry(conn.RemoteAddr(), method, target, "refused-relative")
			proxy.RstHangup(conn)
			_ = conn.Close()
			return
		}
		reqURL.Scheme = "http"
		reqURL.Host = hostHdr
		u = reqURL
	} else {
		// Absolute-form URI: take host from URI
		var uerr error
		u, uerr = url.Parse(target)
		if uerr != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			cl.logEntry(conn.RemoteAddr(), method, target, "refused-relative")
			proxy.RstHangup(conn)
			_ = conn.Close()
			return
		}
	}

	// Look for surrogate token in headers
	surToken, headerName := cl.proxy.extractSurrogate(headers)
	if surToken != "" {
		// Bearer lane surrogate swap pipeline (§5.2)
		cl.handleBearer(ctx, conn, br, method, target, version, u, headers, surToken, headerName)
		return
	}

	// Credential-less egress pipeline (§6.1, §6.3, §7)
	cl.handleCredentialLess(ctx, conn, br, method, target, version, u, headers)
}

// extractSurrogate scans candidate headers for registered surrogate tokens per §5.2.
// Checks Authorization (Bearer sur_... or bare sur_...), X-Api-Key, X-Goog-Api-Key,
// plus configured extra headers from Config.HeaderExtras.
func (p *Proxy) extractSurrogate(headers []string) (token string, headerName string) {
	isCandidate := func(name string) bool {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "x-api-key" || lower == "x-goog-api-key" {
			return true
		}
		for _, extra := range p.extraHeaders {
			if strings.EqualFold(extra, lower) {
				return true
			}
		}
		return false
	}

	for _, h := range headers {
		colon := strings.IndexByte(h, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(h[:colon])
		if !isCandidate(name) {
			continue
		}
		val := strings.TrimSpace(h[colon+1:])
		// Authorization may be "Bearer sur_..." or "sur_..."
		if strings.HasPrefix(strings.ToLower(val), "bearer ") {
			candidate := strings.TrimSpace(val[7:])
			if strings.HasPrefix(candidate, "sur_") {
				return candidate, name
			}
		} else if strings.HasPrefix(val, "sur_") {
			return val, name
		}
	}
	return "", ""
}

// forwardCONNECT handles TLS CONNECT tunneling per CUSTOS-SPEC §5.2, §10 V10.
func (cl *cellListener) forwardCONNECT(ctx context.Context, conn net.Conn, br *bufio.Reader, target string) {
	defer conn.Close()

	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		proxy.RstHangup(conn)
		return
	}

	// Destination floor check per CA-2(ii), CA-2(v)
	ip, err := cl.proxy.resolveFloor(ctx, host)
	if err != nil {
		cl.logEntry(conn.RemoteAddr(), http.MethodConnect, target, "refused-floor")
		cl.drainAndRefuse(conn, br, "policy denied")
		return
	}

	// Forward-time dial: resolve once and dial that IP literal
	dialTarget := net.JoinHostPort(ip, portStr)
	upstream, err := cl.proxy.dialUpstream(ctx, dialTarget)
	if err != nil {
		cl.logEntry(conn.RemoteAddr(), http.MethodConnect, target, "connect-dial-fail")
		proxy.RstHangup(conn)
		return
	}
	defer upstream.Close()

	cl.logEntry(conn.RemoteAddr(), http.MethodConnect, target, "connect")

	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	// Passthrough relay: raw bytes without swap, Authorization rides through untouched per V10
	proxy.Relay(ctx, conn, br, upstream)
}

// handleBearer executes the bearer lane pipeline per CUSTOS-SPEC §5.2, §6.4, §7.
func (cl *cellListener) handleBearer(
	ctx context.Context,
	conn net.Conn,
	br *bufio.Reader,
	method, target, version string,
	u *url.URL,
	headers []string,
	surToken, headerName string,
) {
	// A. Normalize authority and path per CUSTOS-SPEC §5.2, §6.3
	canonHost, canonPort, isIP, err := NormalizeAuthority(u.Host)
	if err != nil {
		cl.drainAndRefuse(conn, br, "surrogate host mismatch")
		cl.auditSwapDenied(surToken, "", u.Host, "invalid_host", "deny")
		return
	}

	cleanedPath := cleanRequestPath(u.EscapedPath())
	if cleanedPath == "" {
		cleanedPath = "/"
	}

	// B. Vault unlock check per CUSTOS-SPEC §C3, §P4
	if cl.proxy.vault == nil || !cl.proxy.vault.IsUnlocked() {
		cl.drainAndRefuse(conn, br, "credential custody locked")
		cl.auditSwapDenied(surToken, "", canonHost, "custos_locked", "deny")
		return
	}

	// C. Surrogate lookup and match per CUSTOS-SPEC §5.2:
	// "must exist, lane == "bearer", host and ports binding must match the forward-time dial target"
	rec, err := cl.proxy.vault.Surrogates().LookupAndMatch(surToken, u.Host, cleanedPath)
	if err != nil {
		body := err.Error()
		reason := reasonFromSurrogateErr(err)
		cl.drainAndRefuse(conn, br, body)
		cl.auditSwapDenied(surToken, "", canonHost, reason, "deny")
		return
	}

	// D. Retrieve credential from vault
	cred, ok := cl.proxy.vault.GetCredential(rec.Credential)
	if !ok {
		cl.drainAndRefuse(conn, br, "surrogate not found")
		cl.auditSwapDenied(surToken, rec.Credential, canonHost, "missing_credential", "deny")
		return
	}

	secret := cred.Secret
	if cred.Kind == "oauth2" && cred.AccessToken != "" {
		secret = cred.AccessToken
	}

	// E. Forward-time DNS resolution and floor check per CA-2(ii), CA-2(v), V16 rebinding doctrine:
	// "resolve the host, floor-check the dialed IP (CA-2 amended), match §6.3 against the actual dial target
	// (V16 rebinding doctrine: DNS answer used for the match is the one dialed — resolve once and dial that IP)."
	ip, err := cl.proxy.resolveFloor(ctx, canonHost)
	if err != nil {
		cl.logEntry(conn.RemoteAddr(), method, u.Host, "refused-floor")
		cl.drainAndRefuse(conn, br, "policy denied")
		cl.auditSwapDenied(surToken, rec.Credential, canonHost, "floor", "deny")
		return
	}

	// For IP-literal targets or host targets, forward-time dial destination
	forwardDialTarget := canonHost
	if isIP {
		forwardDialTarget = ip
	}

	// F. Policy Decide per CUSTOS-SPEC §6.1, §6.2, §6.3:
	// "the destination floor is not a policy: floor runs before any policy lookup"
	verdict, _ := cl.proxy.vault.Policy().Decide(DecideInput{
		Lane:       "bearer",
		Credential: rec.Credential,
		Host:       forwardDialTarget,
		Port:       canonPort,
	})

	switch verdict {
	case VerdictDeny:
		cl.drainAndRefuse(conn, br, "policy denied")
		cl.auditSwapDenied(surToken, rec.Credential, canonHost, "policy", "deny")
		return

	case VerdictAsk:
		// G. Parking Gate per CUSTOS-SPEC §7:
		// "only HTTP/1.1 requests with a valid absolute-form URI (or Host) may park; anything else
		// is closed immediately (a 1.0 client must not hold a 330 s slot)."
		// "Closed immediately" honors the same wire mechanics as refusals:
		// drain first, then close — a bare/RST close would surface ECONNRESET.
		if version != "HTTP/1.1" {
			cl.drainAndClose(conn, br)
			return
		}

		cl.parkAndServeBearer(ctx, conn, br, method, target, u, headers, rec, canonHost, canonPort, surToken, headerName)
		return

	case VerdictAuto:
		// H. Forward immediately
		cl.auditSwapAllowed(surToken, rec.Credential, canonHost, "auto")
		cl.forwardSwapped(ctx, conn, br, method, u, headers, rec, secret, canonHost, canonPort, ip, surToken, headerName)
		return

	default:
		cl.drainAndRefuse(conn, br, "policy denied")
		cl.auditSwapDenied(surToken, rec.Credential, canonHost, "policy", "deny")
		return
	}
}

// registerParkFlow applies the §7 capacity gates (per-cell cap; global
// ceiling shedding the biggest cell first) and registers the §6.4
// approval card, returning the parked flow. On rejection it returns the
// frozen reason and the caller delivers the refusal + audit line.
func (cl *cellListener) registerParkFlow(rec *SurrogateRecord, method, target, canonHost string, canonPort int, surToken string) (*parkedFlow, string) {
	pm := cl.proxy.parkMgr
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if len(pm.byCell[cl.cellID]) >= pm.maxPerCell {
		return nil, "too_many_asks"
	}
	if pm.totalParked >= pm.maxGlobal {
		if shedFlow := pm.findBiggestCellParkLocked(); shedFlow != nil {
			pm.removeParkLocked(shedFlow)
			shedFlow.outcomeCh <- "too_many_asks"
		}
	}

	destSummary := fmt.Sprintf("%s:%d%s", canonHost, canonPort, rec.PathPrefix)
	argDigest := sha256Hex(method + " " + target)
	reviewStr := fmt.Sprintf("sha256:%s", argDigest)
	cardID, decisionCh := cl.proxy.hub.RegisterCustos(
		cl.cellID,
		rec.Credential,
		destSummary,
		reviewStr,
		pm.holdTimeout,
		nil,
	)

	flow := &parkedFlow{
		id:         cardID,
		cellID:     cl.cellID,
		cred:       rec.Credential,
		host:       canonHost,
		port:       canonPort,
		path:       rec.PathPrefix,
		surToken:   surToken,
		review:     reviewStr,
		decisionCh: decisionCh,
		outcomeCh:  make(chan string, 1),
		createdAt:  time.Now(),
	}
	pm.addParkLocked(flow)
	return flow, ""
}

// parkAndServeBearer manages parking, shedding, approval card wait, and re-validation per CUSTOS-SPEC §7.
func (cl *cellListener) parkAndServeBearer(
	ctx context.Context,
	conn net.Conn,
	br *bufio.Reader,
	method, target string,
	u *url.URL,
	headers []string,
	rec *SurrogateRecord,
	canonHost string,
	canonPort int,
	surToken, headerName string,
) {
	pm := cl.proxy.parkMgr

	// 1+2. Capacity gates and approval-card registration per §7/§6.4
	flow, reject := cl.registerParkFlow(rec, method, target, canonHost, canonPort, surToken)
	if reject != "" {
		cl.drainAndRefuse(conn, br, reject)
		cl.auditSwapDenied(surToken, rec.Credential, canonHost, reject, "ask")
		return
	}

	// 3. Client disconnect monitor mid-park per CUSTOS-SPEC §7:
	// "If the cell closes the connection mid-park, the flow is dropped, its slot released,
	// and its card cancelled ... emitted reason is flow_gone, full stop".
	monitorDone := make(chan struct{})
	defer close(monitorDone)

	cl.monitorDisconnect(conn, monitorDone, func() {
		if flow.cancelled.CompareAndSwap(false, true) {
			pm.mu.Lock()
			pm.removeParkLocked(flow)
			pm.mu.Unlock()
			cl.proxy.hub.CancelCard(flow.id, "flow_gone", "custos")
			_ = conn.Close()
		}
	})

	// Parked flow holds socket open without read deadline per §7
	_ = conn.SetReadDeadline(time.Time{})

	// Wait for decision, timeout, shed, or cancel
	timer := time.NewTimer(pm.holdTimeout)
	defer timer.Stop()

	select {
	case outcome := <-flow.outcomeCh:
		// Evicted by global shed
		if flow.cancelled.CompareAndSwap(false, true) {
			cl.proxy.hub.CancelCard(flow.id, "too_many_asks", "custos")
		}
		if outcome == "too_many_asks" {
			cl.drainAndRefuse(conn, br, "too_many_asks")
			cl.auditSwapDenied(surToken, rec.Credential, canonHost, "too_many_asks", "ask")
		} else {
			cl.drainAndRefuse(conn, br, "approval denied by user")
			cl.auditSwapDenied(surToken, rec.Credential, canonHost, "denied", "ask")
		}
		return

	case <-timer.C:
		// Expiry per CUSTOS-SPEC §7:
		// "ask_hold_timeout expiry closes the flow and settles the card with the frozen timeout reason"
		if flow.cancelled.CompareAndSwap(false, true) {
			pm.mu.Lock()
			pm.removeParkLocked(flow)
			pm.mu.Unlock()
			cl.proxy.hub.CancelCard(flow.id, "timeout", "timer")
		}
		cl.drainAndRefuse(conn, br, "approval denied by user")
		cl.auditSwapDenied(surToken, rec.Credential, canonHost, "timeout", "ask")
		return

	case <-cl.done:
		// Listener closed while parked (proxy/daemon shutdown): settle
		// like expiry so Close() never blocks on a full hold window.
		if flow.cancelled.CompareAndSwap(false, true) {
			pm.mu.Lock()
			pm.removeParkLocked(flow)
			pm.mu.Unlock()
			cl.proxy.hub.CancelCard(flow.id, "timeout", "listener_closed")
		}
		cl.drainAndClose(conn, br)
		return

	case approved := <-flow.decisionCh:
		pm.mu.Lock()
		pm.removeParkLocked(flow)
		pm.mu.Unlock()

		if !approved {
			// Card was denied, cancelled, or settled by lock/revoke
			reason := cl.proxy.hub.Reason(cl.cellID, flow.id)
			switch reason {
			case "custos_locked":
				cl.drainAndRefuse(conn, br, "credential custody locked")
				cl.auditSwapDenied(surToken, rec.Credential, canonHost, "custos_locked", "ask")
			case "credential_revoked":
				// Credential revoked while parked: flow closed per §6.4, §7
				cl.auditSwapDenied(surToken, rec.Credential, canonHost, "credential_revoked", "ask")
				proxy.RstHangup(conn)
				_ = conn.Close()
			case "timeout", "timed_out":
				cl.drainAndRefuse(conn, br, "approval denied by user")
				cl.auditSwapDenied(surToken, rec.Credential, canonHost, "timeout", "ask")
			default:
				cl.drainAndRefuse(conn, br, "approval denied by user")
				cl.auditSwapDenied(surToken, rec.Credential, canonHost, "denied", "ask")
			}
			return
		}

		// 4. Decision-point re-validations per CUSTOS-SPEC §7:
		// "Allow at decision time RE-VALIDATES vault unlocked + surrogate present + current policy verdict
		// (policy rm/revoke during park kills flow at dial)"
		if !cl.proxy.vault.IsUnlocked() {
			cl.drainAndRefuse(conn, br, "credential custody locked")
			cl.auditSwapDenied(surToken, rec.Credential, canonHost, "custos_locked", "ask")
			return
		}

		curRec, curOK := cl.proxy.vault.Surrogates().Lookup(surToken)
		if !curOK || curRec.Credential != rec.Credential {
			cl.drainAndRefuse(conn, br, "surrogate not found")
			cl.auditSwapDenied(surToken, rec.Credential, canonHost, "surrogate_revoked", "ask")
			return
		}

		curCred, credOK := cl.proxy.vault.GetCredential(curRec.Credential)
		if !credOK {
			cl.drainAndRefuse(conn, br, "surrogate not found")
			cl.auditSwapDenied(surToken, rec.Credential, canonHost, "missing_credential", "ask")
			return
		}

		curSecret := curCred.Secret
		if curCred.Kind == "oauth2" && curCred.AccessToken != "" {
			curSecret = curCred.AccessToken
		}

		// Re-validate forward-time DNS and floor check per V16 rebinding doctrine
		forwardIP, err := cl.proxy.resolveFloor(ctx, canonHost)
		if err != nil {
			cl.logEntry(conn.RemoteAddr(), method, u.Host, "refused-floor")
			cl.drainAndRefuse(conn, br, "policy denied")
			cl.auditSwapDenied(surToken, curRec.Credential, canonHost, "floor", "ask")
			return
		}

		// Re-validate current policy verdict against the forward-time dial target
		curVerdict, _ := cl.proxy.vault.Policy().Decide(DecideInput{
			Lane:       "bearer",
			Credential: curRec.Credential,
			Host:       canonHost,
			Port:       canonPort,
		})
		if curVerdict == VerdictDeny {
			cl.drainAndRefuse(conn, br, "policy denied")
			cl.auditSwapDenied(surToken, curRec.Credential, canonHost, "policy", "ask")
			return
		}

		// Re-validation passed: commit swap and dial upstream
		cl.auditSwapAllowed(surToken, curRec.Credential, canonHost, "ask")
		cl.forwardSwapped(ctx, conn, br, method, u, headers, curRec, curSecret, canonHost, canonPort, forwardIP, surToken, headerName)
	}
}

func (pm *parkManager) addParkLocked(f *parkedFlow) {
	if pm.byCell[f.cellID] == nil {
		pm.byCell[f.cellID] = make(map[string]*parkedFlow)
	}
	pm.byCell[f.cellID][f.id] = f
	pm.totalParked++
}

func (pm *parkManager) removeParkLocked(f *parkedFlow) {
	if cellMap, exists := pm.byCell[f.cellID]; exists {
		if _, ok := cellMap[f.id]; ok {
			delete(cellMap, f.id)
			pm.totalParked--
			if len(cellMap) == 0 {
				delete(pm.byCell, f.cellID)
			}
		}
	}
}

func (pm *parkManager) findBiggestCellParkLocked() *parkedFlow {
	var biggestCell string
	maxCount := -1
	for cellID, cellMap := range pm.byCell {
		if len(cellMap) > maxCount {
			maxCount = len(cellMap)
			biggestCell = cellID
		}
	}
	if biggestCell == "" || maxCount <= 0 {
		return nil
	}
	// Return the oldest parked flow from the biggest cell
	var oldest *parkedFlow
	for _, f := range pm.byCell[biggestCell] {
		if oldest == nil || f.createdAt.Before(oldest.createdAt) {
			oldest = f
		}
	}
	return oldest
}

// monitorDisconnect checks for client connection termination mid-park without consuming body bytes.
func (cl *cellListener) monitorDisconnect(conn net.Conn, done <-chan struct{}, onDisconnect func()) {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return
	}
	go func() {
		buf := make([]byte, 1)
		for {
			select {
			case <-done:
				return
			default:
			}
			var n int
			var rawErr error
			err := raw.Read(func(fd uintptr) bool {
				n, _, rawErr = syscall.Recvfrom(int(fd), buf, syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
				return true
			})
			// Genuine disconnect only: FIN (n==0), or socket errors.
			// EAGAIN/EWOULDBLOCK (no data pending) and ETIMEDOUT (a
			// read deadline — e.g. the bounded denial-drain window
			// §7) are NOT disconnects; the earlier blanket "any
			// error != EAGAIN" test misfired on the drain deadline
			// and closed the socket before refusal delivery.
			realClose := rawErr != nil && rawErr != syscall.EAGAIN &&
				rawErr != syscall.EWOULDBLOCK && rawErr != syscall.ETIMEDOUT
			if err != nil {
				// RawConn itself broken (conn closed): stop watching.
				return
			}
			if (n == 0 && rawErr == nil) || realClose {
				select {
				case <-done:
					return
				default:
					onDisconnect()
					return
				}
			}
			select {
			case <-done:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
}

// forwardSwapped dials upstream with the swapped credential, strips Accept-Encoding,
// and applies response scrub per CUSTOS-SPEC §5.2.
func (cl *cellListener) forwardSwapped(
	ctx context.Context,
	conn net.Conn,
	br *bufio.Reader,
	method string,
	u *url.URL,
	headers []string,
	rec *SurrogateRecord,
	secret string,
	canonHost string,
	canonPort int,
	dialIP string,
	surToken, headerName string,
) {
	defer conn.Close()

	// Dial the forward-time dial target IP literal
	dialTarget := net.JoinHostPort(dialIP, strconv.Itoa(canonPort))
	upstream, err := cl.proxy.dialUpstream(ctx, dialTarget)
	if err != nil {
		cl.logEntry(conn.RemoteAddr(), method, u.Host, "proxied-dial-fail")
		proxy.RstHangup(conn)
		return
	}
	defer upstream.Close()

	// Write request line
	reqPath := u.RequestURI()
	if reqPath == "" {
		reqPath = "/"
	}
	if _, err := fmt.Fprintf(upstream, "%s %s HTTP/1.1\r\n", method, reqPath); err != nil {
		return
	}

	// Forward headers: swap surrogate token with real secret, strip Accept-Encoding per §5.2
	hasHost := false
	for _, h := range headers {
		colon := strings.IndexByte(h, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(h[:colon])
		lowerName := strings.ToLower(name)

		if proxy.SkipHopByHop(lowerName) {
			continue
		}
		if lowerName == "host" {
			hasHost = true
		}
		// Strip Accept-Encoding per CUSTOS-SPEC §5.2:
		// "strip Accept-Encoding (upstream is asked for identity encoding; compression would put secret echoes beyond the scan)"
		if lowerName == "accept-encoding" {
			continue
		}

		if strings.EqualFold(name, headerName) {
			val := strings.TrimSpace(h[colon+1:])
			newVal := strings.Replace(val, surToken, secret, 1)
			if _, err := fmt.Fprintf(upstream, "%s: %s\r\n", name, newVal); err != nil {
				return
			}
		} else {
			if _, err := fmt.Fprintf(upstream, "%s\r\n", h); err != nil {
				return
			}
		}
	}

	if !hasHost && u.Host != "" {
		if _, err := fmt.Fprintf(upstream, "Host: %s\r\n", u.Host); err != nil {
			return
		}
	}

	if _, err := upstream.Write([]byte("Connection: close\r\n\r\n")); err != nil {
		return
	}

	cl.logEntry(conn.RemoteAddr(), method, u.Host, "proxied")

	// Stream request body from client to upstream
	if err := proxy.CopyRequestBody(conn, br, upstream, headers); err != nil {
		earlyCtx, cancel := context.WithTimeout(ctx, RequestPhaseTimeout)
		proxy.RelayOneWay(earlyCtx, conn, upstream)
		cancel()
		return
	}

	// Upstream -> client response processing with Response Scrub per §5.2
	cl.scrubAndRelayResponse(conn, upstream, rec, secret, canonHost)
}

// scrubAndRelayResponse implements response scrub per CUSTOS-SPEC §5.2:
// - Content-Encoding => refused binary-typed (audit swap_denied reason response_type).
// - text bodies <=1 MiB scanned, swapped secret replaced with [redacted] (keys-spec 8-char floor).
// - >1 MiB or binary type => connection closed, audit swap_denied reason response_size / response_type.
// - allow_binary bypasses scrub.
func (cl *cellListener) scrubAndRelayResponse(conn net.Conn, upstream net.Conn, rec *SurrogateRecord, secret string, host string) {
	upReader := bufio.NewReader(upstream)
	statusLine, err := proxy.ReadLineLimited(upReader)
	if err != nil {
		proxy.RstHangup(conn)
		return
	}

	respHeaders, err := proxy.ReadHeaders(upReader)
	if err != nil {
		proxy.RstHangup(conn)
		return
	}

	hasContentEncoding := false
	contentType := ""
	contentLength := int64(-1)
	isChunked := false

	for _, h := range respHeaders {
		colon := strings.IndexByte(h, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(h[:colon]))
		val := strings.TrimSpace(h[colon+1:])
		switch name {
		case "content-encoding":
			if val != "" && val != "identity" {
				hasContentEncoding = true
			}
		case "content-type":
			contentType = strings.ToLower(val)
		case "content-length":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				contentLength = n
			}
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(val), "chunked") {
				isChunked = true
			}
		}
	}

	// 1. Content-Encoding check per CUSTOS-SPEC §5.2:
	// "responses carrying any Content-Encoding are refused as binary-typed ... unless the binding sets allow_binary: true"
	if hasContentEncoding && !rec.AllowBinary {
		cl.auditSwapDenied(rec.Token, rec.Credential, host, "response_type", "auto")
		cl.drainAndRefuse(conn, nil, "refused binary-typed response")
		return
	}

	// 2. Binary content-type check per CUSTOS-SPEC §5.2
	isBinary := isBinaryType(contentType)
	if isBinary && !rec.AllowBinary {
		cl.auditSwapDenied(rec.Token, rec.Credential, host, "response_type", "auto")
		proxy.RstHangup(conn)
		return
	}

	// 3. allow_binary bypasses scrub per §5.2
	if rec.AllowBinary && (isBinary || hasContentEncoding) {
		statusLine = strings.TrimRight(statusLine, "\r\n")
		_, _ = fmt.Fprintf(conn, "%s\r\n", statusLine)
		for _, h := range respHeaders {
			colon := strings.IndexByte(h, ':')
			if colon >= 0 && proxy.SkipHopByHop(strings.ToLower(strings.TrimSpace(h[:colon]))) {
				continue
			}
			_, _ = fmt.Fprintf(conn, "%s\r\n", h)
		}
		_, _ = conn.Write([]byte("Connection: close\r\n\r\n"))
		_, _ = io.Copy(conn, upReader)
		return
	}

	// 4. Response size check (>1 MiB refused with connection closed, no partial delivery)
	if contentLength > maxScannedBody {
		cl.auditSwapDenied(rec.Token, rec.Credential, host, "response_size", "auto")
		proxy.RstHangup(conn)
		return
	}

	var bodyReader io.Reader
	switch {
	case isChunked:
		bodyReader = httputil.NewChunkedReader(upReader)
	case contentLength >= 0:
		bodyReader = io.LimitReader(upReader, contentLength)
	default:
		bodyReader = upReader
	}

	// Read body into memory up to maxScannedBody + 1 byte
	limitReader := io.LimitReader(bodyReader, maxScannedBody+1)
	bodyBytes, err := io.ReadAll(limitReader)
	if err != nil {
		proxy.RstHangup(conn)
		return
	}
	if int64(len(bodyBytes)) > maxScannedBody {
		cl.auditSwapDenied(rec.Token, rec.Credential, host, "response_size", "auto")
		proxy.RstHangup(conn)
		return
	}

	// 5. Secret redaction per §5.2, §5.1a (8-char scrub floor):
	// "every occurrence of the swapped secret replaced with [redacted] before delivery to the cell ...
	// Substrings shorter than 8 characters are never replaced in bodies"
	outBytes := bodyBytes
	if len(secret) >= 8 {
		outBytes = bytes.ReplaceAll(bodyBytes, []byte(secret), []byte("[redacted]"))
	}

	// Write response headers with updated Content-Length and Connection: close
	statusLine = strings.TrimRight(statusLine, "\r\n")
	_, _ = fmt.Fprintf(conn, "%s\r\n", statusLine)
	hasCL := false
	for _, h := range respHeaders {
		colon := strings.IndexByte(h, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(h[:colon]))
		if proxy.SkipHopByHop(name) {
			continue
		}
		switch name {
		case "content-length":
			_, _ = fmt.Fprintf(conn, "Content-Length: %d\r\n", len(outBytes))
			hasCL = true
		case "transfer-encoding":
			// Chunk framing removed by buffered scan
		default:
			_, _ = fmt.Fprintf(conn, "%s\r\n", h)
		}
	}
	if !hasCL {
		_, _ = fmt.Fprintf(conn, "Content-Length: %d\r\n", len(outBytes))
	}
	_, _ = conn.Write([]byte("Connection: close\r\n\r\n"))

	// Deliver redacted body
	_, _ = conn.Write(outBytes)
}

// isBinaryType reports whether contentType represents non-scannable binary content.
func isBinaryType(ct string) bool {
	if ct == "" {
		return false
	}
	ct = strings.ToLower(ct)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if strings.HasPrefix(ct, "text/") {
		return false
	}
	if strings.Contains(ct, "json") || strings.Contains(ct, "xml") ||
		strings.Contains(ct, "javascript") || strings.Contains(ct, "html") ||
		strings.Contains(ct, "form-urlencoded") {
		return false
	}
	return true
}

// handleCredentialLess handles plain egress requests without surrogate tokens per CUSTOS-SPEC §6.1, §6.3, §7.
func (cl *cellListener) handleCredentialLess(
	ctx context.Context,
	conn net.Conn,
	br *bufio.Reader,
	method, target, version string,
	u *url.URL,
	headers []string,
) {
	defer conn.Close()

	canonHost, canonPort, isIP, err := NormalizeAuthority(u.Host)
	if err != nil {
		cl.logEntry(conn.RemoteAddr(), method, u.Host, "refused-relative")
		proxy.RstHangup(conn)
		return
	}

	// 1. Destination floor check per CA-2(ii), CA-2(v)
	ip, err := cl.proxy.resolveFloor(ctx, canonHost)
	if err != nil {
		cl.logEntry(conn.RemoteAddr(), method, u.Host, "refused-floor")
		cl.auditEgressDenied(canonHost, "floor")
		cl.drainAndRefuse(conn, br, "policy denied")
		return
	}

	forwardDialTarget := canonHost
	if isIP {
		forwardDialTarget = ip
	}

	// 2. Policy Decide per CUSTOS-SPEC §6.1, §6.3:
	// Default is auto, unless egress strict mode is enabled (defaults to ask)
	verdict, _ := cl.proxy.vault.Policy().Decide(DecideInput{
		Host: forwardDialTarget,
		Port: canonPort,
	})

	switch verdict {
	case VerdictDeny:
		cl.logEntry(conn.RemoteAddr(), method, u.Host, "refused-policy")
		cl.auditEgressDenied(canonHost, "policy")
		cl.drainAndRefuse(conn, br, "policy denied")
		return

	case VerdictAsk:
		if version != "HTTP/1.1" {
			cl.drainAndClose(conn, br)
			return
		}
		// Credential-less ask flow uses same parking mechanics
		cl.parkAndServeCredentialLess(ctx, conn, br, method, target, u, headers, canonHost, canonPort)
		return

	case VerdictAuto:
		cl.auditEgressAllowed(canonHost)
		cl.forwardDirect(ctx, conn, br, method, u, headers, canonPort, ip)
		return

	default:
		cl.auditEgressDenied(canonHost, "policy")
		cl.drainAndRefuse(conn, br, "policy denied")
		return
	}
}

func (cl *cellListener) parkAndServeCredentialLess(
	ctx context.Context,
	conn net.Conn,
	br *bufio.Reader,
	method, target string,
	u *url.URL,
	headers []string,
	canonHost string,
	canonPort int,
) {
	pm := cl.proxy.parkMgr

	pm.mu.Lock()
	if len(pm.byCell[cl.cellID]) >= pm.maxPerCell {
		pm.mu.Unlock()
		cl.drainAndRefuse(conn, br, "too_many_asks")
		cl.auditEgressDenied(canonHost, "too_many_asks")
		return
	}

	if pm.totalParked >= pm.maxGlobal {
		shedFlow := pm.findBiggestCellParkLocked()
		if shedFlow != nil {
			pm.removeParkLocked(shedFlow)
			shedFlow.outcomeCh <- "too_many_asks"
		}
	}

	destSummary := fmt.Sprintf("%s:%d", canonHost, canonPort)
	argDigest := sha256Hex(method + " " + target)
	reviewStr := fmt.Sprintf("sha256:%s", argDigest)

	cardID, decisionCh := cl.proxy.hub.RegisterCustos(
		cl.cellID,
		"",
		destSummary,
		reviewStr,
		pm.holdTimeout,
		nil,
	)

	flow := &parkedFlow{
		id:         cardID,
		cellID:     cl.cellID,
		host:       canonHost,
		port:       canonPort,
		review:     reviewStr,
		decisionCh: decisionCh,
		outcomeCh:  make(chan string, 1),
		createdAt:  time.Now(),
	}

	pm.addParkLocked(flow)
	pm.mu.Unlock()

	monitorDone := make(chan struct{})
	defer close(monitorDone)

	cl.monitorDisconnect(conn, monitorDone, func() {
		if flow.cancelled.CompareAndSwap(false, true) {
			pm.mu.Lock()
			pm.removeParkLocked(flow)
			pm.mu.Unlock()
			cl.proxy.hub.CancelCard(flow.id, "flow_gone", "custos")
			_ = conn.Close()
		}
	})

	_ = conn.SetReadDeadline(time.Time{})
	timer := time.NewTimer(pm.holdTimeout)
	defer timer.Stop()

	select {
	case outcome := <-flow.outcomeCh:
		if flow.cancelled.CompareAndSwap(false, true) {
			cl.proxy.hub.CancelCard(flow.id, "too_many_asks", "custos")
		}
		if outcome == "too_many_asks" {
			cl.drainAndRefuse(conn, br, "too_many_asks")
			cl.auditEgressDenied(canonHost, "too_many_asks")
		} else {
			cl.drainAndRefuse(conn, br, "approval denied by user")
			cl.auditEgressDenied(canonHost, "denied")
		}
		return

	case <-timer.C:
		if flow.cancelled.CompareAndSwap(false, true) {
			pm.mu.Lock()
			pm.removeParkLocked(flow)
			pm.mu.Unlock()
			cl.proxy.hub.CancelCard(flow.id, "timeout", "timer")
		}
		cl.drainAndRefuse(conn, br, "approval denied by user")
		cl.auditEgressDenied(canonHost, "timeout")
		return

	case <-cl.done:
		// Listener closed while parked: settle like expiry so Close()
		// never blocks on a full hold window.
		if flow.cancelled.CompareAndSwap(false, true) {
			pm.mu.Lock()
			pm.removeParkLocked(flow)
			pm.mu.Unlock()
			cl.proxy.hub.CancelCard(flow.id, "timeout", "listener_closed")
		}
		cl.drainAndClose(conn, br)
		return

	case approved := <-flow.decisionCh:
		pm.mu.Lock()
		pm.removeParkLocked(flow)
		pm.mu.Unlock()

		if !approved {
			reason := cl.proxy.hub.Reason(cl.cellID, flow.id)
			switch reason {
			case "timeout", "timed_out":
				cl.drainAndRefuse(conn, br, "approval denied by user")
				cl.auditEgressDenied(canonHost, "timeout")
			default:
				cl.drainAndRefuse(conn, br, "approval denied by user")
				cl.auditEgressDenied(canonHost, "denied")
			}
			return
		}

		forwardIP, err := cl.proxy.resolveFloor(ctx, canonHost)
		if err != nil {
			cl.logEntry(conn.RemoteAddr(), method, u.Host, "refused-floor")
			cl.drainAndRefuse(conn, br, "policy denied")
			cl.auditEgressDenied(canonHost, "floor")
			return
		}

		curVerdict, _ := cl.proxy.vault.Policy().Decide(DecideInput{
			Host: canonHost,
			Port: canonPort,
		})
		if curVerdict == VerdictDeny {
			cl.drainAndRefuse(conn, br, "policy denied")
			cl.auditEgressDenied(canonHost, "policy")
			return
		}

		cl.auditEgressAllowed(canonHost)
		cl.forwardDirect(ctx, conn, br, method, u, headers, canonPort, forwardIP)
	}
}

// forwardDirect dials upstream and forwards request without token replacement.
func (cl *cellListener) forwardDirect(
	ctx context.Context,
	conn net.Conn,
	br *bufio.Reader,
	method string,
	u *url.URL,
	headers []string,
	canonPort int,
	dialIP string,
) {
	dialTarget := net.JoinHostPort(dialIP, strconv.Itoa(canonPort))
	upstream, err := cl.proxy.dialUpstream(ctx, dialTarget)
	if err != nil {
		cl.logEntry(conn.RemoteAddr(), method, u.Host, "proxied-dial-fail")
		proxy.RstHangup(conn)
		return
	}
	defer upstream.Close()

	reqPath := u.RequestURI()
	if reqPath == "" {
		reqPath = "/"
	}
	if _, err := fmt.Fprintf(upstream, "%s %s HTTP/1.1\r\n", method, reqPath); err != nil {
		return
	}

	hasHost := false
	for _, h := range headers {
		colon := strings.IndexByte(h, ':')
		if colon >= 0 {
			name := strings.ToLower(strings.TrimSpace(h[:colon]))
			if name == "host" {
				hasHost = true
			}
			if proxy.SkipHopByHop(name) {
				continue
			}
			if _, err := fmt.Fprintf(upstream, "%s\r\n", h); err != nil {
				return
			}
		}
	}
	if !hasHost && u.Host != "" {
		if _, err := fmt.Fprintf(upstream, "Host: %s\r\n", u.Host); err != nil {
			return
		}
	}
	if _, err := upstream.Write([]byte("Connection: close\r\n\r\n")); err != nil {
		return
	}

	cl.logEntry(conn.RemoteAddr(), method, u.Host, "proxied")

	if err := proxy.CopyRequestBody(conn, br, upstream, headers); err != nil {
		earlyCtx, cancel := context.WithTimeout(ctx, RequestPhaseTimeout)
		proxy.RelayOneWay(earlyCtx, conn, upstream)
		cancel()
		return
	}

	proxy.RelayOneWay(ctx, conn, upstream)
}

// drainAndRefuse implements the denial delivery rule per CUSTOS-SPEC §7, §10 V26:
// drain unread receive buffer (EOF or 64 KiB, 250 ms cap, best-effort) BEFORE writing refusal,
// then write + close; no pipelined continuation after parked-flow responses.
func (cl *cellListener) drainAndRefuse(conn net.Conn, br *bufio.Reader, body string) {
	statusLine := "403 surrogate"
	cl.drainSocket(conn, br)

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	resp := fmt.Sprintf("HTTP/1.1 %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		statusLine, len(body), body)
	_, _ = conn.Write([]byte(resp))
	_ = conn.Close()
}

// drainSocket performs the §7 bounded drain (to EOF or 64 KiB, 250 ms
// cap, best-effort) shared by refusal delivery and the parking-gate
// close: closing with unread body bytes makes the kernel emit TCP RST
// (frozen wire mechanics, §7).
func (cl *cellListener) drainSocket(conn net.Conn, br *bufio.Reader) {
	_ = conn.SetReadDeadline(time.Now().Add(drainWindowTimeout))
	var drained int64
	buf := make([]byte, 4096)

	var r io.Reader = conn
	if br != nil {
		r = br
	}
	for drained < drainWindowBytes {
		toRead := len(buf)
		if int64(toRead) > (drainWindowBytes - drained) {
			toRead = int(drainWindowBytes - drained)
		}
		n, err := r.Read(buf[:toRead])
		drained += int64(n)
		if err != nil {
			break
		}
	}
}

// drainAndClose implements the parking-gate close per §7: drain first
// (same bounded window as refusal delivery — a bare close with unread
// bytes would surface as ECONNRESET), then close cleanly with no
// response (the gate closes the connection; it does not answer).
func (cl *cellListener) drainAndClose(conn net.Conn, br *bufio.Reader) {
	cl.drainSocket(conn, br)
	_ = conn.Close()
}

func (cl *cellListener) auditSwapDenied(surToken, cred, host, reason, verdict string) {
	if cl.proxy.vault == nil || cl.proxy.vault.Audit() == nil {
		return
	}
	surFP := ""
	if surToken != "" {
		surFP = SHA256Hex8(surToken)
	}
	_ = cl.proxy.vault.Audit().Append(AuditRecord{
		Kind:    AuditKindSwapDenied,
		Cred:    cred,
		Sur:     surFP,
		Host:    host,
		Verdict: verdict,
		Reason:  reason,
		Actor:   cl.cellID,
	})
}

func (cl *cellListener) auditSwapAllowed(surToken, cred, host, verdict string) {
	if cl.proxy.vault == nil || cl.proxy.vault.Audit() == nil {
		return
	}
	surFP := ""
	if surToken != "" {
		surFP = SHA256Hex8(surToken)
	}
	_ = cl.proxy.vault.Audit().Append(AuditRecord{
		Kind:    AuditKindSwapAllowed,
		Cred:    cred,
		Sur:     surFP,
		Host:    host,
		Verdict: verdict,
		Actor:   cl.cellID,
	})
}

func (cl *cellListener) auditEgressDenied(host, reason string) {
	if cl.proxy.vault == nil || cl.proxy.vault.Audit() == nil {
		return
	}
	_ = cl.proxy.vault.Audit().Append(AuditRecord{
		Kind:    AuditKindEgressDenied,
		Host:    host,
		Verdict: "deny",
		Reason:  reason,
		Actor:   cl.cellID,
	})
}

func (cl *cellListener) auditEgressAllowed(host string) {
	if cl.proxy.vault == nil || cl.proxy.vault.Audit() == nil {
		return
	}
	_ = cl.proxy.vault.Audit().Append(AuditRecord{
		Kind:    AuditKindEgressAllowed,
		Host:    host,
		Verdict: "auto",
		Actor:   cl.cellID,
	})
}

// resolveFloor resolves host and validates against the floor per CUSTOS CA-2(ii), CA-2(iii), CA-2(v).
func (p *Proxy) resolveFloor(ctx context.Context, host string) (string, error) {
	localAddrs, err := proxy.HostInterfaceAddrs()
	if err != nil {
		return "", fmt.Errorf("cannot enumerate host addresses: %w", err)
	}
	isLocal := func(ip net.IP) bool {
		for _, l := range localAddrs {
			if l.Equal(ip) {
				return true
			}
		}
		return false
	}
	reject := func(ip net.IP) bool {
		return IsFloorDestination(ip) || isLocal(ip)
	}

	if ip := net.ParseIP(host); ip != nil {
		if reject(ip) {
			return "", fmt.Errorf("%w: destination %s", proxy.ErrFloor, host)
		}
		return host, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	ips, err := p.resolveHostIPs(ctx, host)
	if err != nil {
		return "", err
	}
	for _, a := range ips {
		if reject(a) {
			return "", fmt.Errorf("%w: destination %s", proxy.ErrFloor, host)
		}
	}
	if len(ips) > 0 {
		return ips[0].String(), nil
	}
	return "", fmt.Errorf("no addresses for %s", host)
}

func (p *Proxy) resolveHostIPs(ctx context.Context, host string) ([]net.IP, error) {
	p.mu.RLock()
	fn := p.resolveFn
	p.mu.RUnlock()
	if fn != nil {
		return fn(ctx, host)
	}

	r := net.DefaultResolver
	addrs, err := r.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, len(addrs))
	for i, a := range addrs {
		out[i] = a.IP
	}
	return out, nil
}

func (p *Proxy) dialUpstream(ctx context.Context, hostPort string) (net.Conn, error) {
	atomic.AddInt64(&p.dialCounter, 1)

	p.mu.RLock()
	fn := p.dialFn
	p.mu.RUnlock()
	if fn != nil {
		return fn(ctx, "tcp", hostPort)
	}

	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return nil, err
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(60 * time.Second)
	}
	return conn, nil
}

// cleanRequestPath decodes percent-encoded dot slices and cleans the path per CUSTOS-SPEC §5.2.
func cleanRequestPath(reqPath string) string {
	decoded := reqPath
	for {
		lower := strings.ToLower(decoded)
		if !strings.Contains(lower, "%2e") {
			break
		}
		decoded = strings.ReplaceAll(decoded, "%2e", ".")
		decoded = strings.ReplaceAll(decoded, "%2E", ".")
	}
	cleaned := path.Clean(decoded)
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	return cleaned
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func reasonFromSurrogateErr(err error) string {
	switch {
	case errors.Is(err, ErrSurrogateNotFound):
		return "not_found"
	case errors.Is(err, ErrSurrogateHostMismatch):
		return "host_mismatch"
	case errors.Is(err, ErrSurrogatePortMismatch):
		return "port_mismatch"
	case errors.Is(err, ErrSurrogatePathMismatch):
		return "path_mismatch"
	case errors.Is(err, ErrSurrogateLaneMismatch):
		return "lane_mismatch"
	default:
		return "invalid_host"
	}
}
