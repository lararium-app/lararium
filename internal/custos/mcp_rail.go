// MCP connector rail handle and worker lane integration
// per MCP-SPEC §4, §5, §6, §7, §8, §11, and CUSTOS-SPEC §5.1, §5.1a, §8.1.

package custos

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AuditSink appends audit records to the hash-chained audit log.
type AuditSink interface {
	Append(rec AuditRecord) error
}

// VaultGate defines the first-unlock vault interaction seam per
// MCP-SPEC §7 / A7.
type VaultGate interface {
	// IsOccupied checks if the vault already holds a credential named name.
	IsOccupied(name string) (bool, error)
	// EnsureSlot ensures an empty vault slot named name exists once unlocked.
	EnsureSlot(name string) error
}

// FingerprintRevoker handles standing auto-rule revocations when fingerprints
// change.
type FingerprintRevoker interface {
	RevokeAlwaysRule(connector, tool string) error
}

// MCPConnector implements the frozen Connector interface (worker.go:155)
// and ToolLister (A1) for one external MCP server.
type MCPConnector struct {
	cfg        MCPServerConfig
	hearthDir  string
	state      string
	reason     string
	mu         sync.Mutex
	refreshMu  sync.Mutex
	ensured    bool
	inFlight   int64
	drainCond  *sync.Cond
	snapshot   atomic.Pointer[CatalogSnapshot]
	semaphore  chan struct{}
	childProc  atomic.Pointer[ChildProcess]
	session    atomic.Pointer[mcp.ClientSession]
	needles    []string
	restarts   []time.Time
	backoffDur time.Duration
	retryTimer *time.Timer

	// Debounced refresh state per §4.3
	debounceMu sync.Mutex
	debRunning bool
	debPending bool

	// Injected seams & options
	auditSink   AuditSink
	vaultGate   VaultGate
	envResolver func(string) (string, error)
	revoker     FingerprintRevoker
	clock       func() time.Time
	afterFunc   func(time.Duration, func()) *time.Timer
	sigkillWait time.Duration
	execTimeout time.Duration
}

// Option configures an MCPConnector.
type Option func(*MCPConnector)

// WithAuditSink sets the audit record sink.
func WithAuditSink(sink AuditSink) Option {
	return func(c *MCPConnector) {
		c.auditSink = sink
	}
}

// WithVaultGate sets the vault first-unlock interaction seam.
func WithVaultGate(gate VaultGate) Option {
	return func(c *MCPConnector) {
		c.vaultGate = gate
	}
}

// WithEnvResolver sets the resolver for ${vault:X} and ${env:NAME} expressions.
func WithEnvResolver(fn func(string) (string, error)) Option {
	return func(c *MCPConnector) {
		c.envResolver = fn
	}
}

// WithFingerprintRevoker sets the revocation callback for fingerprint changes.
func WithFingerprintRevoker(revoker FingerprintRevoker) Option {
	return func(c *MCPConnector) {
		c.revoker = revoker
	}
}

// WithClock sets a time provider for testing backoff without real sleep.
func WithClock(fn func() time.Time) Option {
	return func(c *MCPConnector) {
		c.clock = fn
	}
}

// WithSigkillGrace sets the SIGTERM grace period before SIGKILL.
func WithSigkillGrace(d time.Duration) Option {
	return func(c *MCPConnector) {
		c.sigkillWait = d
	}
}

// WithExecTimeout sets the worker execution timeout ceiling.
func WithExecTimeout(d time.Duration) Option {
	return func(c *MCPConnector) {
		c.execTimeout = d
	}
}

// WithAfterFunc sets an injectable timer seam for backoff retries.
func WithAfterFunc(fn func(time.Duration, func()) *time.Timer) Option {
	return func(c *MCPConnector) {
		c.afterFunc = fn
	}
}

// NewMCPConnector builds one MCPConnector instance. Initial state is
// starting (§7).
func NewMCPConnector(cfg MCPServerConfig, hearthDir string, opts ...Option) *MCPConnector {
	cfg.normalize()

	conn := &MCPConnector{
		cfg:         cfg,
		hearthDir:   hearthDir,
		state:       MCPStateStarting,
		reason:      MCPReasonOK,
		backoffDur:  time.Second,
		semaphore:   make(chan struct{}, cfg.MaxConcurrentCalls),
		clock:       time.Now,
		afterFunc:   time.AfterFunc,
		sigkillWait: defaultGracePeriod,
		execTimeout: DefaultWorkerExecTimeout,
	}
	conn.drainCond = sync.NewCond(&conn.mu)

	for _, opt := range opts {
		opt(conn)
	}

	// Register empty snapshot initially
	emptySnap := &CatalogSnapshot{
		Connector:    cfg.Name,
		ByName:       make(map[string]*SnapshotTool),
		Fingerprints: make(map[string]string),
		RawToCanon:   make(map[string]string),
		CanonToRaw:   make(map[string]string),
	}
	conn.snapshot.Store(emptySnap)

	return conn
}

// Name implements Connector.Name() (§4.2).
func (c *MCPConnector) Name() string {
	return c.cfg.Name
}

// Credential implements Connector.Credential(): returns the config name
// (A7 identity slot).
func (c *MCPConnector) Credential() string {
	return c.cfg.Name
}

// Host implements Connector.Host() (§4.2): stdio returns connector name;
// http returns URL hostname.
func (c *MCPConnector) Host() string {
	if c.cfg.Transport == "http" {
		if u, err := url.Parse(c.cfg.URL); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	return c.cfg.Name
}

// HasTool implements Connector.HasTool(tool).
func (c *MCPConnector) HasTool(name string) bool {
	snap := c.snapshot.Load()
	return snap.HasTool(name)
}

// ValidateArgs implements Connector.ValidateArgs(tool, args).
func (c *MCPConnector) ValidateArgs(tool string, args json.RawMessage) error {
	snap := c.snapshot.Load()
	st, ok := snap.ByName[tool]
	if !ok {
		return wErr(ErrCodeUnknownTool, detailUnknownTool)
	}
	return ValidateArgs(st.InputSchema, args)
}

// ReviewFields implements Connector.ReviewFields(tool, args).
func (c *MCPConnector) ReviewFields(tool string, args json.RawMessage) string {
	snap := c.snapshot.Load()
	st, ok := snap.ByName[tool]

	destructive := false
	if ok && st.Annotations != nil && st.Annotations.DestructiveHint != nil {
		destructive = *st.Annotations.DestructiveHint
	}

	c.mu.Lock()
	needles := append([]string(nil), c.needles...)
	c.mu.Unlock()

	return BuildReviewLine(args, destructive, needles)
}

// ListTools implements ToolLister.ListTools() (A1).
func (c *MCPConnector) ListTools() []ToolInfo {
	snap := c.snapshot.Load()
	if snap == nil {
		return nil
	}
	return snap.ToolInfos
}

// Execute implements Connector.Execute per MCP-SPEC §4.5, §6, §7, §8.3.
func (c *MCPConnector) Execute(
	ctx context.Context,
	tool string,
	args json.RawMessage,
	cred Credential,
) (json.RawMessage, error) {
	// 1. Execute-entry kill / state check: disabled fails closed immediately
	c.mu.Lock()
	if c.state == MCPStateDisabled {
		c.mu.Unlock()
		return nil, wErr(ErrCodeDenied, detailDeniedCredGone)
	}
	session := c.session.Load()
	snapNeedles := append([]string(nil), c.needles...)
	c.mu.Unlock()

	// 2. Concurrency semaphore bounded by worker execution timeout
	execTimeout := c.execTimeout
	if execTimeout <= 0 {
		execTimeout = DefaultWorkerExecTimeout
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, execTimeout)
	defer waitCancel()

	if waitCtx.Err() != nil {
		return nil, wErr(ErrCodeTimeout, detailTimeout)
	}

	select {
	case c.semaphore <- struct{}{}:
		defer func() { <-c.semaphore }()
	case <-waitCtx.Done():
		return nil, wErr(ErrCodeTimeout, detailTimeout)
	}

	// 3. Pre-dispatch revalidation against then-current snapshot (I11 fail closed)
	snap := c.snapshot.Load()
	st, ok := snap.ByName[tool]
	if !ok {
		return nil, wErr(ErrCodeUnknownTool, detailUnknownTool)
	}
	if err := ValidateArgs(st.InputSchema, args); err != nil {
		return nil, err
	}

	// 4. In-flight needle retention (A2(iii)): call-local needle snapshot
	// held until settle
	atomic.AddInt64(&c.inFlight, 1)
	defer func() {
		atomic.AddInt64(&c.inFlight, -1)
		c.mu.Lock()
		c.drainCond.Broadcast()
		c.mu.Unlock()
	}()

	// 5. Dispatch tools/call to MCP server
	if session == nil {
		return nil, wErr(ErrCodeTransport, detailTransport)
	}

	var argsMap map[string]any
	if len(args) > 0 {
		_ = json.Unmarshal(args, &argsMap)
	}
	if argsMap == nil {
		argsMap = make(map[string]any)
	}

	rawToolName := snap.CanonToRaw[tool]
	if rawToolName == "" {
		rawToolName = tool
	}

	callRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      rawToolName,
		Arguments: argsMap,
	})
	if err != nil {
		if isDeadlineErr(err) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, wErr(ErrCodeTimeout, detailTimeout)
		}
		var rpcErr *jsonrpc.Error
		if errors.As(err, &rpcErr) {
			return nil, wErr(ErrCodeUpstream, DetailProtocolViolation)
		}
		return nil, wErr(ErrCodeTransport, detailTransport)
	}

	// 6. Process CallToolResult through closed parse, scrub, and converge loop
	resBytes, err := json.Marshal(callRes)
	if err != nil {
		return nil, wErr(ErrCodeUpstream, DetailProtocolViolation)
	}

	parsed, blocks, err := ParseClosedResult(resBytes)
	if err != nil {
		return nil, wErr(ErrCodeUpstream, DetailProtocolViolation)
	}

	// Server-initiated interaction / MRTR input_required fails closed per §8.4
	if parsed.ResultType == "input_required" {
		return nil, wErr(ErrCodeUpstream, DetailProtocolViolation)
	}

	// Step 0: scrub before budget measurement
	for i, blk := range blocks {
		blocks[i] = scrubContentBlock(blk, snapNeedles)
	}
	scrubbedSC := scrubStructuredContent(parsed.StructuredContent, snapNeedles)

	convergedData, tokens, err := ConvergeResult(blocks, scrubbedSC, parsed.IsError)
	if err != nil {
		return nil, wErr(ErrCodeInternal, detailInternal)
	}

	// Emit mcp_result_truncated audit if shrinking occurred
	if len(tokens) > 0 && c.auditSink != nil {
		_ = c.auditSink.Append(AuditRecord{
			Kind:  AuditKindMCPResultTruncated,
			Cred:  c.cfg.Name,
			Tool:  c.cfg.Name + "/" + tool,
			Host:  c.Host(),
			Names: tokens,
		})
	}

	return json.RawMessage(convergedData), nil
}
