package custos

import (
	"context"
	"io"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SetKillPointHook configures the crash-injection seam for V25 tests.
// Available only in test binaries.
func SetKillPointHook(hook func(string)) {
	killPointHook = hook
}

// SetCopyPayloadForTest sets the copyPayload seam for M10 testing.
func SetCopyPayloadForTest(fn func(io.Writer, io.Reader) (int64, error)) {
	if fn == nil {
		copyPayload = io.Copy
	} else {
		copyPayload = fn
	}
}

// MCPConnector test seams (MCP rail). Production mcp_rail.go stays free of
// test-only surface; these compile into the test binary only.

// NotifyChangeForTest triggers the change notification manually in tests.
func (c *MCPConnector) NotifyChangeForTest(toolsChanged bool) {
	c.notifyChange(toolsChanged)
}

// SetStateForTest sets lifecycle state and reason for unit testing.
func (c *MCPConnector) SetStateForTest(state, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = state
	c.reason = reason
}

// SetSnapshotForTest sets the catalog snapshot on a connector for unit testing.
func (c *MCPConnector) SetSnapshotForTest(snap *CatalogSnapshot) {
	c.snapshot.Store(snap)
}

// TriggerRestartForTest simulates a restart event and notifies listeners.
func (c *MCPConnector) TriggerRestartForTest() {
	c.mu.Lock()
	c.restarts = append(c.restarts, time.Now())
	c.mu.Unlock()
	c.notifyChange(false)
}

// NewTestCellWorker builds an unbound cellWorker for direct Dispatch tests
// (the socket is irrelevant when Dispatch is exercised in-process).
func NewTestCellWorker(cellID, token, sockPath string) *cellWorker {
	return &cellWorker{cellID: cellID, token: token, sockPath: sockPath}
}

// ConnDeadlineForTest exposes the per-connection deadline arithmetic.
func (ws *WorkerServer) ConnDeadlineForTest() time.Duration {
	return ws.connDeadline()
}

// LockProxyParkManagerForTest simulates the park-path invariant: it holds
// the proxy park-manager lock (as registerParkFlow does while registering
// a card) and returns the release function.
func LockProxyParkManagerForTest(p *Proxy) func() {
	p.parkMgr.mu.Lock()
	return func() { p.parkMgr.mu.Unlock() }
}

// SetExecTimeoutForTest overrides the worker execution budget in tests.
func (ws *WorkerServer) SetExecTimeoutForTest(d time.Duration) {
	ws.cfg.WorkerExecTimeout = d
}

// RegisterProxyParkForTest registers a parked proxy flow for tests
// (V28 wire-parity/IP scenarios); lives in export_test.go per repo
// convention so production files stay test-seam-free.
func (p *Proxy) RegisterProxyParkForTest(id, cellID, cred, host string, port int, path, review string) {
	if p.parkMgr == nil {
		return
	}
	p.parkMgr.mu.Lock()
	defer p.parkMgr.mu.Unlock()
	p.parkMgr.addParkLocked(&parkedFlow{
		id:        id,
		cellID:    cellID,
		cred:      cred,
		host:      host,
		port:      port,
		path:      path,
		review:    review,
		createdAt: time.Now(),
	})
}

// KeyNameReForTest exposes the frozen keyNameRe regexp for V16l tests.
var KeyNameReForTest = keyNameRe

// ScrubContentBlockForTest exposes scrubContentBlock for V16b tests.
func ScrubContentBlockForTest(block map[string]any, needles []string) map[string]any {
	return scrubContentBlock(block, needles)
}

// ScrubStructuredContentForTest exposes scrubStructuredContent for V16b tests.
func ScrubStructuredContentForTest(v any, needles []string) any {
	return scrubStructuredContent(v, needles)
}

// SetSessionForTest sets the client session on a connector for unit testing.
func (c *MCPConnector) SetSessionForTest(session *mcp.ClientSession) {
	c.session.Store(session)
}

// SetChildProcForTest sets the child process on a connector for unit testing.
func (c *MCPConnector) SetChildProcForTest(proc *ChildProcess) {
	c.childProc.Store(proc)
}

// ChildProcForTest gets the child process on a connector for unit testing.
func (c *MCPConnector) ChildProcForTest() *ChildProcess {
	return c.childProc.Load()
}

// ScheduleDebouncedRefreshForTest triggers debounced refresh coalescing for testing.
func (c *MCPConnector) ScheduleDebouncedRefreshForTest() {
	c.scheduleDebouncedRefresh()
}

// SetNeedlesForTest sets injected needles on a connector for unit testing.
func (c *MCPConnector) SetNeedlesForTest(needles []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.needles = needles
}

// BackoffDurationForTest returns current backoff duration for testing.
func (c *MCPConnector) BackoffDurationForTest() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.backoffDur
}

// TriggerStartLockedForTest triggers startLocked for testing.
func (c *MCPConnector) TriggerStartLockedForTest(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.startLocked(ctx)
}

// AcquireSemaphoreSlotForTest acquires a slot in the connector's semaphore.
func (c *MCPConnector) AcquireSemaphoreSlotForTest() func() {
	c.semaphore <- struct{}{}
	return func() { <-c.semaphore }
}

// TransitionBackoffLockedForTest exercises transitionBackoffLocked for testing.
func (c *MCPConnector) TransitionBackoffLockedForTest(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.transitionBackoffLocked(reason)
}

// SetBackoffDurForTest sets the backoff duration for testing.
func (c *MCPConnector) SetBackoffDurForTest(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.backoffDur = d
}

// SetRestartsForTest sets the restart timestamps for testing.
func (c *MCPConnector) SetRestartsForTest(restarts []time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restarts = restarts
}

// SetInFlightForTest sets inFlight counter for testing.
func (c *MCPConnector) SetInFlightForTest(count int64) {
	c.mu.Lock()
	c.inFlight = count
	c.mu.Unlock()
}

// DrainCondBroadcastForTest signals drainCond for testing.
func (c *MCPConnector) DrainCondBroadcastForTest() {
	c.mu.Lock()
	c.drainCond.Broadcast()
	c.mu.Unlock()
}

// ConsecutiveFailuresForTest returns current consecutive failures for testing.
func (c *MCPConnector) ConsecutiveFailuresForTest() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.consecutiveFailures
}

// HasRetryTimerForTest returns whether retryTimer is scheduled.
func (c *MCPConnector) HasRetryTimerForTest() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.retryTimer != nil
}
