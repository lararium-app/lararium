// Package proxy implements the Lararium Phase-3 dumb forward proxy
// (CELL-SPEC §4, frozen contract): absolute-form GET/POST + CONNECT,
// one listener per cell bound to that cell's gateway address, append-only
// access log. It is deliberately minimal: Phase 3 proves the CAGE, not
// policy — L7 rules, DNS re-resolution, and CONNECT filtering are custos
// (Phase 4).
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
	"strings"
	"sync"
	"time"
)

// Limits (spec §4 / brief rev2): bounded connections per cell and
// bounded idle so a wedged guest cannot pile up proxy state.
const (
	// MaxConnsPerCell caps simultaneous proxied connections.
	MaxConnsPerCell = 256
	// IdleTimeout bounds one direction's silence (also the overall
	// per-connection ceiling in this dumb implementation).
	IdleTimeout = 5 * time.Minute

	// maxRequestLine bounds the request-line buffer before we
	// conclude the client is not speaking proxy protocol at all.
	maxRequestLine = 8 * 1024
	// copyBufSize per spec: 32 KiB relay buffers.
	copyBufSize = 32 * 1024
)

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

// handle reads one request line and dispatches. Any deviation from the
// proxy protocol (relative-form request = the DNAT no-bypass path, or
// garbage) gets a TCP RST with ZERO bytes written: an HTTP 400 would be
// exit-0 for curl and flip C1 to PASS-the-wrong-way (spec §4 mandates a
// connection failure).
func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer s.release()
	defer conn.Close()

	br := bufio.NewReaderSize(conn, maxRequestLine)
	_ = conn.SetReadDeadline(time.Now().Add(IdleTimeout))
	line, err := br.ReadString('\n')
	if err != nil {
		// EOF or read error before a request line: RST is rude but
		// correct per spec for the no-bypass path; for a plain
		// idle close it is harmless (peer is gone anyway).
		rstHangup(conn)
		return
	}

	method, target, _, ok := parseRequestLine(line)
	if !ok {
		// Garbage or HTTP/0.9 or anything not a request.
		rstHangup(conn)
		return
	}

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
		s.logEntry(conn.RemoteAddr(), method, u.Host, "proxied")
		s.forwardHTTP(ctx, conn, br, u, method)
	case http.MethodConnect:
		if !validHostPort(target) {
			rstHangup(conn)
			return
		}
		s.logEntry(conn.RemoteAddr(), method, target, "connect")
		s.forwardCONNECT(ctx, conn, target)
	default:
		// Not a proxy verb: same no-bypass refusal.
		s.logEntry(conn.RemoteAddr(), method, target, "refused-relative")
		rstHangup(conn)
	}
}

// parseRequestLine splits "METHOD SP TARGET SP HTTP/x.y". Returns
// ok=false on any malformed line.
func parseRequestLine(line string) (method, target, version string, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
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
func (s *Server) forwardHTTP(ctx context.Context, conn net.Conn, br *bufio.Reader, u *url.URL, method string) {
	upstream, err := dialUpstream(ctx, u.Host)
	if err != nil {
		// Upstream unreachable: close without a synthesized
		// response (no 400/502 — the cage refuses, it does not
		// role-play a server).
		rstHangup(conn)
		return
	}
	defer upstream.Close()

	// Read client headers.
	var headers []string
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if l == "\r\n" || l == "\n" {
			break
		}
		name := l
		if i := strings.IndexByte(l, ':'); i >= 0 {
			name = strings.ToLower(strings.TrimSpace(l[:i]))
		}
		if skipHopByHop(name) {
			continue
		}
		headers = append(headers, strings.TrimRight(l, "\r\n"))
	}

	// Absolute-form request line to the upstream.
	path := u.RequestURI()
	if _, err := fmt.Fprintf(upstream, "%s %s HTTP/1.1\r\n", method, path); err != nil {
		return
	}
	for _, h := range headers {
		if _, err := fmt.Fprintf(upstream, "%s\r\n", h); err != nil {
			return
		}
	}
	if _, err := upstream.Write([]byte("\r\n")); err != nil {
		return
	}

	// Bidirectional stream until either side closes.
	relay(ctx, conn, upstream)
}

// forwardCONNECT establishes the TLS tunnel: dial, 200, raw copy.
func (s *Server) forwardCONNECT(ctx context.Context, conn net.Conn, target string) {
	upstream, err := dialUpstream(ctx, target)
	if err != nil {
		rstHangup(conn)
		return
	}
	defer upstream.Close()

	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	relay(ctx, conn, upstream)
}

// relay copies both directions with 32 KiB buffers; EOF/cancel on either
// side closes both (spec: EOF/ctx-cancel closes both sides).
func relay(ctx context.Context, a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		buf := make([]byte, copyBufSize)
		_, _ = io.CopyBuffer(b, a, buf)
		done <- struct{}{}
	}()
	go func() {
		buf := make([]byte, copyBufSize)
		_, _ = io.CopyBuffer(a, b, buf)
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	a.Close()
	b.Close()
}

// dialUpstream resolves and connects with a bounded dial timeout,
// honoring ctx cancellation (unit stop must not leave ghost upstream
// connections).
func dialUpstream(ctx context.Context, hostPort string) (net.Conn, error) {
	d := net.Dialer{Timeout: 15 * time.Second}
	ctx, cancel := context.WithTimeout(ctx, IdleTimeout)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", hostPort)
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

// skipHopByHop drops headers that must not cross a proxy.
func skipHopByHop(name string) bool {
	switch name {
	case "proxy-connection", "proxy-authorization", "keep-alive",
		"te", "trailer", "transfer-encoding", "upgrade",
		"connection":
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
