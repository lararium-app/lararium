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

// SetExecTimeoutForTest overrides the worker execution budget in tests.
func (ws *WorkerServer) SetExecTimeoutForTest(d time.Duration) {
	ws.cfg.WorkerExecTimeout = d
}
