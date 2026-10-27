package custos

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lararium-app/lararium/internal/surface"
)

const (
	ctlTokenLen       = 32
	ctlTokenAlphabet  = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	ctlCommandTimeout = 5 * time.Second
)

// ErrCtlNotListening indicates no running daemon on ctl.sock.
var ErrCtlNotListening = errors.New("not listening")

// GenerateCtlToken mints a 32-character token from crypto/rand matching surface convention.
func GenerateCtlToken() (string, error) {
	alphabetLen := big.NewInt(int64(len(ctlTokenAlphabet)))
	b := make([]byte, ctlTokenLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, alphabetLen)
		if err != nil {
			return "", err
		}
		b[i] = ctlTokenAlphabet[n.Int64()]
	}
	return "lar1_" + string(b), nil
}

// CtlPaths returns the paths to ctl.sock and ctl.token in stateDir.
func CtlPaths(stateDir string) (sockPath string, tokenPath string) {
	return filepath.Join(stateDir, "ctl.sock"), filepath.Join(stateDir, "ctl.token")
}

// EnsureCtlToken reads ctl.token or creates it with mode 0600 if absent.
func EnsureCtlToken(stateDir string) (string, error) {
	_, tokenPath := CtlPaths(stateDir)
	if data, err := os.ReadFile(tokenPath); err == nil {
		tok := strings.TrimSpace(string(data))
		if tok != "" {
			return tok, nil
		}
	}

	tok, err := GenerateCtlToken()
	if err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(stateDir, "ctltoken-*.tmp")
	if err != nil {
		return "", err
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

// ReadCtlToken reads the token from ctl.token file in stateDir.
func ReadCtlToken(stateDir string) (string, error) {
	_, tokenPath := CtlPaths(stateDir)
	b, err := os.ReadFile(tokenPath)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", errors.New("empty ctl.token")
	}
	return tok, nil
}

// CtlServer serves the daemon control socket at ctl.sock (0600) per CUSTOS-SPEC §3, §C1.
type CtlServer struct {
	mu        sync.RWMutex
	stateDir  string
	sockPath  string
	tokenPath string
	token     string
	vault     *Vault
	proxy     *Proxy
	workers   *WorkerServer
	rail      *RailRegistry
	hub       *surface.ApprovalHub
	ln        net.Listener
	done      chan struct{}
	onStop    func()
}

// SetRailRegistry attaches the RailRegistry to the control server (§9).
func (s *CtlServer) SetRailRegistry(r *RailRegistry) {
	s.mu.Lock()
	s.rail = r
	w := s.workers
	s.mu.Unlock()
	if w != nil && r != nil {
		w.BindRailRegistry(r)
	}
}

func (s *CtlServer) railRegistry() *RailRegistry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.rail != nil {
		return s.rail
	}
	if s.workers != nil {
		return s.workers.RailRegistry()
	}
	return nil
}

// SetProxy attaches the custody proxy to the control server.
func (s *CtlServer) SetProxy(p *Proxy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proxy = p
}

// SetWorkers attaches the worker lane server to the control server (§5.1).
func (s *CtlServer) SetWorkers(w *WorkerServer) {
	s.mu.Lock()
	s.workers = w
	if s.hub == nil && w != nil && w.hub != nil {
		s.hub = w.hub
	}
	r := s.rail
	if r == nil && w != nil {
		r = w.RailRegistry()
	}
	s.mu.Unlock()
	if w != nil && r != nil {
		w.BindRailRegistry(r)
	}
}

// SetHub attaches the ApprovalHub to the control server (§6.4a).
func (s *CtlServer) SetHub(h *surface.ApprovalHub) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hub = h
	if h != nil && !h.HasStaleVerdictHook() && s.vault != nil && s.vault.Audit() != nil {
		h.SetStaleVerdictHook(func(id, sessionID, cred, cell, reason string) {
			_ = s.vault.Audit().Append(AuditRecord{
				Kind:   AuditKindStaleVerdict,
				Cred:   cred,
				Actor:  cell,
				Reason: reason,
			})
		})
	}
}

func (s *CtlServer) hubInstance() *surface.ApprovalHub {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.hub != nil {
		return s.hub
	}
	if s.vault != nil {
		return s.vault.ApprovalHub()
	}
	return nil
}

func (s *CtlServer) proxyInstance() *Proxy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.proxy
}

func (s *CtlServer) workersInstance() *WorkerServer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.workers
}

// StartCtlServer starts listening on ctl.sock with token authentication.
func StartCtlServer(stateDir string, v *Vault, onStop func()) (*CtlServer, error) {
	sockPath, tokenPath := CtlPaths(stateDir)

	token, err := EnsureCtlToken(stateDir)
	if err != nil {
		return nil, fmt.Errorf("ensure ctl.token: %w", err)
	}

	// Stale socket handling: dial to check if another daemon is listening
	if _, err := os.Stat(sockPath); err == nil {
		d := net.Dialer{Timeout: 1 * time.Second}
		conn, dialErr := d.DialContext(context.Background(), "unix", sockPath)
		if dialErr == nil {
			conn.Close()
			return nil, fmt.Errorf("another custosd is listening on %s", sockPath)
		}
		// Stale dead socket file: remove it
		_ = os.Remove(sockPath)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen ctl.sock: %w", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		ln.Close()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("chmod ctl.sock: %w", err)
	}

	cs := &CtlServer{
		stateDir:  stateDir,
		sockPath:  sockPath,
		tokenPath: tokenPath,
		token:     token,
		vault:     v,
		ln:        ln,
		done:      make(chan struct{}),
		onStop:    onStop,
	}

	go cs.acceptLoop()
	return cs, nil
}

func (s *CtlServer) acceptLoop() {
	defer close(s.done)
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *CtlServer) handleConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(ctlCommandTimeout))

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return
	}
	line = strings.TrimRight(line, "\r\n")

	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		fmt.Fprint(conn, "ERR bad_request\n")
		return
	}

	cmd := parts[0]
	tok := parts[1]

	// Verify ctl.token constant-time
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
		fmt.Fprint(conn, "ERR unauthorized\n")
		return
	}

	if s.handleSurrogateCmd(conn, cmd, parts) || s.handlePolicyCmd(conn, cmd, parts) || s.handleListenerCmd(conn, cmd, parts) || s.handleWorkerCmd(conn, cmd, parts) || s.handleApprovalCmd(conn, cmd, parts) || s.handleMcpCmd(conn, cmd, parts) {
		return
	}

	switch cmd {
	case "PING":
		fmt.Fprint(conn, "OK PONG\n")

	case "STATUS":
		failed := 0
		if p := s.proxyInstance(); p != nil {
			failed = p.ListenersFailed()
		}
		st := s.vault.Status(true, failed)
		b, err := json.Marshal(st)
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err)
			return
		}
		fmt.Fprintf(conn, "OK %s\n", string(b))

	case "UNLOCK":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR empty key\n")
			return
		}
		passphrase := parts[2]
		if err := s.vault.Unlock(passphrase, false); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return
		}
		fmt.Fprint(conn, "OK unlocked\n")

	case "LOCK":
		if err := s.vault.Lock(); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return
		}
		fmt.Fprint(conn, "OK locked\n")

	case "SHUTDOWN":
		fmt.Fprint(conn, "OK stopping\n")
		if s.onStop != nil {
			go s.onStop()
		}

	case "CHANGE-PASSPHRASE":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR empty key\n")
			return
		}
		pwParts := strings.SplitN(parts[2], "\x00", 2)
		if len(pwParts) != 2 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return
		}
		if err := s.vault.ChangePassphrase(pwParts[0], pwParts[1]); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return
		}
		fmt.Fprint(conn, "OK passphrase_changed\n")

	case "REVOKE-CREDENTIAL":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return
		}
		if err := s.vault.RevokeCredential("", parts[2], "cli"); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return
		}
		if reg := s.railRegistry(); reg != nil {
			reg.NotifyCredentialGone(parts[2])
		}
		fmt.Fprint(conn, "OK revoked\n")

	default:
		fmt.Fprint(conn, "ERR unknown_command\n")
	}
}

// Close closes the listener and unlinks ctl.sock.
func (s *CtlServer) Close() error {
	err := s.ln.Close()
	<-s.done
	_ = os.Remove(s.sockPath)
	return err
}

// CtlClient communicates with the daemon over ctl.sock.
type CtlClient struct {
	sockPath  string
	token     string
	timeout   time.Duration
	parkedOut io.Writer
}

// SetParkedWriter sets the destination for interim parked-approval notices
// (defaults to os.Stderr when nil).
func (c *CtlClient) SetParkedWriter(w io.Writer) {
	c.parkedOut = w
}

func (c *CtlClient) parkedWriter() io.Writer {
	if c.parkedOut != nil {
		return c.parkedOut
	}
	return os.Stderr
}

// NewCtlClient creates a client connecting to ctl.sock with token read from stateDir.
func NewCtlClient(stateDir string, timeout time.Duration) (*CtlClient, error) {
	sockPath, _ := CtlPaths(stateDir)
	token, err := ReadCtlToken(stateDir)
	if err != nil {
		return nil, fmt.Errorf("read ctl.token: %w", err)
	}
	if timeout <= 0 {
		timeout = ctlCommandTimeout
	}
	return &CtlClient{
		sockPath: sockPath,
		token:    token,
		timeout:  timeout,
	}, nil
}

// RoundTrip sends cmd and returns the trimmed response body or error.
func (c *CtlClient) RoundTrip(cmd string) (string, error) {
	d := net.Dialer{Timeout: c.timeout}
	conn, err := d.DialContext(context.Background(), "unix", c.sockPath)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrCtlNotListening, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(c.timeout))

	if _, err := fmt.Fprintf(conn, "%s %s\n", cmd, c.token); err != nil {
		return "", err
	}

	resp, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	resp = strings.TrimRight(resp, "\r\n")

	if strings.HasPrefix(resp, "ERR ") {
		return "", errors.New(strings.TrimPrefix(resp, "ERR "))
	}
	if strings.HasPrefix(resp, "OK ") {
		return strings.TrimPrefix(resp, "OK "), nil
	}
	if resp == "OK" {
		return "OK", nil
	}
	return resp, nil
}

// RoundTripWithArg sends "CMD token arg" and returns the response.
func (c *CtlClient) RoundTripWithArg(cmd, arg string) (string, error) {
	return c.RoundTripWithArgTimeout(cmd, arg, c.timeout)
}

// RoundTripWithArgTimeout is RoundTripWithArg with an explicit full
// round-trip deadline (slow-path verbs: MCP-CALL waits for a worker
// exec, which can outlast ctlCommandTimeout).
func (c *CtlClient) RoundTripWithArgTimeout(cmd, arg string, timeout time.Duration) (string, error) {
	if cmd == "MCP-CALL" {
		minDeadline := workerConnDeadline(DefaultAskHoldTimeout, DefaultWorkerExecTimeout)
		if timeout < minDeadline {
			timeout = minDeadline
		}
	}
	d := net.Dialer{Timeout: c.timeout}
	conn, err := d.DialContext(context.Background(), "unix", c.sockPath)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrCtlNotListening, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	if _, err := fmt.Fprintf(conn, "%s %s %s\n", cmd, c.token, arg); err != nil {
		return "", err
	}

	reader := bufio.NewReader(conn)
	resp, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	resp = strings.TrimRight(resp, "\r\n")

	var parkedID string
	if strings.HasPrefix(resp, "PARKED ") {
		parkedID = strings.TrimSpace(strings.TrimPrefix(resp, "PARKED "))
		fmt.Fprintf(c.parkedWriter(), "call parked pending approval (id %s)\n", parkedID)
		_ = conn.SetDeadline(time.Now().Add(timeout))
		resp, err = reader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("approval denied or timed out while parked (id %s)", parkedID)
		}
		resp = strings.TrimRight(resp, "\r\n")
	}

	if strings.HasPrefix(resp, "ERR ") {
		reason := strings.TrimPrefix(resp, "ERR ")
		if parkedID != "" {
			return "", fmt.Errorf("approval %s (id %s)", reason, parkedID)
		}
		return "", errors.New(reason)
	}
	if strings.HasPrefix(resp, "OK ") {
		return strings.TrimPrefix(resp, "OK "), nil
	}
	if resp == "OK" {
		return "OK", nil
	}
	return resp, nil
}

// BindListener sends a BIND-LISTENER command to custosd over ctl.sock.
func (c *CtlClient) BindListener(addr string) error {
	_, err := c.RoundTripWithArg("BIND-LISTENER", addr)
	return err
}

// CloseListener sends a CLOSE-LISTENER command to custosd over ctl.sock.
func (c *CtlClient) CloseListener(addr string) error {
	_, err := c.RoundTripWithArg("CLOSE-LISTENER", addr)
	return err
}

// ListListeners queries the list of bound per-cell proxy listeners.
func (c *CtlClient) ListListeners() ([]string, error) {
	resp, err := c.RoundTrip("LIST-LISTENERS")
	if err != nil {
		return nil, err
	}
	var list []string
	if err := json.Unmarshal([]byte(resp), &list); err != nil {
		return nil, err
	}
	return list, nil
}

// Cards queries pending approval cards from custosd over ctl.sock per CUSTOS-SPEC §6.4a.
func (c *CtlClient) Cards() ([]ApprovalCardWire, error) {
	resp, err := c.RoundTrip("CARDS")
	if err != nil {
		return nil, err
	}
	var cards []ApprovalCardWire
	if err := json.Unmarshal([]byte(resp), &cards); err != nil {
		return nil, fmt.Errorf("unmarshal cards: %w", err)
	}
	return cards, nil
}

// Approve approves an approval card over ctl.sock per CUSTOS-SPEC §6.4a.
func (c *CtlClient) Approve(cardID string) (string, error) {
	return c.RoundTripWithArg("APPROVE", cardID)
}

// Deny denies an approval card over ctl.sock per CUSTOS-SPEC §6.4a.
func (c *CtlClient) Deny(cardID string) (string, error) {
	return c.RoundTripWithArg("DENY", cardID)
}
