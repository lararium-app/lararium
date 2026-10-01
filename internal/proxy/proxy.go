// Package proxy implements the Lararium Phase-3 dumb forward proxy
// (CELL-SPEC §4, frozen contract): absolute-form GET/POST + CONNECT,
// one listener per cell bound to that cell's gateway address, append-only
// access log. It is deliberately minimal: Phase 3 proves the CAGE, not
// policy — L7 rules and CONNECT filtering are custos (Phase 4).
//
// The one policy the proxy MUST enforce itself is the destination floor
// (erratum E3): the proxy runs in the host namespace, so relaying to
// host loopback, link-local/metadata, or the cell mesh (10.91.0.0/16)
// would hand cells a route the cage promises they do not have (live
// breach 2026-10-01: CONNECT 127.0.0.1:22 returned the host sshd
// banner). The floor rejects before any packet leaves the process.
package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Limits (spec §4 / brief rev2): bounded connections per cell and
// bounded read phases so a wedged or hostile guest cannot pile up
// proxy state or host memory.
const (
	// MaxConnsPerCell caps simultaneous proxied connections.
	MaxConnsPerCell = 256
	// IdleTimeout bounds one direction's silence during relay.
	IdleTimeout = 5 * time.Minute
	// RequestPhaseTimeout bounds the whole request-line+headers
	// read: a guest that opens the door and then sends TLS
	// records or garbage must be refused in seconds, not held
	// for the idle window (agy F11).
	RequestPhaseTimeout = 10 * time.Second

	// maxRequestLine bounds one request/header line.
	maxRequestLine = 8 * 1024
	// maxTotalHeaders bounds aggregate header bytes (memory cap:
	// the proxy runs OUTSIDE the cell's cgroup).
	maxTotalHeaders = 64 * 1024
	// copyBufSize per spec: 32 KiB relay buffers.
	copyBufSize = 32 * 1024
	// maxRelayBody bounds a single request body's stream (agy r2
	// F7's chunked-path cap).
	maxRelayBody = 16 << 20
)

// cellMesh covers every allocated cell subnet (10.91.<n>.0/28 for all
// n): the proxy must never relay into it (cell-to-cell and
// proxy-to-other-cell gateway both bypass the filter's fib scope).
var cellMesh = &net.IPNet{IP: net.IPv4(10, 91, 0, 0), Mask: net.CIDRMask(16, 32)}

// ErrFloor marks a destination the proxy floor refused (as opposed to
// a plain network failure). Callers log it as refused-floor evidence
// (C7: the audit trail must distinguish "we said no" from "origin
// down").
var ErrFloor = errors.New("refused by proxy floor")

// forbiddenDest reports destination IPs the floor rejects: loopback,
// unspecified, link-local (incl. cloud metadata 169.254.169.254),
// multicast, and the cell mesh. Private internet ranges are policy
// (custos, Phase 4), not the floor.
func forbiddenDest(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || cellMesh.Contains(ip)
}

// hostInterfaceAddrs returns every address currently assigned to a
// host interface (agy r2 F2: the floor must cover the host's OWN
// non-loopback addresses — dialing the box's LAN IP, docker0 bridge,
// or tailscale address from a cell reaches the same daemons loopback
// was blocked for; bench-verified escape class). Enumerated per
// request, not cached: interfaces churn (tunnels up/down) and a stale
// allow-set is exactly the bypass. Failure is fatal for the request —
// if we cannot enumerate, we cannot certify the destination.
func hostInterfaceAddrs() ([]net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP != nil {
			ips = append(ips, ipn.IP)
		}
	}
	return ips, nil
}

// Server is one cell's dumb proxy listener.
type Server struct {
	// Listen is the gateway address:port (10.91.<n>.1:<port>).
	Listen string
	// Peer is the cell's own address (10.91.<n>.2): every accepted
	// connection must come from it. Defense in depth behind the
	// fib-scoped filter rule — the weak host model means the filter
	// alone could be bypassed if the rule set ever changes.
	Peer string
	// LogPath is the append-only access log (C7 evidence).
	LogPath string

	logMu sync.Mutex
	conns int
	mu    sync.Mutex
}

// ErrFull is returned (after RST) when the per-cell connection cap is
// reached.
var ErrFull = errors.New("proxy: connection cap reached")

// Serve binds and serves until ctx is cancelled. Binding failure is
// immediate (fail loud — the unit should exit non-zero).
func (s *Server) Serve(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.Listen)
	if err != nil {
		return fmt.Errorf("proxy bind %s: %w", s.Listen, err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		ln.Close() // unblocks Accept
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown
			}
			return err
		}
		if !s.admit(conn) {
			continue
		}
		go s.handle(ctx, conn)
	}
}

// admit validates the peer and the connection cap, RST-ing otherwise.
func (s *Server) admit(conn net.Conn) bool {
	peerIP := ipOnly(conn.RemoteAddr().String())
	if peerIP != s.Peer {
		// A neighbor cell borrowed our listener through a weak-host
		// route: kill it silently (never service a foreign cell).
		rstHangup(conn)
		conn.Close()
		return false
	}
	s.mu.Lock()
	full := s.conns >= MaxConnsPerCell
	if !full {
		s.conns++
	}
	s.mu.Unlock()
	if full {
		rstHangup(conn)
		conn.Close()
		return false
	}
	return true
}

func (s *Server) release() {
	s.mu.Lock()
	s.conns--
	s.mu.Unlock()
}

// handle reads one request (line + headers, bounded) and dispatches.
// Any deviation from the proxy protocol (relative-form request = the
// DNAT no-bypass path, a non-ASCII first byte = e.g. TLS ClientHello
// shoved at the DNAT'd port, garbage) gets a TCP RST with ZERO bytes
// written: an HTTP 400 would be exit-0 for curl and flip C1 to
// PASS-the-wrong-way (spec §4 mandates a connection failure).
func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer s.release()
	defer conn.Close()

	br := bufio.NewReaderSize(conn, maxRequestLine)
	_ = conn.SetReadDeadline(time.Now().Add(RequestPhaseTimeout))
	line, err := readLineLimited(br, maxRequestLine)
	if err != nil {
		// EOF, oversized line, or timeout before a request line:
		// RST is rude but correct per spec for the no-bypass path.
		rstHangup(conn)
		return
	}

	method, target, _, ok := parseRequestLine(line)
	if !ok {
		// Not a request line (TLS record, HTTP/0.9, garbage).
		rstHangup(conn)
		return
	}

	// Drain headers for EVERY request shape (GET/POST and CONNECT):
	// the client always follows the request line with headers and a
	// blank line, and those bytes must be consumed from br BEFORE
	// any tunneling/relay starts, or they leak into the relayed
	// stream (TLS records corrupted by ASCII) while br's buffered
	// tail gets orphaned (agy F1/F2, live-breach class).
	headers, err := readHeaders(br)
	if err != nil {
		rstHangup(conn)
		return
	}

	// Request parsed: relax to the relay idle window.
	_ = conn.SetReadDeadline(time.Now().Add(IdleTimeout))

	switch method {
	case http.MethodGet, http.MethodPost:
		u, uerr := url.Parse(target)
		if uerr != nil || !u.IsAbs() || u.Host == "" ||
			(u.Scheme != "http" && u.Scheme != "https") {
			// Relative-form (or foreign scheme): the DNAT
			// no-bypass path. Refuse with RST, log it.
			s.logEntry(conn.RemoteAddr(), method, target, "refused-relative")
			rstHangup(conn)
			return
		}
		s.forwardHTTP(ctx, conn, br, u, method, headers)
	case http.MethodConnect:
		if !validHostPort(target) {
			rstHangup(conn)
			return
		}
		s.forwardCONNECT(ctx, conn, br, target)
	default:
		// Not a proxy verb: same no-bypass refusal.
		s.logEntry(conn.RemoteAddr(), method, target, "refused-relative")
		rstHangup(conn)
	}
}

// parseRequestLine splits "METHOD SP TARGET SP HTTP/x.y" and enforces
// the printable-ASCII floor: the first byte of anything the proxy
// speaks is an HTTP method letter (agy F11: a TLS ClientHello starts
// 0x16 and must be refused instantly, not hang on a newline).
func parseRequestLine(line string) (method, target, version string, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" || line[0] < 'A' || line[0] > 'Z' {
		return "", "", "", false
	}
	parts := strings.Split(line, " ")
	if len(parts) != 3 {
		return "", "", "", false
	}
	if !strings.HasPrefix(parts[2], "HTTP/") {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// readLineLimited reads one \n-terminated line, failing past max bytes
// BEFORE the bytes hit memory (agy r2 F3: ReadString('\n') returns only
// at the newline, so an endless body without one accumulated
// unbounded — the size check after return was too late). ReadSlice
// parks at the buffer boundary; we accumulate in explicit chunks and
// refuse the moment the budget trips.
func readLineLimited(br *bufio.Reader, maxLen int) (string, error) {
	var b strings.Builder
	for {
		chunk, err := br.ReadSlice('\n')
		b.Write(chunk)
		if errors.Is(err, bufio.ErrBufferFull) {
			if b.Len() > maxLen {
				return "", fmt.Errorf("line exceeds %d bytes", maxLen)
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if b.Len() > maxLen {
			return "", fmt.Errorf("line exceeds %d bytes", maxLen)
		}
		return b.String(), nil
	}
}

// readHeaders consumes lines until the terminating blank line, capping
// each line and the aggregate (memory floor for a process outside the
// cell cgroup). Returns the original lines, blank line excluded.
func readHeaders(br *bufio.Reader) ([]string, error) {
	var headers []string
	total := 0
	for {
		l, err := readLineLimited(br, maxRequestLine)
		if err != nil {
			return nil, err
		}
		total += len(l)
		if total > maxTotalHeaders {
			return nil, fmt.Errorf("headers exceed %d bytes", maxTotalHeaders)
		}
		if l == "\r\n" || l == "\n" {
			return headers, nil
		}
		headers = append(headers, strings.TrimRight(l, "\r\n"))
	}
}

// validHostPort checks the CONNECT authority form host:port.
func validHostPort(target string) bool {
	host, port, err := net.SplitHostPort(target)
	if err != nil || host == "" {
		return false
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(port) >= 1 && len(port) <= 5
}

// forwardHTTP dials the upstream, replays the request headers verbatim
// (minus hop-by-hop noise), and streams the response back byte-for-byte.
// The client->upstream relay MUST start from io.MultiReader(br, conn):
// br holds the body bytes the client pipelined behind the headers
// (agy F2 — reading raw conn orphans them and the upstream hangs on
// Content-Length forever).
func (s *Server) forwardHTTP(ctx context.Context, conn net.Conn, br *bufio.Reader, u *url.URL, method string, headers []string) {
	upstream, err := dialUpstream(ctx, withDefaultPort(u))
	if err != nil {
		if errors.Is(err, ErrFloor) {
			s.logEntry(conn.RemoteAddr(), method, u.Host, "refused-floor")
		} else {
			s.logEntry(conn.RemoteAddr(), method, u.Host, "proxied-dial-fail")
		}
		// Upstream unreachable (or refused by the destination
		// floor): close without a synthesized response (no
		// 400/502 — the cage refuses, it does not role-play a
		// server).
		rstHangup(conn)
		return
	}
	defer upstream.Close()

	// Absolute-form request line to the upstream.
	path := u.RequestURI()
	if _, err := fmt.Fprintf(upstream, "%s %s HTTP/1.1\r\n", method, path); err != nil {
		return
	}
	for _, h := range headers {
		name := h
		if i := strings.IndexByte(h, ':'); i >= 0 {
			name = strings.ToLower(strings.TrimSpace(h[:i]))
		}
		if skipHopByHop(name) {
			continue
		}
		if _, err := fmt.Fprintf(upstream, "%s\r\n", h); err != nil {
			return
		}
	}
	if _, err := upstream.Write([]byte("\r\n")); err != nil {
		return
	}

	// Client -> upstream: exactly this request's body bytes,
	// br's buffered tail FIRST (agy F2), then only up to
	// Content-Length of what remains on the wire. NEVER a blind
	// io.Copy: that forwards pipelined second requests straight to
	// THIS upstream, bypassing floor + log per request
	// (agy r2 F7, smuggling class).
	s.logEntry(conn.RemoteAddr(), method, u.Host, "proxied")
	if err := copyRequestBody(conn, br, upstream, headers); err != nil {
		return
	}

	// Upstream -> client only (agy r2 F7; body already streamed).
	relayOneWay(ctx, conn, upstream)
}

// copyRequestBody streams exactly one request body: bytes already
// buffered in br (the tail of the body a header-read swallowed),
// then from the raw connection, capped at Content-Length. No body →
// nothing copied. Chunked bodies ride the raw connection until the
// framing's terminator — cap them at the same budget a hostile cell
// should not be able to exceed anyway (16 MiB).
func copyRequestBody(conn net.Conn, br *bufio.Reader, upstream io.Writer, headers []string) error {
	contentLen := int64(-1)
	chunked := false
	for _, h := range headers {
		i := strings.IndexByte(h, ':')
		if i < 0 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(h[:i])) {
		case "content-length":
			if n, err := strconv.ParseInt(strings.TrimSpace(h[i+1:]), 10, 64); err == nil {
				contentLen = n
			}
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(h[i+1:]), "chunked") {
				chunked = true
			}
		}
	}
	if contentLen == 0 {
		return nil
	}
	if contentLen > maxRelayBody {
		return fmt.Errorf("body too large: %d bytes", contentLen)
	}
	src := io.MultiReader(br, conn)
	switch {
	case contentLen >= 0:
		src = io.LimitReader(src, contentLen)
	case chunked:
		// Framed body of unknown length: bounded budget; the
		// upstream's own chunk parser terminates the copy at
		// the zero-size chunk well before this cap in
		// practice.
		src = io.LimitReader(src, maxRelayBody)
	default:
		// No Content-Length, not chunked: no body on the wire.
		return nil
	}
	_, err := io.Copy(upstream, src)
	return err
}

// forwardCONNECT establishes the TLS tunnel: destination floor + dial,
// 200, raw copy. Headers were already drained from br in handle; the
// client side of the relay is io.MultiReader(br, conn) so any
// ClientHello bytes br had already buffered reach upstream (agy F1).
func (s *Server) forwardCONNECT(ctx context.Context, conn net.Conn, br *bufio.Reader, target string) {
	upstream, err := dialUpstream(ctx, target)
	if err != nil {
		if errors.Is(err, ErrFloor) {
			s.logEntry(conn.RemoteAddr(), http.MethodConnect, target, "refused-floor")
		} else {
			s.logEntry(conn.RemoteAddr(), http.MethodConnect, target, "connect-dial-fail")
		}
		rstHangup(conn)
		return
	}
	defer upstream.Close()
	s.logEntry(conn.RemoteAddr(), http.MethodConnect, target, "connect")

	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	relay(ctx, conn, br, upstream)
}

// relay copies both directions with 32 KiB buffers; EOF/cancel on
// either side closes both (spec: EOF/ctx-cancel closes both sides).
// client is the buffered continuation of the guest connection (br
// first, raw socket after); hostSide is the proxy-side socket whose
// Close unblocks the copies.
func relay(ctx context.Context, hostSide net.Conn, client io.Reader, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		buf := make([]byte, copyBufSize)
		_, _ = io.CopyBuffer(upstream, client, buf)
		done <- struct{}{}
	}()
	go func() {
		buf := make([]byte, copyBufSize)
		_, _ = io.CopyBuffer(hostSide, upstream, buf)
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	hostSide.Close()
	upstream.Close()
}

// relayOneWay streams the upstream's response back to the client
// only. forwardHTTP uses this (agy r2 F7): the request body was
// already copied exactly, so keeping the client->upstream direction
// open would hand any pipelined second request to THIS upstream —
// unlogged, unfloored. Closing after the response gives Connection:
// close semantics on both hops.
func relayOneWay(ctx context.Context, hostSide net.Conn, upstream net.Conn) {
	done := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, copyBufSize)
		_, _ = io.CopyBuffer(hostSide, upstream, buf)
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	hostSide.Close()
	upstream.Close()
}

// withDefaultPort returns u.Host with the scheme's default port
// appended when the URL omits it (net.Dial refuses bare "example.com").
// Hostname() is mandatory, not Host: for IPv6 literals Host carries
// brackets, and JoinHostPort would double-wrap them into
// "[[::1]]:80" — unparseable downstream (agy r2 F4).
func withDefaultPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "https" {
		return net.JoinHostPort(u.Hostname(), "443")
	}
	return net.JoinHostPort(u.Hostname(), "80")
}

// dialUpstream applies the destination floor (E3) and connects.
//
// Resolution is done FIRST and the connection is dialed to the
// validated IP literal: resolving-then-dialing-by-name would let DNS
// answer differently on the second lookup (classic rebind). Host
// semantics survive: the HTTP absolute-form request line and Host
// header carry the name verbatim, and TLS SNI for CONNECT tunnels is
// end-to-end inside the tunnel — the proxy never terminates it.
//
// ANY resolved address on the forbidden list rejects the whole
// request (not "try the next one": a DNS record mixing a public and a
// loopback IP is an attack, not a hint).
func dialUpstream(ctx context.Context, hostPort string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, IdleTimeout)
	defer cancel()

	ip, err := resolveFloor(ctx, host)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, port))
	if err != nil {
		return nil, err
	}
	// Keepalive so dead peers surface as errors within the idle
	// window rather than hanging relay forever.
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(60 * time.Second)
	}
	return conn, nil
}

// resolveFloor resolves host and returns the first usable IP, failing
// the whole request if ANY answer trips the floor. Hostname literals
// are checked without resolution.
func resolveFloor(ctx context.Context, host string) (string, error) {
	localAddrs, err := hostInterfaceAddrs()
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
		return forbiddenDest(ip) || isLocal(ip)
	}
	if ip := net.ParseIP(host); ip != nil {
		if reject(ip) {
			return "", fmt.Errorf("%w: destination %s", ErrFloor, host)
		}
		return host, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r := net.DefaultResolver
	ips, err := r.LookupIPAddr(ctx, host)
	if err != nil {
		// Resolution failed (NXDOMAIN, timeout, ...): fail closed
		// by propagating — dialing on a failed answer would skip
		// the floor check (agy F4 fail-open review).
		return "", err
	}
	for _, a := range ips {
		if reject(a.IP) {
			return "", fmt.Errorf("%w: destination %s", ErrFloor, host)
		}
	}
	if len(ips) > 0 {
		return ips[0].IP.String(), nil
	}
	return "", fmt.Errorf("no addresses for %s", host)
}

// skipHopByHop drops headers that must not cross a proxy.
// Transfer-Encoding is deliberately NOT on this list (agy F7): this is
// a stream relay, not a message re-encoder — stripping chunked framing
// turns a chunked POST into an upstream-smuggled pipeline (RFC 9112
// §6.3 forbids exactly this).
func skipHopByHop(name string) bool {
	switch name {
	case "proxy-connection", "proxy-authorization", "keep-alive",
		"te", "trailer", "upgrade", "connection":
		return true
	}
	return false
}

// logEntry appends one access-log line: UTC RFC3339, cell ip, method,
// host[:port], outcome (C7's evidence the packet took the door).
func (s *Server) logEntry(remote fmt.Stringer, method, host, outcome string) {
	f, err := os.OpenFile(s.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return // logging must never take the proxy down
	}
	defer f.Close()
	s.logMu.Lock()
	defer s.logMu.Unlock()
	// Audit line is fire-and-forget: a failed write must never take
	// the proxy down (best-effort by design).
	_, _ = fmt.Fprintf(f, "%s %s %s %s %s\n",
		time.Now().UTC().Format(time.RFC3339), ipOnly(remote.String()), method, host, outcome)
}

// ipOnly strips the port from an address string.
func ipOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// rstHangup forces an immediate RST on close (SO_LINGER 0). Used for
// every refusal: no bytes, no FIN handshake, exactly a refused
// connection from the guest's point of view.
func rstHangup(c net.Conn) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	//nolint:errcheck // RST is best-effort by nature
	tc.SetLinger(0)
}
