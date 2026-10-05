package custos

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	stateDir  string
	sockPath  string
	tokenPath string
	token     string
	vault     *Vault
	proxy     *Proxy
	workers   *WorkerServer
	ln        net.Listener
	done      chan struct{}
	onStop    func()
}

// SetProxy attaches the custody proxy to the control server.
func (s *CtlServer) SetProxy(p *Proxy) {
	s.proxy = p
}

// SetWorkers attaches the worker lane server to the control server (§5.1).
func (s *CtlServer) SetWorkers(w *WorkerServer) {
	s.workers = w
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

	if s.handleSurrogateCmd(conn, cmd, parts) || s.handlePolicyCmd(conn, cmd, parts) || s.handleListenerCmd(conn, cmd, parts) || s.handleWorkerCmd(conn, cmd, parts) {
		return
	}

	switch cmd {
	case "PING":
		fmt.Fprint(conn, "OK PONG\n")

	case "STATUS":
		failed := 0
		if s.proxy != nil {
			failed = s.proxy.ListenersFailed()
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
		fmt.Fprint(conn, "OK revoked\n")

	default:
		fmt.Fprint(conn, "ERR unknown_command\n")
	}
}

func (s *CtlServer) handleSurrogateCmd(conn net.Conn, cmd string, parts []string) bool {
	switch cmd {
	case "SURROGATE-ADD":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		var req struct {
			Credential  string `json:"credential"`
			Host        string `json:"host"`
			Port        int    `json:"port"`
			PathPrefix  string `json:"path_prefix"`
			AllowBinary bool   `json:"allow_binary"`
		}
		if err := json.Unmarshal([]byte(parts[2]), &req); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		tok, err := s.vault.AddSurrogate("", req.Credential, req.Host, req.Port, req.PathPrefix, req.AllowBinary, "cli")
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprintf(conn, "OK %s\n", tok)
		return true

	case "SURROGATE-LIST":
		list, err := s.vault.ListSurrogates()
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		b, err := json.Marshal(list)
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprintf(conn, "OK %s\n", string(b))
		return true

	case "SURROGATE-REVOKE":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		if err := s.vault.RevokeSurrogate("", parts[2], "cli"); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprint(conn, "OK surrogate_revoked\n")
		return true

	default:
		return false
	}
}

func (s *CtlServer) handlePolicyCmd(conn net.Conn, cmd string, parts []string) bool {
	switch cmd {
	case "POLICY-ADD":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		var req struct {
			Lane    string `json:"lane"`
			Pattern string `json:"pattern"`
			Verdict string `json:"verdict"`
		}
		if err := json.Unmarshal([]byte(parts[2]), &req); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		var notes []string
		var err error
		if req.Lane == "egress" {
			notes, err = s.vault.Policy().AddEgressRule(req.Pattern, req.Verdict, false, "cli", "cli")
		} else {
			notes, err = s.vault.Policy().AddCredentialRule(req.Pattern, req.Verdict, false, "cli", "cli")
		}
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprintf(conn, "OK %s\n", strings.Join(notes, "\x00"))
		return true

	case "POLICY-LIST":
		rows := s.vault.Policy().ListRules()
		b, err := json.Marshal(rows)
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprintf(conn, "OK %s\n", string(b))
		return true

	case "POLICY-RM":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		var lane, pat string
		if strings.Contains(parts[2], "\x00") {
			sub := strings.SplitN(parts[2], "\x00", 2)
			lane, pat = sub[0], sub[1]
		} else {
			pat = parts[2]
		}
		removed, err := s.vault.Policy().RemoveRule(lane, pat, "cli")
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		if !removed {
			fmt.Fprint(conn, "ERR rule not found\n")
			return true
		}
		fmt.Fprint(conn, "OK removed\n")
		return true

	case "POLICY-RESET":
		if err := s.vault.Policy().Reset("cli"); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprint(conn, "OK always-rules cleared\n")
		return true

	case "EGRESS-STRICT":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		strict := parts[2] == "on"
		if err := s.vault.Policy().SetEgressStrict(strict, "cli"); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprintf(conn, "OK egress strict: %s\n", parts[2])
		return true

	default:
		return false
	}
}

func (s *CtlServer) handleListenerCmd(conn net.Conn, cmd string, parts []string) bool {
	switch strings.ToUpper(cmd) {
	case "BIND-LISTENER", "BIND_LISTENER":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		if s.proxy == nil {
			fmt.Fprint(conn, "ERR proxy_not_configured\n")
			return true
		}
		arg := strings.TrimSpace(parts[2])
		var addr, cellID, peer, logPath string
		if strings.HasPrefix(arg, "{") {
			var req struct {
				Addr    string `json:"addr"`
				CellID  string `json:"cell_id"`
				Peer    string `json:"peer"`
				LogPath string `json:"log_path"`
			}
			if err := json.Unmarshal([]byte(arg), &req); err != nil {
				fmt.Fprintf(conn, "ERR %s\n", err.Error())
				return true
			}
			addr, cellID, peer, logPath = req.Addr, req.CellID, req.Peer, req.LogPath
		} else {
			fields := strings.Fields(arg)
			addr = fields[0]
			if len(fields) > 1 {
				cellID = fields[1]
			}
			if len(fields) > 2 {
				peer = fields[2]
			}
			if len(fields) > 3 {
				logPath = fields[3]
			}
		}
		if err := s.proxy.BindListener(addr, cellID, peer, logPath); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprint(conn, "OK listener_bound\n")
		return true

	case "CLOSE-LISTENER", "CLOSE_LISTENER":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		if s.proxy == nil {
			fmt.Fprint(conn, "ERR proxy_not_configured\n")
			return true
		}
		arg := strings.TrimSpace(parts[2])
		var addr string
		if strings.HasPrefix(arg, "{") {
			var req struct {
				Addr string `json:"addr"`
			}
			if err := json.Unmarshal([]byte(arg), &req); err != nil {
				fmt.Fprintf(conn, "ERR %s\n", err.Error())
				return true
			}
			addr = req.Addr
		} else {
			addr = strings.Fields(arg)[0]
		}
		if err := s.proxy.CloseListener(addr); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprint(conn, "OK listener_closed\n")
		return true

	case "LIST-LISTENERS", "LIST_LISTENERS":
		if s.proxy == nil {
			fmt.Fprint(conn, "OK []\n")
			return true
		}
		list := s.proxy.ListListeners()
		b, err := json.Marshal(list)
		if err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprintf(conn, "OK %s\n", string(b))
		return true

	default:
		return false
	}
}

// handleWorkerCmd serves the slice-4 worker-lane ctl ops (§5.1, §4.5):
// BIND-CELL binds a per-cell custos.sock; UNBIND-CELL releases it;
// LOGIN-STORE stores an exchanged oauth2 grant through the Mutate path.
func (s *CtlServer) handleWorkerCmd(conn net.Conn, cmd string, parts []string) bool {
	switch strings.ToUpper(cmd) {
	case "BIND-CELL", "BIND_CELL":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		if s.workers == nil {
			fmt.Fprint(conn, "ERR workers_not_configured\n")
			return true
		}
		var req struct {
			CellID string `json:"cell_id"`
			Sock   string `json:"sock"`
			Token  string `json:"token"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(parts[2])), &req); err != nil {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		if err := s.workers.BindCell(req.CellID, req.Sock, req.Token); err != nil {
			fmt.Fprint(conn, "ERR bind_failed\n")
			return true
		}
		fmt.Fprint(conn, "OK cell_bound\n")
		return true

	case "UNBIND-CELL", "UNBIND_CELL":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		if s.workers == nil {
			fmt.Fprint(conn, "ERR workers_not_configured\n")
			return true
		}
		cellID := strings.TrimSpace(parts[2])
		if err := s.workers.UnbindCell(cellID); err != nil {
			fmt.Fprint(conn, "ERR unbind_failed\n")
			return true
		}
		fmt.Fprint(conn, "OK cell_unbound\n")
		return true

	case "LOGIN-STORE", "LOGIN_STORE":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		var req struct {
			Name string     `json:"name"`
			Cred Credential `json:"cred"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(parts[2])), &req); err != nil || req.Name == "" {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		if err := s.vault.StoreOAuthGrant("", req.Name, req.Cred, "cli"); err != nil {
			fmt.Fprint(conn, "ERR store_failed\n")
			return true
		}
		fmt.Fprint(conn, "OK credential_added\n")
		return true

	default:
		return false
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
	sockPath string
	token    string
	timeout  time.Duration
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
	d := net.Dialer{Timeout: c.timeout}
	conn, err := d.DialContext(context.Background(), "unix", c.sockPath)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrCtlNotListening, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(c.timeout))

	if _, err := fmt.Fprintf(conn, "%s %s %s\n", cmd, c.token, arg); err != nil {
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
