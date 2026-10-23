// Worker lane per CUSTOS-SPEC §5.1: per-cell unix socket custos.sock,
// NDJSON request/response, cell_token identity, dispatch order
// locked → unknown_connector → unknown_tool → policy (worker lane Decide) →
// ask-card (review fields from tool schema) → execute.
// Error contract §5.1a closed enum + frozen detail templates; every detail
// is scrubbed against every loaded credential field and surrogate (§5.1a).

package custos

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lararium-app/lararium/internal/surface"
)

const (
	workerCellTokenMinLength = 16
	workerMaxLineBytes       = 1 << 20
	// workerConnDeadlineMargin is the slack added on top of the full ask
	// hold plus the execution budget for read/write overhead (§7 hold +
	// per-request execution must both fit inside the conn deadline).
	workerConnDeadlineMargin = 30 * time.Second
	scrubMinLength           = 8 // KEYS-SPEC floor: <8-char values never replaced
	redactedPlaceholder      = "[redacted]"
)

// errLineTooLong marks an NDJSON request line over workerMaxLineBytes.
var errLineTooLong = errors.New("worker request line exceeds cap")

// readCappedLine reads one newline-terminated line, refusing to buffer more
// than workerMaxLineBytes+1 bytes: a peer streaming an endless line can make
// the custosd read side return errLineTooLong, never grow unboundedly.
func readCappedLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		b, err := br.ReadByte()
		if b == '\n' {
			return line, err
		}
		if err != nil {
			return line, err
		}
		line = append(line, b)
		if len(line) > workerMaxLineBytes {
			return nil, errLineTooLong
		}
	}
}

// workerConnDeadline is the per-connection deadline arithmetic: a full ask
// hold plus the execution budget plus the write-back margin. Kept in a
// helper so the value is testable without waiting through a real hold.
func workerConnDeadline(hold, exec time.Duration) time.Duration {
	return hold + exec + workerConnDeadlineMargin
}

// Connector is one worker connector (v1: gmail) exposing typed tools (§5.1).
type Connector interface {
	Name() string
	Credential() string
	Host() string
	HasTool(name string) bool
	// ValidateArgs enforces the tool's typed argument schema (bad ⇒ bad_args).
	ValidateArgs(tool string, args json.RawMessage) error
	// ReviewFields renders the card review line declared by the tool schema
	// (§7: gmail/send declares to and subject).
	ReviewFields(tool string, args json.RawMessage) string
	Execute(ctx context.Context, tool string, args json.RawMessage, cred Credential) (json.RawMessage, error)
}

// WorkerServer serves the per-cell custos.sock worker lane per §5.1.
type WorkerServer struct {
	vault      *Vault
	hub        *surface.ApprovalHub
	cfg        *Config
	parkMgr    *parkManager
	mu         sync.Mutex
	cells      map[string]*cellWorker
	connectors map[string]Connector
	rail       *RailRegistry
	done       chan struct{}
	// cardRefresh re-pushes an enriched CARD frame after a worker
	// park registers (H3.2). Set once at wiring time by the door
	// server; nil until then.
	cardRefresh func(id string)
}

// SetCardRefresh installs the door-side card enricher (H3.2).
func (ws *WorkerServer) SetCardRefresh(fn func(id string)) {
	ws.mu.Lock()
	ws.cardRefresh = fn
	ws.mu.Unlock()
}

func (ws *WorkerServer) refreshCard(id string) {
	ws.mu.Lock()
	fn := ws.cardRefresh
	ws.mu.Unlock()
	if fn != nil && id != "" {
		go fn(id)
	}
}

// WorkerServerOption configures NewWorkerServer.
type WorkerServerOption func(*WorkerServer)

// WithRailRegistry sets an existing RailRegistry on WorkerServer.
func WithRailRegistry(r *RailRegistry) WorkerServerOption {
	return func(ws *WorkerServer) {
		ws.rail = r
	}
}

// cellWorker is one bound cell socket with its stored cell_token (§5.1:
// cell_token binds audit actor to a verified identity).
type cellWorker struct {
	cellID   string
	token    string
	sockPath string
	ln       net.Listener
	wg       sync.WaitGroup
}

// NewWorkerServer creates the worker lane service with the v1 gmail
// connector registered and MCP connectors registered via RailRegistry.
func NewWorkerServer(vault *Vault, hub *surface.ApprovalHub, cfg *Config, opts ...WorkerServerOption) *WorkerServer {
	if cfg == nil {
		cfg = &Config{}
	}
	cfg.Normalize()
	ws := &WorkerServer{
		vault:   vault,
		hub:     hub,
		cfg:     cfg,
		parkMgr: newParkManager(cfg.MaxParkedPerCell, cfg.MaxParkedGlobal, cfg.AskHoldTimeout),
		cells:   make(map[string]*cellWorker),
		done:    make(chan struct{}),
	}
	_ = ws.RegisterConnector(NewGmailConnector(vault))

	for _, opt := range opts {
		opt(ws)
	}

	if ws.rail == nil {
		mcpCfgs := cfg.MCPServers
		if len(mcpCfgs) == 0 {
			mcpCfgs = cfg.Connectors.MCP
		}
		hearthDir := ""
		if vault != nil {
			hearthDir = filepath.Dir(vault.StateDir())
		}
		var railOpts []Option
		if vault != nil {
			railOpts = append(railOpts,
				WithAuditSink(&auditSinkAdapter{logger: vault.Audit()}),
				WithVaultGate(newVaultGateAdapter(vault)),
				WithEnvResolver(newEnvResolver(vault)),
				WithFingerprintRevoker(&policyFingerprintRevoker{policy: vault.Policy()}),
			)
		}
		ws.rail = NewRailRegistry(mcpCfgs, hearthDir, railOpts...)
	}

	for _, conn := range ws.rail.Connectors() {
		_ = ws.RegisterConnector(conn)
	}

	if vault != nil && ws.rail != nil && len(ws.rail.Connectors()) > 0 {
		reg := ws.rail
		vault.SetOnFirstUnlock(func() {
			reg.OnFirstUnlock(context.Background())
		})
	}

	return ws
}

// RailRegistry returns the internal RailRegistry.
func (ws *WorkerServer) RailRegistry() *RailRegistry {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.rail
}

// SetRailRegistry sets the internal RailRegistry.
func (ws *WorkerServer) SetRailRegistry(r *RailRegistry) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.rail = r
}

// RegisterConnector adds (or replaces) a connector by name.
func (ws *WorkerServer) RegisterConnector(c Connector) error {
	if c == nil || c.Name() == "" {
		return errors.New("connector requires a name")
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.connectors == nil {
		ws.connectors = make(map[string]Connector)
	}
	ws.connectors[c.Name()] = c
	return nil
}

// BindCell binds per-cell unix socket custos.sock (created 0600) with the
// cell's stored cell_token. hearthd owns the bind-mount into the cell; here
// the socket lives under the cell's custos dir.
func (ws *WorkerServer) BindCell(cellID, sockPath, token string) error {
	if cellID == "" || sockPath == "" {
		return errors.New("cell id and socket path required")
	}
	if len(token) < workerCellTokenMinLength {
		return errors.New("cell token too short")
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()
	if _, exists := ws.cells[cellID]; exists {
		return fmt.Errorf("cell %q already bound", cellID)
	}

	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return err
	}
	// Stale socket from a previous crash run: replace it.
	if fi, err := os.Lstat(sockPath); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(sockPath)
	}
	lc := net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "unix", sockPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = ln.Close()
		return err
	}

	cw := &cellWorker{cellID: cellID, token: token, sockPath: sockPath, ln: ln}
	ws.cells[cellID] = cw
	cw.wg.Add(1)
	go ws.acceptLoop(cw)
	return nil
}

// UnbindCell closes and unlinks one cell socket.
func (ws *WorkerServer) UnbindCell(cellID string) error {
	ws.mu.Lock()
	cw, ok := ws.cells[cellID]
	if ok {
		delete(ws.cells, cellID)
	}
	ws.mu.Unlock()
	if !ok {
		return fmt.Errorf("cell %q not bound", cellID)
	}
	_ = cw.ln.Close()
	cw.wg.Wait()
	_ = os.Remove(cw.sockPath)
	return nil
}

// Close shuts every cell socket down.
func (ws *WorkerServer) Close() error {
	ws.mu.Lock()
	select {
	case <-ws.done:
	default:
		close(ws.done)
	}
	cells := make([]*cellWorker, 0, len(ws.cells))
	for id, cw := range ws.cells {
		cells = append(cells, cw)
		delete(ws.cells, id)
	}
	ws.mu.Unlock()

	for _, cw := range cells {
		_ = cw.ln.Close()
		cw.wg.Wait()
		_ = os.Remove(cw.sockPath)
	}
	return nil
}

func (ws *WorkerServer) acceptLoop(cw *cellWorker) {
	defer cw.wg.Done()
	for {
		conn, err := cw.ln.Accept()
		if err != nil {
			return
		}
		go ws.handleConn(cw, conn)
	}
}

// handleConn serves NDJSON over one cell socket connection: one request per
// line, one response per line, until EOF. A single request line may not
// exceed workerMaxLineBytes: an oversized line gets the frozen bad_args
// frame and the connection closes (read side stays bounded).
func (ws *WorkerServer) handleConn(cw *cellWorker, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	enc := json.NewEncoder(conn)

	for {
		select {
		case <-ws.done:
			return
		default:
		}
		_ = conn.SetDeadline(time.Now().Add(ws.connDeadline()))

		// Read one line with a hard cap: bytes are pulled one at a time
		// from the buffered reader so the per-request buffer can never
		// exceed workerMaxLineBytes, however long the peer keeps writing.
		line, rerr := readCappedLine(br)
		if errors.Is(rerr, errLineTooLong) {
			// Frozen bad_args frame, then close: a partial-line buffer is
			// never reused for a second request.
			ws.writeBadArgsFrame(conn)
			return
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			if errors.Is(rerr, io.EOF) {
				return // EOF with no pending request: clean disconnect
			}
			continue // blank line between requests: skip, keep serving
		}

		var req WorkerRequest
		if jsonErr := json.Unmarshal(line, &req); jsonErr != nil {
			// Frozen constant frame: written raw so no marshaling can fail
			// on this path (the frame is identical to errResponse's).
			ws.writeBadArgsFrame(conn)
			if errors.Is(rerr, io.EOF) {
				return
			}
			continue
		}

		if encErr := enc.Encode(ws.Dispatch(cw, &req)); encErr != nil {
			return
		}
		if errors.Is(rerr, io.EOF) {
			return
		}
	}
}

// writeBadArgsFrame writes the frozen §5.1a bad_args frame raw (no
// marshaling can fail on this path).
func (ws *WorkerServer) writeBadArgsFrame(conn io.Writer) {
	_, _ = conn.Write([]byte(`{"error_code":"` + ErrCodeBadArgs +
		`","detail":"` + detailBadArgs + `"}` + "\n"))
}

// verifyCellToken constant-time compares the request cell_token against the
// cell's stored token (§5.1: wrong/missing ⇒ denied + worker_call_denied).
func (cw *cellWorker) verifyCellToken(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(cw.token)) == 1
}

// connDeadline is the per-connection deadline for this server's budgets:
// full ask hold + execution budget + margin. The arithmetic itself is the
// contract, asserted by TestWorker_ConnDeadlineArithmetic.
func (ws *WorkerServer) connDeadline() time.Duration {
	return workerConnDeadline(ws.cfg.AskHoldTimeout, ws.cfg.WorkerExecTimeout)
}
