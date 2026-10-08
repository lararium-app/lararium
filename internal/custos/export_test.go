package custos

import "time"

// SetKillPointHook configures the crash-injection seam for V25 tests.
// Available only in test binaries.
func SetKillPointHook(hook func(string)) {
	killPointHook = hook
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
