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
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lararium-app/lararium/internal/surface"
)

// Frozen worker-lane error codes per CUSTOS-SPEC §5.1a (closed enum).
const (
	ErrCodeDenied           = "denied"
	ErrCodeLocked           = "locked"
	ErrCodeUnknownConnector = "unknown_connector"
	ErrCodeUnknownTool      = "unknown_tool"
	ErrCodeBadArgs          = "bad_args"
	ErrCodeUpstream         = "upstream"
	ErrCodeRateLimited      = "rate_limited"
	ErrCodeTimeout          = "timeout"
	ErrCodeTransport        = "transport"
	ErrCodeInternal         = "internal"
)

const (
	workerCellTokenMinLength = 16
	workerMaxLineBytes       = 1 << 20
	workerRequestTimeout     = 5 * time.Minute
	scrubMinLength           = 8 // KEYS-SPEC floor: <8-char values never replaced
	redactedPlaceholder      = "[redacted]"
)

// Frozen detail templates per §5.1a: closed set, no URL, no upstream body,
// no wrapped error chain.
const (
	detailDeniedToken      = "denied: invalid cell token"
	detailDeniedPolicy     = "denied: policy"
	detailDeniedApproval   = "denied: approval"
	detailDeniedRevoked    = "denied: revoked"
	detailDeniedCredGone   = "denied: credential unavailable" //nolint:gosec // frozen detail template, not a secret
	detailLocked           = "custody locked"
	detailUnknownConnector = "unknown connector"
	detailUnknownTool      = "unknown tool"
	detailBadArgs          = "invalid arguments"
	detailUpstream5xx      = "upstream returned 5xx"
	detailUpstream4xx      = "upstream returned 4xx"
	detailRateLimited      = "upstream rate limited"
	detailTimeout          = "operation deadline exceeded"
	detailTransport        = "connection failed before response"
	detailInternal         = "internal error"
)

// ValidWorkerErrorCode reports membership in the frozen §5.1a enum.
func ValidWorkerErrorCode(code string) bool {
	switch code {
	case ErrCodeDenied, ErrCodeLocked, ErrCodeUnknownConnector, ErrCodeUnknownTool,
		ErrCodeBadArgs, ErrCodeUpstream, ErrCodeRateLimited, ErrCodeTimeout,
		ErrCodeTransport, ErrCodeInternal:
		return true
	default:
		return false
	}
}

// workerError carries a §5.1a code with a template-frozen detail. Its error
// text never contains a URL, an upstream body, or a wrapped cause chain.
type workerError struct {
	code   string
	detail string
}

func (e *workerError) Error() string { return e.code + ": " + e.detail }

func wErr(code, detail string) *workerError { return &workerError{code: code, detail: detail} }

// WorkerRequest is one NDJSON worker request per §5.1.
type WorkerRequest struct {
	Op        string          `json:"op"`
	Connector string          `json:"connector"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	CellToken string          `json:"cell_token"`
}

// WorkerResponse is one NDJSON worker response per §5.1:
// {"ok":true,"result":…} or {"error_code":"…","detail":"…"}.
type WorkerResponse struct {
	OK        bool            `json:"ok,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	ErrorCode string          `json:"error_code,omitempty"`
	Detail    string          `json:"detail,omitempty"`
}

func okResponse(result json.RawMessage) WorkerResponse {
	return WorkerResponse{OK: true, Result: result}
}

func errResponse(code, detail string) WorkerResponse {
	return WorkerResponse{ErrorCode: code, Detail: detail}
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
	done       chan struct{}
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
// connector registered.
func NewWorkerServer(vault *Vault, hub *surface.ApprovalHub, cfg *Config) *WorkerServer {
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
	return ws
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
// line, one response per line, until EOF.
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
		_ = conn.SetDeadline(time.Now().Add(workerRequestTimeout))

		line, rerr := br.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) == 0 {
			return
		}

		var req WorkerRequest
		if jsonErr := json.Unmarshal(line, &req); jsonErr != nil {
			// Frozen constant frame: written raw so no marshaling can fail
			// on this path (the frame is identical to errResponse's).
			_, _ = conn.Write([]byte(`{"error_code":"` + ErrCodeBadArgs +
				`","detail":"` + detailBadArgs + `"}` + "\n"))
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

// verifyCellToken constant-time compares the request cell_token against the
// cell's stored token (§5.1: wrong/missing ⇒ denied + worker_call_denied).
func (cw *cellWorker) verifyCellToken(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(cw.token)) == 1
}

// Dispatch runs the §5.1 order: locked → unknown_connector → unknown_tool →
// policy worker-lane Decide → ask card (review fields) → execute.
// Every response detail passes the §5.1a whole-set scrub.
func (ws *WorkerServer) Dispatch(cw *cellWorker, req *WorkerRequest) (resp WorkerResponse) {
	host := ""
	connector := ""
	tool := ""

	defer func() {
		if resp.Detail != "" {
			resp.Detail = ws.ScrubDetail(resp.Detail)
		}
	}()

	// Protocol gate: op must be "call".
	if req.Op != "call" {
		resp = errResponse(ErrCodeBadArgs, detailBadArgs)
		ws.auditDenied(cw, connector, host, tool, "bad_op")
		return resp
	}

	// cell_token identity (§5.1).
	if !cw.verifyCellToken(req.CellToken) {
		resp = errResponse(ErrCodeDenied, detailDeniedToken)
		ws.auditDenied(cw, connector, host, tool, "cell_token")
		return resp
	}

	// 1. custody locked — only when the vault exists and is actually
	// locked; a nil vault is a server wiring fault, never "locked"
	// (a cell must never be told custody is sealed when none is
	// loaded). A nil vault has no audit logger; the denial response
	// itself is the signal.
	if ws.vault == nil {
		return errResponse(ErrCodeInternal, detailInternal)
	}
	if !ws.vault.IsUnlocked() {
		resp = errResponse(ErrCodeLocked, detailLocked)
		ws.auditDenied(cw, connector, host, tool, "locked")
		return resp
	}

	// 2. unknown connector.
	conn, known := ws.connectorByName(req.Connector)
	if !known {
		resp = errResponse(ErrCodeUnknownConnector, detailUnknownConnector)
		ws.auditDenied(cw, req.Connector, host, tool, "unknown_connector")
		return resp
	}
	connector = conn.Name()

	// 3. unknown tool.
	if !conn.HasTool(req.Tool) {
		resp = errResponse(ErrCodeUnknownTool, detailUnknownTool)
		ws.auditDenied(cw, connector, host, req.Tool, "unknown_tool")
		return resp
	}
	tool = req.Tool
	host = conn.Host()

	// 4. typed args schema (bad shape ⇒ bad_args).
	if err := conn.ValidateArgs(tool, req.Args); err != nil {
		resp = errResponse(ErrCodeBadArgs, detailBadArgs)
		ws.auditDenied(cw, connector, host, tool, "bad_args")
		return resp
	}

	// 5. policy worker-lane Decide (§6.1/§6.2 tool-qualified patterns).
	verdict, pattern := ws.vault.Policy().Decide(DecideInput{
		Lane:       "worker",
		Credential: conn.Credential(),
		Tool:       tool,
		Host:       host,
	})

	switch verdict {
	case VerdictDeny:
		resp = errResponse(ErrCodeDenied, detailDeniedPolicy)
		ws.auditDenied(cw, connector, host, tool, "policy:"+pattern)
		return resp

	case VerdictAsk:
		if ok, r := ws.askCard(cw, conn, tool, req.Args); !ok {
			resp = r
			return resp
		}
		// §7 doctrine (mirror of bearer policy-during-park): the engine is
		// re-consulted after the hold settles — a policy rm/flip committed
		// during the hold kills the flow before any upstream byte.
		// §6.2 doctrine: if the policy flipped to deny while the card
		// was held (via any surface), the approval never executes.
		// An unchanged ask is the very thing the human just answered.
		if v2, _ := ws.vault.Policy().Decide(DecideInput{
			Lane:       "worker",
			Credential: conn.Credential(),
			Tool:       tool,
			Host:       host,
		}); v2 == VerdictDeny {
			resp = errResponse(ErrCodeDenied, detailDeniedPolicy)
			ws.auditDenied(cw, connector, host, tool, "policy_changed_during_hold")
			return resp
		}

	case VerdictAuto:
		// fall through to execution

	default:
		// Unknown verdict is impossible from the engine; refuse closed.
		resp = errResponse(ErrCodeDenied, detailDeniedPolicy)
		ws.auditDenied(cw, connector, host, tool, "verdict")
		return resp
	}

	// Execution-time re-validation (§7 doctrine: policy rm / revoke during a
	// hold kills the flow at the dial; the credential must still exist).
	cred, ok := ws.vault.GetCredential(conn.Credential())
	if !ok {
		resp = errResponse(ErrCodeDenied, detailDeniedCredGone)
		ws.auditDenied(cw, connector, host, tool, "credential_gone")
		return resp
	}

	ctx, cancel := context.WithTimeout(context.Background(), ws.cfg.AskHoldTimeout)
	defer cancel()

	result, execErr := conn.Execute(ctx, tool, req.Args, cred)
	if execErr != nil {
		we := classifyWorkerError(execErr)
		resp = errResponse(we.code, we.detail)
		ws.auditDenied(cw, connector, host, tool, we.code)
		return resp
	}

	ws.auditAllowed(cw, connector, host, tool)
	return okResponse(result)
}

// connectorByName looks a connector up under the dispatch lock.
func (ws *WorkerServer) connectorByName(name string) (Connector, bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	c, ok := ws.connectors[name]
	return c, ok
}

// askCard parks the worker call behind an approval card carrying the tool
// schema's review fields (§7 card contents; hold/shed/timeout mechanics
// identical to bearer parking via the shared park manager).
func (ws *WorkerServer) askCard(cw *cellWorker, conn Connector, tool string, args json.RawMessage) (bool, WorkerResponse) {
	if ws.hub == nil {
		return false, errResponse(ErrCodeDenied, detailDeniedPolicy)
	}

	pm := ws.parkMgr
	pm.mu.Lock()
	if len(pm.byCell[cw.cellID]) >= pm.maxPerCell {
		pm.mu.Unlock()
		return false, errResponse(ErrCodeDenied, detailDeniedApproval)
	}
	var shedFlow *parkedFlow
	if pm.totalParked >= pm.maxGlobal {
		shedFlow = pm.findBiggestCellParkLocked()
		if shedFlow != nil {
			pm.removeParkLocked(shedFlow)
		}
	}
	flow := &parkedFlow{
		cellID:    cw.cellID,
		cred:      conn.Credential(),
		host:      conn.Host(),
		path:      tool,
		outcomeCh: make(chan string, 1),
		createdAt: time.Now(),
	}
	pm.addParkLocked(flow)
	pm.mu.Unlock()

	if shedFlow != nil {
		select {
		case shedFlow.outcomeCh <- "too_many_asks":
		default:
		}
	}

	review := conn.ReviewFields(tool, args)
	dest := conn.Host() + ":" + tool
	id, decisionCh := ws.hub.RegisterCustos(cw.cellID, conn.Credential(), dest, review, ws.cfg.AskHoldTimeout, nil)
	flow.id = id

	timer := time.NewTimer(ws.cfg.AskHoldTimeout)
	defer timer.Stop()

	settle := func() {
		pm.mu.Lock()
		pm.removeParkLocked(flow)
		pm.mu.Unlock()
	}

	select {
	case <-flow.outcomeCh:
		// Evicted by global shed.
		if flow.cancelled.CompareAndSwap(false, true) {
			ws.hub.CancelCard(flow.id, "too_many_asks", "custos")
		}
		settle()
		return false, errResponse(ErrCodeDenied, detailDeniedApproval)

	case <-timer.C:
		if flow.cancelled.CompareAndSwap(false, true) {
			settle()
			ws.hub.CancelCard(flow.id, "timeout", "timer")
		}
		return false, errResponse(ErrCodeDenied, detailDeniedApproval)

	case <-ws.done:
		if flow.cancelled.CompareAndSwap(false, true) {
			settle()
			ws.hub.CancelCard(flow.id, "timeout", "server_closed")
		}
		return false, errResponse(ErrCodeLocked, detailLocked)

	case approved := <-decisionCh:
		settle()
		if !approved {
			reason := ws.hub.Reason(cw.cellID, id)
			switch reason {
			case "custos_locked":
				return false, errResponse(ErrCodeLocked, detailLocked)
			case "credential_revoked":
				return false, errResponse(ErrCodeDenied, detailDeniedRevoked)
			default:
				return false, errResponse(ErrCodeDenied, detailDeniedApproval)
			}
		}
		return true, WorkerResponse{}
	}
}

// auditAllowed appends worker_call_allowed (§8.1) with the verified actor.
func (ws *WorkerServer) auditAllowed(cw *cellWorker, cred, host, tool string) {
	_ = ws.vault.Audit().Append(AuditRecord{
		Kind:    AuditKindWorkerCallAllowed,
		Cred:    cred,
		Actor:   cw.cellID,
		Host:    host,
		Tool:    tool,
		Verdict: "allow",
	})
}

// auditDenied appends worker_call_denied (§8.1) with the verified actor.
func (ws *WorkerServer) auditDenied(cw *cellWorker, cred, host, tool, reason string) {
	_ = ws.vault.Audit().Append(AuditRecord{
		Kind:    AuditKindWorkerCallDenied,
		Cred:    cred,
		Actor:   cw.cellID,
		Host:    host,
		Tool:    tool,
		Verdict: "deny",
		Reason:  reason,
	})
}

// classifyWorkerError maps execution failures onto the frozen §5.1a enum.
// Output is code + frozen template only: never a URL, upstream body, or
// wrapped chain (§5.1a).
func classifyWorkerError(err error) *workerError {
	var we *workerError
	if errors.As(err, &we) {
		return we
	}
	if errors.Is(err, ErrCredentialGone) {
		return wErr(ErrCodeDenied, detailDeniedCredGone)
	}
	if isDeadlineErr(err) {
		return wErr(ErrCodeTimeout, detailTimeout)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		// DNS / refused / no-status: connection failed before a response.
		return wErr(ErrCodeTransport, detailTransport)
	}
	// Bare resolver/socket failures (connector dials outside http.Client
	// wrappers too): any net.Error — DNSError, OpError — is transport.
	var ne net.Error
	if errors.As(err, &ne) {
		return wErr(ErrCodeTransport, detailTransport)
	}
	return wErr(ErrCodeInternal, detailInternal)
}

// V13TestError is the exported view of a classified worker error for the
// doctrine tests (classification itself stays package-internal).
type V13TestError struct {
	Code   string
	Detail string
}

// ClassifyWorkerErrorForTest exposes classifyWorkerError to the V-suite.
func ClassifyWorkerErrorForTest(err error) V13TestError {
	we := classifyWorkerError(err)
	return V13TestError{Code: we.code, Detail: we.detail}
}

// ScrubDetail applies the §5.1a whole-set scrub: every loaded credential
// field individually (api_key secret; oauth2 client_secret, refresh_token,
// access_token separately) and every loaded surrogate token, replaced
// left-to-right non-overlapping with [redacted]; values < 8 chars are never
// replaced (KEYS-SPEC floor).
func (ws *WorkerServer) ScrubDetail(detail string) string {
	pairs := ws.scrubNeedles()
	if len(pairs) == 0 {
		return detail
	}
	return strings.NewReplacer(pairs...).Replace(detail)
}

// scrubNeedles collects replacement pairs from the loaded vault state:
// every credential field individually, every surrogate token.
func (ws *WorkerServer) scrubNeedles() []string {
	var vals []string
	add := func(v string) {
		if len(v) >= scrubMinLength {
			vals = append(vals, v)
		}
	}

	for _, c := range ws.vault.LoadedCredentials() {
		add(c.Secret)
		add(c.ClientSecret)
		add(c.RefreshToken)
		add(c.AccessToken)
	}
	if ws.vault.Surrogates() != nil {
		for _, s := range ws.vault.Surrogates().List() {
			add(s.Token)
		}
	}

	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	pairs := make([]string, 0, 2*len(vals))
	for _, v := range vals {
		pairs = append(pairs, v, redactedPlaceholder)
	}
	return pairs
}
