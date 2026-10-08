package custos

// This file implements the fan-out door channel (doors.sock) per CUSTOS-SPEC §6.4b (amendment v9).

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/lararium-app/lararium/internal/surface"
)

var nameRegex = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// DoorPaths returns the paths to doors.sock and door.token in stateDir.
func DoorPaths(stateDir string) (sockPath string, tokenPath string) {
	return filepath.Join(stateDir, "doors.sock"), filepath.Join(stateDir, "door.token")
}

// EnsureDoorToken reads door.token or creates it with mode 0600 if absent per CUSTOS-SPEC §6.4b.
// The token is 32 random bytes stored as 64 lowercase hex characters.
func EnsureDoorToken(stateDir string) (string, error) {
	_, tokenPath := DoorPaths(stateDir)
	if data, err := os.ReadFile(tokenPath); err == nil {
		tok := strings.TrimSpace(string(data))
		if len(tok) == 64 && isHex(tok) {
			return tok, nil
		}
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate door token entropy: %w", err)
	}
	tok := hex.EncodeToString(b)

	tmp, err := os.CreateTemp(stateDir, "doortoken-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp door token: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), tokenPath); err != nil {
		return "", err
	}
	return tok, nil
}

// ReadDoorToken reads the token from door.token file in stateDir.
func ReadDoorToken(stateDir string) (string, error) {
	_, tokenPath := DoorPaths(stateDir)
	b, err := os.ReadFile(tokenPath)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("empty door.token")
	}
	return tok, nil
}

func isHex(s string) bool {
	for i := range len(s) {
		if !strings.ContainsRune("0123456789abcdef", rune(s[i])) {
			return false
		}
	}
	return true
}

// GoneWire represents the JSON payload of a GONE frame per CUSTOS-SPEC §6.4b.
type GoneWire struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// DoorServerOption configures a DoorServer instance.
type DoorServerOption func(*DoorServer)

// WithVault attaches the vault to the door server.
func WithVault(v *Vault) DoorServerOption {
	return func(s *DoorServer) {
		s.vault = v
	}
}

// WithWorkers attaches the worker server to the door server.
func WithWorkers(w *WorkerServer) DoorServerOption {
	return func(s *DoorServer) {
		s.workers = w
	}
}

// WithProxy attaches the proxy to the door server.
func WithProxy(p *Proxy) DoorServerOption {
	return func(s *DoorServer) {
		s.proxy = p
	}
}

type doorEvent struct {
	isPending bool
	id        string
	frame     string
}

type doorClient struct {
	server      *DoorServer
	conn        net.Conn
	name        string
	outbound    chan string // capacity 64
	writeMu     sync.Mutex
	closeOnce   sync.Once
	closed      atomic.Bool
	snapshotIDs map[string]bool
	bufferMu    sync.Mutex
	buffered    []doorEvent
	ready       bool
}

func newDoorClient(server *DoorServer, conn net.Conn, name string) *doorClient {
	return &doorClient{
		server:      server,
		conn:        conn,
		name:        name,
		outbound:    make(chan string, 64),
		snapshotIDs: make(map[string]bool),
	}
}

func (c *doorClient) getUnreadBytes() int {
	if uc, ok := c.conn.(*net.UnixConn); ok {
		raw, err := uc.SyscallConn()
		if err != nil {
			return 0
		}
		var outq int
		_ = raw.Control(func(fd uintptr) {
			outq, _ = unix.IoctlGetInt(int(fd), unix.TIOCOUTQ)
		})
		return outq
	}
	return 0
}

func (c *doorClient) writeRaw(data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := c.conn.Write(data)
	return err
}

func (c *doorClient) writeLoop() {
	defer c.Close()
	for frame := range c.outbound {
		if c.closed.Load() {
			return
		}
		// Wait for socket output buffer to drain so that slow clients (that stopped reading)
		// retain items in c.outbound until queue overflow (64 frames).
		for {
			unread := c.getUnreadBytes()
			if unread == 0 || c.closed.Load() {
				break
			}
			time.Sleep(1 * time.Millisecond)
		}
		if c.closed.Load() {
			return
		}
		if err := c.writeRaw([]byte(frame)); err != nil {
			return
		}
	}
}

func (c *doorClient) enqueueEvent(ev doorEvent) {
	c.bufferMu.Lock()
	if !c.ready {
		c.buffered = append(c.buffered, ev)
		c.bufferMu.Unlock()
		return
	}
	c.bufferMu.Unlock()

	if ev.isPending && c.snapshotIDs[ev.id] {
		return
	}

	select {
	case c.outbound <- ev.frame:
	default:
		// 64-frame outbound queue overflow: drop the slow door connection
		c.Close()
	}
}

func (c *doorClient) drainBuffer() {
	c.bufferMu.Lock()
	buffered := c.buffered
	c.buffered = nil
	c.ready = true
	c.bufferMu.Unlock()

	for _, ev := range buffered {
		if ev.isPending && c.snapshotIDs[ev.id] {
			continue
		}
		select {
		case c.outbound <- ev.frame:
		default:
			c.Close()
			return
		}
	}
}

// Close shuts the client connection once and deregisters it from the server.
func (c *doorClient) Close() {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		_ = c.conn.Close()
		c.server.removeClient(c)
	})
}

// DoorServer serves external approval door connections over doors.sock per CUSTOS-SPEC §6.4b.
type DoorServer struct {
	mu        sync.RWMutex
	stateDir  string
	sockPath  string
	tokenPath string
	token     string
	vault     *Vault
	workers   *WorkerServer
	proxy     *Proxy
	hub       *surface.ApprovalHub
	ln        net.Listener
	clients   map[*doorClient]bool
	done      chan struct{}
	closeOnce sync.Once
}

// StateDir returns the state directory for the door server.
func (s *DoorServer) StateDir() string {
	return s.stateDir
}

// SetVault attaches the Vault to the door server.
func (s *DoorServer) SetVault(v *Vault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vault = v
}

// SetWorkers attaches the WorkerServer to the door server.
func (s *DoorServer) SetWorkers(w *WorkerServer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workers = w
}

// SetProxy attaches the Proxy to the door server.
func (s *DoorServer) SetProxy(p *Proxy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proxy = p
}

func (s *DoorServer) vaultInstance() *Vault {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.vault
}

func (s *DoorServer) workersInstance() *WorkerServer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.workers
}

func (s *DoorServer) proxyInstance() *Proxy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.proxy
}

type chainedFanout struct {
	first  surface.ApprovalFanout
	second surface.ApprovalFanout
}

// ApprovalPending forwards a pending notification to both fans.
func (c *chainedFanout) ApprovalPending(id, sessionID, name, argsSummary string) {
	c.first.ApprovalPending(id, sessionID, name, argsSummary)
	c.second.ApprovalPending(id, sessionID, name, argsSummary)
}

// ApprovalTerminal forwards a terminal notification to both fans.
func (c *chainedFanout) ApprovalTerminal(id, sessionID, state, reason, source string) {
	c.first.ApprovalTerminal(id, sessionID, state, reason, source)
	c.second.ApprovalTerminal(id, sessionID, state, reason, source)
}

// ApprovalTerminalVia forwards a via-attributed terminal notification to both
// fans, falling back to ApprovalTerminal for fans without via support.
func (c *chainedFanout) ApprovalTerminalVia(id, sessionID, state, reason, source, via string) {
	if fv, ok := c.first.(surface.ApprovalTerminalVia); ok {
		fv.ApprovalTerminalVia(id, sessionID, state, reason, source, via)
	} else {
		c.first.ApprovalTerminal(id, sessionID, state, reason, source)
	}

	if fv, ok := c.second.(surface.ApprovalTerminalVia); ok {
		fv.ApprovalTerminalVia(id, sessionID, state, reason, source, via)
	} else {
		c.second.ApprovalTerminal(id, sessionID, state, reason, source)
	}
}

// StartDoorServer binds doors.sock and starts the door server per CUSTOS-SPEC §6.4b.
func StartDoorServer(stateDir string, hub *surface.ApprovalHub, opts ...DoorServerOption) (*DoorServer, error) {
	sockPath, tokenPath := DoorPaths(stateDir)

	token, err := EnsureDoorToken(stateDir)
	if err != nil {
		return nil, fmt.Errorf("ensure door.token: %w", err)
	}

	// State directory law: parent dir must be 0700, refuse symlinked or foreign dirs
	fi, err := os.Lstat(stateDir)
	if err != nil {
		return nil, fmt.Errorf("state dir stat: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("state dir cannot be a symlink: %s", stateDir)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("state dir is not a directory: %s", stateDir)
	}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		if sys.Uid != uint32(os.Geteuid()) { //nolint:gosec // G115: euid is non-negative by construction
			return nil, fmt.Errorf("state dir owned by foreign uid %d", sys.Uid)
		}
	}
	if fi.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(stateDir, 0o700)
	}

	// Stale socket handling: dial to check if another daemon is listening
	if _, err := os.Stat(sockPath); err == nil {
		d := net.Dialer{Timeout: 1 * time.Second}
		conn, dialErr := d.DialContext(context.Background(), "unix", sockPath)
		if dialErr == nil {
			conn.Close()
			return nil, fmt.Errorf("another door server is listening on %s", sockPath)
		}
		_ = os.Remove(sockPath)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen doors.sock: %w", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		ln.Close()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("chmod doors.sock: %w", err)
	}

	ds := &DoorServer{
		stateDir:  stateDir,
		sockPath:  sockPath,
		tokenPath: tokenPath,
		token:     token,
		hub:       hub,
		ln:        ln,
		clients:   make(map[*doorClient]bool),
		done:      make(chan struct{}),
	}

	for _, opt := range opts {
		opt(ds)
	}

	// Register on hub as fanout subscriber (chain if existing fanout is present)
	if hub != nil {
		existing := hub.Fanout()
		if existing != nil {
			hub.SetFanout(&chainedFanout{first: existing, second: ds})
		} else {
			hub.SetFanout(ds)
		}
	}

	go ds.acceptLoop()
	return ds, nil
}

func (s *DoorServer) acceptLoop() {
	defer close(s.done)
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *DoorServer) addClient(c *doorClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[c] = true
}

func (s *DoorServer) removeClient(c *doorClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, c)
}

func (s *DoorServer) clientList() []*doorClient {
	list := make([]*doorClient, 0, len(s.clients))
	for c := range s.clients {
		list = append(list, c)
	}
	return list
}

func (s *DoorServer) handleConn(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	firstLine, err := reader.ReadString('\n')
	if err != nil && firstLine == "" {
		return
	}
	firstLine = strings.TrimRight(firstLine, "\r\n")

	// Pre-HELLO validation: command MUST be HELLO
	if !strings.HasPrefix(firstLine, "HELLO ") {
		_, _ = conn.Write([]byte("ERR bad_token\n"))
		return
	}

	parts := strings.Split(firstLine, " ")
	if len(parts) != 3 {
		_, _ = conn.Write([]byte("ERR bad_token\n"))
		return
	}

	token := parts[1]
	name := parts[2]

	if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
		_, _ = conn.Write([]byte("ERR bad_token\n"))
		return
	}

	if !nameRegex.MatchString(name) {
		_, _ = conn.Write([]byte("ERR bad_name\n"))
		return
	}

	client := newDoorClient(s, conn, name)
	go client.writeLoop()
	defer client.Close()

	// Atomic replay: attach subscription and take snapshot
	var snapshotWires []ApprovalCardWire
	v := s.vaultInstance()
	switch {
	case v != nil && !v.IsUnlocked():
		s.addClient(client)
		snapshotWires = []ApprovalCardWire{}
	case s.hub != nil:
		_ = s.hub.PendingSnapshot(func() {
			s.addClient(client)
		})
		snapshotWires = CardSnapshot(s.hub, s.workersInstance(), s.proxyInstance())
	default:
		s.addClient(client)
		snapshotWires = []ApprovalCardWire{}
	}

	client.snapshotIDs = make(map[string]bool)
	for _, card := range snapshotWires {
		client.snapshotIDs[card.ID] = true
	}

	b, err := json.Marshal(snapshotWires) //nolint:gosec // ApprovalCardWire.Cred carries provider/credential name, never secret value
	if err != nil {
		return
	}
	if err := client.writeRaw([]byte("OK " + string(b) + "\n")); err != nil {
		return
	}

	// Drain any events that arrived during the attach snapshot window
	client.drainBuffer()

	// Subsequent command loop
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		cmdLine := strings.TrimSpace(scanner.Text())
		if cmdLine == "" {
			continue
		}
		s.handleClientCommand(client, cmdLine)
	}
}

func (s *DoorServer) handleClientCommand(client *doorClient, line string) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return
	}

	switch fields[0] {
	case "APPROVE":
		// APPROVE <id> once|always via web|telegram
		if len(fields) != 5 || fields[3] != "via" {
			_ = client.writeRaw([]byte("ERR bad_source\n"))
			return
		}
		mode := fields[2]
		via := fields[4]
		if (mode != "once" && mode != "always") || (via != "web" && via != "telegram") {
			_ = client.writeRaw([]byte("ERR bad_source\n"))
			return
		}
		cardID := fields[1]
		s.handleApprove(client, cardID, mode == "always", via)

	case "DENY":
		// DENY <id> via web|telegram
		if len(fields) != 4 || fields[2] != "via" {
			_ = client.writeRaw([]byte("ERR bad_source\n"))
			return
		}
		via := fields[3]
		if via != "web" && via != "telegram" {
			_ = client.writeRaw([]byte("ERR bad_source\n"))
			return
		}
		cardID := fields[1]
		s.handleDeny(client, cardID, via)

	default:
		_ = client.writeRaw([]byte("ERR bad_source\n"))
	}
}

func (s *DoorServer) handleApprove(client *doorClient, cardID string, always bool, via string) {
	v := s.vaultInstance()
	if v != nil && !v.IsUnlocked() {
		_ = client.writeRaw([]byte("ERR locked\n"))
		return
	}

	if s.hub == nil {
		_ = client.writeRaw([]byte("ERR no_such_card\n"))
		return
	}

	// IP-literal check per CUSTOS-SPEC §6.4b, §6.5
	if always && s.isIPLiteralTarget(cardID) {
		_ = client.writeRaw([]byte("ERR ip_ask_only\n"))
		return
	}

	session, cred := s.findSessionAndCred(cardID)

	status := s.hub.ResolveFromVia(session, cardID, true, "surface", via)
	switch status {
	case 200:
		if always {
			s.storeAlwaysRule(cardID, "surface")
		}
		if v != nil && v.Audit() != nil {
			_ = v.Audit().AppendApprovalAnsweredVia(cred, "ok", "surface", via)
		}
		_ = client.writeRaw([]byte("OK\n"))
	case 410:
		origReason := s.hub.Reason(session, cardID)
		if isStaleReason(origReason) {
			_ = client.writeRaw([]byte("ERR stale_verdict\n"))
		} else {
			_ = client.writeRaw([]byte("ERR already_answered\n"))
		}
	case 404:
		_ = client.writeRaw([]byte("ERR no_such_card\n"))
	default:
		_ = client.writeRaw([]byte(fmt.Sprintf("ERR %d\n", status)))
	}
}

func (s *DoorServer) handleDeny(client *doorClient, cardID string, via string) {
	v := s.vaultInstance()
	if v != nil && !v.IsUnlocked() {
		_ = client.writeRaw([]byte("ERR locked\n"))
		return
	}

	if s.hub == nil {
		_ = client.writeRaw([]byte("ERR no_such_card\n"))
		return
	}

	session, cred := s.findSessionAndCred(cardID)

	status := s.hub.ResolveFromVia(session, cardID, false, "surface", via)
	switch status {
	case 200:
		if v != nil && v.Audit() != nil {
			_ = v.Audit().AppendApprovalAnsweredVia(cred, "denied", "surface", via)
		}
		_ = client.writeRaw([]byte("OK\n"))
	case 410:
		_ = client.writeRaw([]byte("ERR already_answered\n"))
	case 404:
		_ = client.writeRaw([]byte("ERR no_such_card\n"))
	default:
		_ = client.writeRaw([]byte(fmt.Sprintf("ERR %d\n", status)))
	}
}

func isStaleReason(reason string) bool {
	switch reason {
	case "timed_out", "timeout", "flow_gone", "credential_revoked", "custos_locked":
		return true
	default:
		return false
	}
}

func (s *DoorServer) findSessionAndCred(cardID string) (session string, cred string) {
	if s.hub != nil {
		for _, card := range s.hub.Pending() {
			if card.ID == cardID {
				session = card.Cell
				cred = card.Cred
				break
			}
		}
		if session == "" {
			if sid, ok := s.hub.SessionOf(cardID); ok {
				session = sid
			}
		}
	}

	workers := s.workersInstance()
	if cred == "" && workers != nil && workers.parkMgr != nil {
		workers.parkMgr.mu.Lock()
		for _, cellParks := range workers.parkMgr.byCell {
			if flow, ok := cellParks[cardID]; ok {
				cred = flow.cred
				if session == "" {
					session = flow.cellID
				}
				break
			}
		}
		workers.parkMgr.mu.Unlock()
	}

	proxy := s.proxyInstance()
	if cred == "" && proxy != nil && proxy.parkMgr != nil {
		proxy.parkMgr.mu.Lock()
		for _, cellParks := range proxy.parkMgr.byCell {
			if flow, ok := cellParks[cardID]; ok {
				cred = flow.cred
				if session == "" {
					session = flow.cellID
				}
				break
			}
		}
		proxy.parkMgr.mu.Unlock()
	}

	return session, cred
}

func (s *DoorServer) isIPLiteralTarget(cardID string) bool {
	workers := s.workersInstance()
	if workers != nil && workers.parkMgr != nil {
		workers.parkMgr.mu.Lock()
		for _, cellParks := range workers.parkMgr.byCell {
			if flow, ok := cellParks[cardID]; ok {
				workers.parkMgr.mu.Unlock()
				return isIPString(flow.host)
			}
		}
		workers.parkMgr.mu.Unlock()
	}

	proxy := s.proxyInstance()
	if proxy != nil && proxy.parkMgr != nil {
		proxy.parkMgr.mu.Lock()
		for _, cellParks := range proxy.parkMgr.byCell {
			if flow, ok := cellParks[cardID]; ok {
				proxy.parkMgr.mu.Unlock()
				return isIPString(flow.host)
			}
		}
		proxy.parkMgr.mu.Unlock()
	}

	// Check hub card Dest if present
	if s.hub != nil {
		for _, card := range s.hub.Pending() {
			if card.ID == cardID {
				return isIPString(card.Cred)
			}
		}
	}
	return false
}

func isIPString(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	// Strip path
	if idx := strings.Index(raw, "/"); idx != -1 {
		raw = raw[:idx]
	}
	// Bracketed IPv6: [::1]:8080 or [::1]
	if strings.HasPrefix(raw, "[") {
		idx := strings.Index(raw, "]")
		if idx != -1 {
			return net.ParseIP(raw[1:idx]) != nil
		}
	}
	// Strip port
	if idx := strings.LastIndex(raw, ":"); idx != -1 {
		if strings.Count(raw, ":") == 1 {
			raw = raw[:idx]
		}
	}
	return net.ParseIP(raw) != nil
}

func (s *DoorServer) storeAlwaysRule(cardID, actor string) {
	v := s.vaultInstance()
	if v == nil || v.Policy() == nil {
		return
	}

	workers := s.workersInstance()
	if workers != nil && workers.parkMgr != nil {
		workers.parkMgr.mu.Lock()
		for _, cellParks := range workers.parkMgr.byCell {
			if flow, ok := cellParks[cardID]; ok {
				tool := flow.path
				cname := flow.cred
				workers.parkMgr.mu.Unlock()
				pat := cname
				if tool != "" {
					pat = cname + "/" + tool
				}
				_, _ = v.Policy().AddCredentialRule(pat, "auto", true, "always", actor)
				return
			}
		}
		workers.parkMgr.mu.Unlock()
	}

	proxy := s.proxyInstance()
	if proxy != nil && proxy.parkMgr != nil {
		proxy.parkMgr.mu.Lock()
		for _, cellParks := range proxy.parkMgr.byCell {
			if flow, ok := cellParks[cardID]; ok {
				cname := flow.cred
				host := flow.host
				port := flow.port
				proxy.parkMgr.mu.Unlock()
				if cname != "" {
					_, _ = v.Policy().AddCredentialRule(cname, "auto", true, "always", actor)
					return
				}
				pat := host
				if port != 0 && port != 80 {
					pat = fmt.Sprintf("%s:%d", host, port)
				}
				_, _ = v.Policy().AddEgressRule(pat, "auto", true, "always", actor)
				return
			}
		}
		proxy.parkMgr.mu.Unlock()
	}
}

// buildCardWire renders the §6.4b CARD frame for a hub pending fan-out.
// CRITICAL LOCKING RULE: ApprovalPending fires synchronously from
// RegisterCustos, which every custody park path invokes while holding
// parkMgr.mu (the register-inside-the-lock invariant that keeps a global
// shed from orphaning a card). Go mutexes are not reentrant, so the
// flow-enrichment lookups below MUST use TryLock: contended means the
// card id was just minted on this very goroutine and is by ordering not
// yet in any park map — the argsSummary fallback IS the correct wire.
// Blocking here once deadlocked the whole park manager (dogfood 2026-10-08).
func (s *DoorServer) buildCardWire(id, sessionID, name, argsSummary string) ApprovalCardWire {
	wire := ApprovalCardWire{
		ID:     id,
		Cell:   sessionID,
		Cred:   name,
		Tool:   "",
		Dest:   "",
		Review: "",
	}

	workers := s.workersInstance()
	if workers != nil && workers.parkMgr != nil && workers.parkMgr.mu.TryLock() {
		for _, cellParks := range workers.parkMgr.byCell {
			if flow, ok := cellParks[id]; ok {
				wire.Tool = flow.path
				wire.Review = flow.review
				if flow.cred != "" {
					wire.Cred = flow.cred
				}
				if flow.cellID != "" {
					wire.Cell = flow.cellID
				}
				now := time.Now()
				wire.AgeS = int(now.Sub(flow.createdAt).Seconds())
				if wire.AgeS < 0 {
					wire.AgeS = 0
				}
				wire.ExpiresInS = int(flow.createdAt.Add(workers.parkMgr.holdTimeout).Sub(now).Seconds())
				if wire.ExpiresInS < 0 {
					wire.ExpiresInS = 0
				}
				workers.parkMgr.mu.Unlock()
				return wire
			}
		}
		workers.parkMgr.mu.Unlock()
	}

	proxy := s.proxyInstance()
	if proxy != nil && proxy.parkMgr != nil && proxy.parkMgr.mu.TryLock() {
		for _, cellParks := range proxy.parkMgr.byCell {
			if flow, ok := cellParks[id]; ok {
				if flow.cred != "" {
					wire.Cred = flow.cred
				}
				if flow.cellID != "" {
					wire.Cell = flow.cellID
				}
				wire.Review = flow.review
				if flow.port > 0 {
					wire.Dest = fmt.Sprintf("%s:%d%s", flow.host, flow.port, flow.path)
				} else if flow.host != "" {
					wire.Dest = fmt.Sprintf("%s%s", flow.host, flow.path)
				}
				now := time.Now()
				wire.AgeS = int(now.Sub(flow.createdAt).Seconds())
				if wire.AgeS < 0 {
					wire.AgeS = 0
				}
				wire.ExpiresInS = int(flow.createdAt.Add(proxy.parkMgr.holdTimeout).Sub(now).Seconds())
				if wire.ExpiresInS < 0 {
					wire.ExpiresInS = 0
				}
				proxy.parkMgr.mu.Unlock()
				return wire
			}
		}
		proxy.parkMgr.mu.Unlock()
	}

	wire.Dest = argsSummary
	return wire
}

// ApprovalPending is called on hub pending transition outside the hub mutex.
func (s *DoorServer) ApprovalPending(id, sessionID, name, argsSummary string) {
	wire := s.buildCardWire(id, sessionID, name, argsSummary)
	b, err := json.Marshal(wire) //nolint:gosec // ApprovalCardWire.Cred carries provider/credential name, never secret value
	if err != nil {
		return
	}
	frame := "CARD " + string(b) + "\n"
	ev := doorEvent{
		isPending: true,
		id:        id,
		frame:     frame,
	}

	s.mu.RLock()
	clients := s.clientList()
	s.mu.RUnlock()

	for _, c := range clients {
		c.enqueueEvent(ev)
	}
}

// ApprovalTerminal is called on hub terminal transition without via.
func (s *DoorServer) ApprovalTerminal(id, sessionID, state, reason, source string) {
	s.ApprovalTerminalVia(id, sessionID, state, reason, source, "")
}

// ApprovalTerminalVia is called on hub terminal transition with via attribution.
func (s *DoorServer) ApprovalTerminalVia(id, sessionID, state, reason, source, via string) {
	gone := mapTerminalToGone(id, state, reason, source, via)
	b, err := json.Marshal(gone)
	if err != nil {
		return
	}
	frame := "GONE " + string(b) + "\n"
	ev := doorEvent{
		isPending: false,
		id:        id,
		frame:     frame,
	}

	s.mu.RLock()
	clients := s.clientList()
	s.mu.RUnlock()

	for _, c := range clients {
		c.enqueueEvent(ev)
	}
	if reason == "custos_locked" && len(clients) > 0 {
		time.Sleep(10 * time.Millisecond)
	}
}

// doorActorLabel resolves the actor label for a GONE frame reason: explicit
// via wins, ctl/cli collapse to "cli", otherwise the raw source is used.
func doorActorLabel(via, source string) string {
	switch {
	case via != "":
		return via
	case source == "ctl" || source == "cli":
		return "cli"
	default:
		return source
	}
}

func mapTerminalToGone(id, state, reason, source, via string) GoneWire {
	goneState := state
	goneReason := reason

	switch {
	case reason == "credential_revoked" || reason == "custos_locked" || reason == "flow_gone":
		goneState = "cancelled"
		goneReason = reason

	case state == "timed_out" || reason == "timeout" || reason == "timed_out":
		goneState = "timed_out"
		goneReason = "timer"

	case state == "approved":
		goneState = "approved"
		goneReason = doorActorLabel(via, source)

	case state == "denied":
		if reason == "cancelled" {
			goneState = "cancelled"
			goneReason = reason
		} else {
			goneState = "denied"
			goneReason = doorActorLabel(via, source)
		}

	case state == "cancelled":
		goneState = "cancelled"
		if reason != "" {
			goneReason = reason
		} else {
			goneReason = source
		}
	}

	return GoneWire{
		ID:     id,
		State:  goneState,
		Reason: goneReason,
	}
}

// Close closes the listener and unlinks doors.sock.
func (s *DoorServer) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		clients := s.clientList()
		s.mu.Unlock()

		for _, c := range clients {
			c.Close()
		}

		err = s.ln.Close()
		<-s.done
		_ = os.Remove(s.sockPath)
	})
	return err
}

// DoorConn is a client connection to doors.sock for testing and external integrations.
type DoorConn struct {
	conn      net.Conn
	reader    *bufio.Reader
	cards     []ApprovalCardWire
	mu        sync.Mutex
	eventChan chan string
}

// DialDoor connects to doors.sock, authenticates via HELLO, and returns the DoorConn.
func DialDoor(sockPath, token, name string) (*DoorConn, error) {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(context.Background(), "unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("dial doors.sock: %w", err)
	}

	reader := bufio.NewReader(conn)
	helloCmd := fmt.Sprintf("HELLO %s %s\n", token, name)
	if _, err := conn.Write([]byte(helloCmd)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send HELLO: %w", err)
	}

	resp, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read HELLO response: %w", err)
	}
	resp = strings.TrimRight(resp, "\r\n")

	if !strings.HasPrefix(resp, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("HELLO failed: %s", resp)
	}

	rawJSON := strings.TrimPrefix(resp, "OK ")
	var cards []ApprovalCardWire
	if err := json.Unmarshal([]byte(rawJSON), &cards); err != nil {
		conn.Close()
		return nil, fmt.Errorf("parse snapshot cards: %w", err)
	}

	return &DoorConn{
		conn:      conn,
		reader:    reader,
		cards:     cards,
		eventChan: make(chan string, 128),
	}, nil
}

// Cards returns the snapshot cards received during HELLO.
func (c *DoorConn) Cards() []ApprovalCardWire {
	return c.cards
}

func (c *DoorConn) readAck() (string, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "CARD ") || strings.HasPrefix(trimmed, "GONE ") {
			select {
			case c.eventChan <- trimmed:
			default:
			}
			continue
		}
		return trimmed, nil
	}
}

// NextFrame reads the next line frame (CARD, GONE, etc.) with timeout.
func (c *DoorConn) NextFrame(timeout time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	select {
	case ev := <-c.eventChan:
		return ev, nil
	default:
	}

	if timeout > 0 {
		_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	} else {
		_ = c.conn.SetReadDeadline(time.Time{})
	}
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// Approve sends APPROVE and returns synchronous ack.
func (c *DoorConn) Approve(id string, always bool, via string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	mode := "once"
	if always {
		mode = "always"
	}
	cmd := fmt.Sprintf("APPROVE %s %s via %s\n", id, mode, via)
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Write([]byte(cmd)); err != nil {
		return "", err
	}
	return c.readAck()
}

// Deny sends DENY and returns synchronous ack.
func (c *DoorConn) Deny(id string, via string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cmd := fmt.Sprintf("DENY %s via %s\n", id, via)
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Write([]byte(cmd)); err != nil {
		return "", err
	}
	return c.readAck()
}

// RawSend sends raw bytes to the connection.
func (c *DoorConn) RawSend(line string) error {
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := c.conn.Write([]byte(line))
	return err
}

// RawReadLine reads raw line from the connection.
func (c *DoorConn) RawReadLine(timeout time.Duration) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readAck()
}

// Close closes the underlying network connection.
func (c *DoorConn) Close() error {
	return c.conn.Close()
}
