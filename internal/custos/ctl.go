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

// ApprovalCardWire is the uniform wire shape for pending approval cards per CUSTOS-SPEC §6.4a.
type ApprovalCardWire struct {
	ID         string `json:"id"`
	Cell       string `json:"cell"`
	Cred       string `json:"cred"`
	Dest       string `json:"dest"`
	Tool       string `json:"tool"`
	Review     string `json:"review"`
	AgeS       int    `json:"age_s"`
	ExpiresInS int    `json:"expires_in_s"`
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
	hub       *surface.ApprovalHub
	ln        net.Listener
	done      chan struct{}
	onStop    func()
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
	defer s.mu.Unlock()
	s.workers = w
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

	if s.handleSurrogateCmd(conn, cmd, parts) || s.handlePolicyCmd(conn, cmd, parts) || s.handleListenerCmd(conn, cmd, parts) || s.handleWorkerCmd(conn, cmd, parts) || s.handleApprovalCmd(conn, cmd, parts) {
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
	proxy := s.proxyInstance()
	switch strings.ToUpper(cmd) {
	case "BIND-LISTENER", "BIND_LISTENER":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		if proxy == nil {
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
		if err := proxy.BindListener(addr, cellID, peer, logPath); err != nil {
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
		if proxy == nil {
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
		if err := proxy.CloseListener(addr); err != nil {
			fmt.Fprintf(conn, "ERR %s\n", err.Error())
			return true
		}
		fmt.Fprint(conn, "OK listener_closed\n")
		return true

	case "LIST-LISTENERS", "LIST_LISTENERS":
		if proxy == nil {
			fmt.Fprint(conn, "OK []\n")
			return true
		}
		list := proxy.ListListeners()
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
	workers := s.workersInstance()
	switch strings.ToUpper(cmd) {
	case "BIND-CELL", "BIND_CELL":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		if workers == nil {
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
		if err := workers.BindCell(req.CellID, req.Sock, req.Token); err != nil {
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
		if workers == nil {
			fmt.Fprint(conn, "ERR workers_not_configured\n")
			return true
		}
		cellID := strings.TrimSpace(parts[2])
		if err := workers.UnbindCell(cellID); err != nil {
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
		// Overwriting an existing credential is a rotation: audit it with
		// the frozen credential_rotated kind (§8.1) so a same-name replace
		// is distinguishable from a first store in the chain.
		_, existed := s.vault.GetCredential(req.Name)
		if err := s.vault.StoreOAuthGrant("", req.Name, req.Cred, "cli"); err != nil {
			// Name grammar (CA-1(e)) and the store cap surface as distinct
			// refusals; everything else is a generic store failure.
			switch {
			case errors.Is(err, ErrBadName):
				fmt.Fprint(conn, "ERR bad_name\n")
			case errors.Is(err, ErrFull):
				fmt.Fprint(conn, "ERR store_full\n")
			default:
				fmt.Fprint(conn, "ERR store_failed\n")
			}
			return true
		}
		if existed {
			_ = s.vault.Audit().Append(AuditRecord{
				Kind:  AuditKindCredentialRotated,
				Cred:  req.Name,
				Actor: "cli",
			})
			fmt.Fprint(conn, "OK credential_rotated\n")
			return true
		}
		fmt.Fprint(conn, "OK credential_added\n")
		return true

	default:
		return false
	}
}

// handleApprovalCmd handles standalone approval door verbs: CARDS, APPROVE, DENY per CUSTOS-SPEC §6.4a.
func (s *CtlServer) handleApprovalCmd(conn net.Conn, cmd string, parts []string) bool {
	upper := strings.ToUpper(cmd)
	if upper != "CARDS" && upper != "APPROVE" && upper != "DENY" {
		return false
	}
	if s.vault == nil || !s.vault.IsUnlocked() {
		fmt.Fprint(conn, "ERR locked\n")
		return true
	}
	switch upper {
	case "CARDS":
		s.handleCards(conn)
		return true

	case "APPROVE":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		cardID := strings.TrimSpace(parts[2])
		if cardID == "" {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		s.handleResolve(conn, cardID, true)
		return true

	case "DENY":
		if len(parts) < 3 {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		cardID := strings.TrimSpace(parts[2])
		if cardID == "" {
			fmt.Fprint(conn, "ERR bad_request\n")
			return true
		}
		s.handleResolve(conn, cardID, false)
		return true

	default:
		return false
	}
}

// CardSnapshot produces the uniform ApprovalCardWire list by joining pending hub cards
// with worker and proxy park managers per CUSTOS-SPEC §6.4a, §6.4b.
func CardSnapshot(hub *surface.ApprovalHub, workers *WorkerServer, proxy *Proxy) []ApprovalCardWire {
	if hub == nil {
		return []ApprovalCardWire{}
	}

	pending := hub.Pending()
	if len(pending) == 0 {
		return []ApprovalCardWire{}
	}

	cards := make([]ApprovalCardWire, 0, len(pending))
	for _, card := range pending {
		wire := ApprovalCardWire{
			ID:     card.ID,
			Cell:   card.Cell,
			Cred:   card.Cred,
			Tool:   "",
			Dest:   "",
			Review: "",
		}

		found := false
		if workers != nil && workers.parkMgr != nil {
			workers.parkMgr.mu.Lock()
			for _, cellParks := range workers.parkMgr.byCell {
				if flow, ok := cellParks[card.ID]; ok {
					found = true
					wire.Tool = flow.path
					wire.Dest = ""
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
					break
				}
			}
			workers.parkMgr.mu.Unlock()
		}

		if !found && proxy != nil && proxy.parkMgr != nil {
			proxy.parkMgr.mu.Lock()
			for _, cellParks := range proxy.parkMgr.byCell {
				if flow, ok := cellParks[card.ID]; ok {
					found = true
					if flow.cred != "" {
						wire.Cred = flow.cred
					} else {
						wire.Cred = card.Cred
					}
					wire.Tool = ""
					if flow.port > 0 {
						wire.Dest = fmt.Sprintf("%s:%d%s", flow.host, flow.port, flow.path)
					} else if flow.host != "" {
						wire.Dest = fmt.Sprintf("%s%s", flow.host, flow.path)
					}
					if flow.cellID != "" {
						wire.Cell = flow.cellID
					}
					wire.Review = flow.review
					now := time.Now()
					wire.AgeS = int(now.Sub(flow.createdAt).Seconds())
					if wire.AgeS < 0 {
						wire.AgeS = 0
					}
					wire.ExpiresInS = int(flow.createdAt.Add(proxy.parkMgr.holdTimeout).Sub(now).Seconds())
					if wire.ExpiresInS < 0 {
						wire.ExpiresInS = 0
					}
					break
				}
			}
			proxy.parkMgr.mu.Unlock()
		}

		if !found {
			if (workers == nil || workers.parkMgr == nil) && (proxy == nil || proxy.parkMgr == nil) {
				cards = append(cards, wire)
			}
			continue
		}

		cards = append(cards, wire)
	}
	return cards
}

func (s *CtlServer) handleCards(conn net.Conn) {
	cards := CardSnapshot(s.hubInstance(), s.workersInstance(), s.proxyInstance())
	b, err := json.Marshal(cards) //nolint:gosec // ApprovalCardWire.Cred carries provider/credential name, never secret value
	if err != nil {
		fmt.Fprintf(conn, "ERR %s\n", err.Error())
		return
	}
	fmt.Fprintf(conn, "OK %s\n", string(b))
}

func (s *CtlServer) handleResolve(conn net.Conn, cardID string, allow bool) {
	hub := s.hubInstance()
	if hub == nil {
		fmt.Fprint(conn, "ERR no such card\n")
		return
	}

	var session, cred string
	for _, card := range hub.Pending() {
		if card.ID == cardID {
			session = card.Cell
			cred = card.Cred
			break
		}
	}

	if session == "" {
		if sid, ok := hub.SessionOf(cardID); ok {
			session = sid
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

	status := hub.ResolveFrom(session, cardID, allow, "ctl")
	switch status {
	case 200:
		if allow {
			if s.vault != nil && s.vault.Audit() != nil {
				_ = s.vault.Audit().AppendApprovalAnswered(cred, "ok")
			}
			fmt.Fprint(conn, "OK approved\n")
		} else {
			if s.vault != nil && s.vault.Audit() != nil {
				_ = s.vault.Audit().AppendApprovalAnswered(cred, "denied")
			}
			fmt.Fprint(conn, "OK denied\n")
		}
	case 410:
		fmt.Fprint(conn, "ERR already answered\n")
	case 404:
		fmt.Fprint(conn, "ERR no such card\n")
	default:
		fmt.Fprintf(conn, "ERR %d\n", status)
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
