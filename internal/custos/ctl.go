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
	ln        net.Listener
	done      chan struct{}
	onStop    func()
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

	switch cmd {
	case "PING":
		fmt.Fprint(conn, "OK PONG\n")

	case "STATUS":
		st := s.vault.Status(true, 0)
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
