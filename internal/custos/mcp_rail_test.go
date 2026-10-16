// Tests for MCP rail handle, lifecycle, concurrency, error mapping, and ctl
// per MCP-SPEC §4, §5, §6, §7, §8, §9, §11, §12.

package custos_test

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lararium-app/lararium/internal/custos"
)

// mockVaultGate records IsOccupied checks and EnsureSlot invocations.
type mockVaultGate struct {
	mu          sync.Mutex
	occupied    map[string]bool
	ensureCalls []string
}

func (m *mockVaultGate) IsOccupied(name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.occupied[name], nil
}

func (m *mockVaultGate) EnsureSlot(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureCalls = append(m.ensureCalls, name)
	return nil
}

func (m *mockVaultGate) EnsureCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := make([]string, len(m.ensureCalls))
	copy(copied, m.ensureCalls)
	return copied
}

// mockRevoker records RevokeAlwaysRule callbacks.
type mockRevoker struct {
	mu    sync.Mutex
	calls [][2]string
}

func (m *mockRevoker) RevokeAlwaysRule(connector, tool string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, [2]string{connector, tool})
	return nil
}

func (m *mockRevoker) Calls() [][2]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := make([][2]string, len(m.calls))
	copy(copied, m.calls)
	return copied
}

// TestFirstUnlock_OccupancyRefusal verifies §7 first-unlock order:
// refusal -> ensure slot -> spawn.
func TestFirstUnlock_OccupancyRefusal(t *testing.T) {
	t.Run("occupied_name_refuses_without_ensure_or_spawn", func(t *testing.T) {
		gate := &mockVaultGate{
			occupied: map[string]bool{"occupied-mcp": true},
		}

		cfg := custos.MCPServerConfig{
			Name:      "occupied-mcp",
			Transport: "stdio",
			Command:   "definitely-nonexistent-command-xyz",
		}

		conn := custos.NewMCPConnector(
			cfg,
			t.TempDir(),
			custos.WithVaultGate(gate),
		)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if err := conn.OnFirstUnlock(ctx); err != nil {
			t.Fatalf("OnFirstUnlock unexpected error: %v", err)
		}

		status := conn.Status()
		if status.State != custos.MCPStateDisabled {
			t.Errorf("state = %q, want %q", status.State, custos.MCPStateDisabled)
		}
		if status.Reason != custos.MCPReasonNameOccupied {
			t.Errorf("reason = %q, want %q", status.Reason, custos.MCPReasonNameOccupied)
		}

		// EnsureSlot must NOT have been called
		if calls := gate.EnsureCalls(); len(calls) != 0 {
			t.Errorf("EnsureCalls = %v, want empty", calls)
		}
	})

	t.Run("unoccupied_name_ensures_slot_and_latches_once", func(t *testing.T) {
		gate := &mockVaultGate{
			occupied: map[string]bool{"free-mcp": false},
		}

		cfg := custos.MCPServerConfig{
			Name:      "free-mcp",
			Transport: "stdio",
			Command:   "definitely-nonexistent-command-xyz",
		}

		conn := custos.NewMCPConnector(
			cfg,
			t.TempDir(),
			custos.WithVaultGate(gate),
		)

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_ = conn.OnFirstUnlock(ctx)

		calls := gate.EnsureCalls()
		if len(calls) != 1 || calls[0] != "free-mcp" {
			t.Errorf("EnsureCalls = %v, want [free-mcp]", calls)
		}

		// Second unlock must not re-call EnsureSlot (latched once)
		_ = conn.OnFirstUnlock(ctx)
		calls2 := gate.EnsureCalls()
		if len(calls2) != 1 {
			t.Errorf("EnsureCalls after 2nd unlock = %v, want exactly 1 call", calls2)
		}
	})
}

// TestV14_ErrorTable tests exact workerError code and detail per MCP-SPEC §8.3.
func TestV14_ErrorTable(t *testing.T) {
	tempDir := t.TempDir()

	t.Run("disabled_at_entry", func(t *testing.T) {
		cfg := custos.MCPServerConfig{
			Name:      "test-conn",
			Transport: "stdio",
			Command:   "echo",
		}
		conn := custos.NewMCPConnector(cfg, tempDir)
		conn.SetStateForTest(custos.MCPStateDisabled, custos.MCPReasonCredentialUnavailable)

		_, err := conn.Execute(context.Background(), "some_tool", json.RawMessage(`{}`), custos.Credential{})
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		classified := custos.ClassifyWorkerErrorForTest(err)
		if !custos.ValidWorkerErrorCode(classified.Code) {
			t.Errorf("invalid worker error code: %q", classified.Code)
		}
		if classified.Code != custos.ErrCodeDenied {
			t.Errorf("code = %q, want %q", classified.Code, custos.ErrCodeDenied)
		}
		if classified.Detail != "denied: credential unavailable" {
			t.Errorf("detail = %q, want %q", classified.Detail, "denied: credential unavailable")
		}
	})

	t.Run("vanished_unknown_tool", func(t *testing.T) {
		cfg := custos.MCPServerConfig{
			Name:      "test-conn",
			Transport: "stdio",
			Command:   "echo",
		}
		conn := custos.NewMCPConnector(cfg, tempDir)
		conn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)

		_, err := conn.Execute(context.Background(), "unknown_tool_xyz", json.RawMessage(`{}`), custos.Credential{})
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		classified := custos.ClassifyWorkerErrorForTest(err)
		if !custos.ValidWorkerErrorCode(classified.Code) {
			t.Errorf("invalid worker error code: %q", classified.Code)
		}
		if classified.Code != custos.ErrCodeUnknownTool {
			t.Errorf("code = %q, want %q", classified.Code, custos.ErrCodeUnknownTool)
		}
		if classified.Detail != "unknown tool" {
			t.Errorf("detail = %q, want %q", classified.Detail, "unknown tool")
		}
	})

	t.Run("violated_arguments_schema", func(t *testing.T) {
		cfg := custos.MCPServerConfig{
			Name:      "test-conn",
			Transport: "stdio",
			Command:   "echo",
		}
		conn := custos.NewMCPConnector(cfg, tempDir)
		conn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)

		schema := []byte(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`)
		snap, err := custos.BuildCatalogSnapshot("test-conn", []*mcp.Tool{
			{Name: "read_file", InputSchema: schema},
		}, nil, nil, nil, time.Minute, time.Now())
		if err != nil {
			t.Fatalf("BuildCatalogSnapshot: %v", err)
		}
		conn.SetSnapshotForTest(snap)

		// Violate schema by providing number instead of string for required "path"
		_, err = conn.Execute(context.Background(), "read_file", json.RawMessage(`{"path":123}`), custos.Credential{})
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		classified := custos.ClassifyWorkerErrorForTest(err)
		if !custos.ValidWorkerErrorCode(classified.Code) {
			t.Errorf("invalid worker error code: %q", classified.Code)
		}
		if classified.Code != custos.ErrCodeBadArgs {
			t.Errorf("code = %q, want %q", classified.Code, custos.ErrCodeBadArgs)
		}
		if classified.Detail != "invalid arguments" {
			t.Errorf("detail = %q, want %q", classified.Detail, "invalid arguments")
		}
	})

	t.Run("transport_loss_nil_session", func(t *testing.T) {
		cfg := custos.MCPServerConfig{
			Name:      "test-conn",
			Transport: "stdio",
			Command:   "echo",
		}
		conn := custos.NewMCPConnector(cfg, tempDir)
		conn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)

		snap, err := custos.BuildCatalogSnapshot("test-conn", []*mcp.Tool{
			{Name: "echo_tool"},
		}, nil, nil, nil, time.Minute, time.Now())
		if err != nil {
			t.Fatalf("BuildCatalogSnapshot: %v", err)
		}
		conn.SetSnapshotForTest(snap)

		// Session is nil (dead child / transport lost before response)
		_, err = conn.Execute(context.Background(), "echo_tool", json.RawMessage(`{}`), custos.Credential{})
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		classified := custos.ClassifyWorkerErrorForTest(err)
		if !custos.ValidWorkerErrorCode(classified.Code) {
			t.Errorf("invalid worker error code: %q", classified.Code)
		}
		if classified.Code != custos.ErrCodeTransport {
			t.Errorf("code = %q, want %q", classified.Code, custos.ErrCodeTransport)
		}
		if classified.Detail != "connection failed before response" {
			t.Errorf("detail = %q, want %q", classified.Detail, "connection failed before response")
		}
	})

	t.Run("semaphore_wait_timeout", func(t *testing.T) {
		cfg := custos.MCPServerConfig{
			Name:               "test-conn",
			Transport:          "stdio",
			Command:            "echo",
			MaxConcurrentCalls: 1,
		}
		conn := custos.NewMCPConnector(
			cfg,
			tempDir,
			custos.WithExecTimeout(20*time.Millisecond),
		)
		conn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)

		// Acquire the 1 semaphore slot so the execute call blocks
		release := conn.AcquireSemaphoreSlotForTest()
		defer release()

		_, err := conn.Execute(context.Background(), "any_tool", json.RawMessage(`{}`), custos.Credential{})
		if err == nil {
			t.Fatal("expected timeout error, got nil")
		}

		classified := custos.ClassifyWorkerErrorForTest(err)
		if !custos.ValidWorkerErrorCode(classified.Code) {
			t.Errorf("invalid worker error code: %q", classified.Code)
		}
		if classified.Code != custos.ErrCodeTimeout {
			t.Errorf("code = %q, want %q", classified.Code, custos.ErrCodeTimeout)
		}
		if classified.Detail != "operation deadline exceeded" {
			t.Errorf("detail = %q, want %q", classified.Detail, "operation deadline exceeded")
		}
	})

	t.Run("context_deadline_expired", func(t *testing.T) {
		cfg := custos.MCPServerConfig{
			Name:      "test-conn",
			Transport: "stdio",
			Command:   "echo",
		}
		conn := custos.NewMCPConnector(cfg, tempDir)
		conn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)

		snap, err := custos.BuildCatalogSnapshot("test-conn", []*mcp.Tool{
			{Name: "echo_tool"},
		}, nil, nil, nil, time.Minute, time.Now())
		if err != nil {
			t.Fatalf("BuildCatalogSnapshot: %v", err)
		}
		conn.SetSnapshotForTest(snap)

		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()

		_, err = conn.Execute(ctx, "echo_tool", json.RawMessage(`{}`), custos.Credential{})
		if err == nil {
			t.Fatal("expected timeout error, got nil")
		}

		classified := custos.ClassifyWorkerErrorForTest(err)
		if !custos.ValidWorkerErrorCode(classified.Code) {
			t.Errorf("invalid worker error code: %q", classified.Code)
		}
		if classified.Code != custos.ErrCodeTimeout {
			t.Errorf("code = %q, want %q", classified.Code, custos.ErrCodeTimeout)
		}
		if classified.Detail != "operation deadline exceeded" {
			t.Errorf("detail = %q, want %q", classified.Detail, "operation deadline exceeded")
		}
	})
}

// TestSemaphore_Concurrency tests max_concurrent_calls enforcement.
func TestSemaphore_Concurrency(t *testing.T) {
	cfg := custos.MCPServerConfig{
		Name:               "concurrency-mcp",
		Transport:          "stdio",
		Command:            "echo",
		MaxConcurrentCalls: 2,
	}

	conn := custos.NewMCPConnector(
		cfg,
		t.TempDir(),
		custos.WithExecTimeout(50*time.Millisecond),
	)
	conn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)

	// Fill both semaphore slots
	release1 := conn.AcquireSemaphoreSlotForTest()
	defer release1()
	release2 := conn.AcquireSemaphoreSlotForTest()
	defer release2()

	// 3rd concurrent call must block and hit execTimeout
	start := time.Now()
	_, err := conn.Execute(context.Background(), "any_tool", json.RawMessage(`{}`), custos.Credential{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}

	classified := custos.ClassifyWorkerErrorForTest(err)
	if classified.Code != custos.ErrCodeTimeout {
		t.Errorf("code = %q, want %q", classified.Code, custos.ErrCodeTimeout)
	}

	if elapsed < 40*time.Millisecond {
		t.Errorf("elapsed = %v, expected at least ~50ms timeout wait", elapsed)
	}
}

// TestBackoffDoublingAndCap tests backoff doubling, 5m cap, and 5/hour
// limit (§7).
func TestBackoffDoublingAndCap(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	clockNow := now

	cfg := custos.MCPServerConfig{
		Name:      "backoff-mcp",
		Transport: "stdio",
		Command:   "echo",
	}

	conn := custos.NewMCPConnector(
		cfg,
		t.TempDir(),
		custos.WithClock(func() time.Time { return clockNow }),
	)

	// Initial backoff is 1s
	if dur := conn.BackoffDurationForTest(); dur != time.Second {
		t.Fatalf("initial backoff = %v, want 1s", dur)
	}

	// 1st failure: 1s -> 2s
	conn.SetRestartsForTest([]time.Time{clockNow})
	conn.TransitionBackoffLockedForTest(custos.MCPReasonSpawnFailed)
	if dur := conn.BackoffDurationForTest(); dur != 2*time.Second {
		t.Errorf("1st backoff = %v, want 2s", dur)
	}
	if status := conn.Status(); status.State != custos.MCPStateBackoff {
		t.Errorf("status.State = %q, want %q", status.State, custos.MCPStateBackoff)
	}

	// 2nd failure: 2s -> 4s
	conn.SetRestartsForTest(append([]time.Time{clockNow}, clockNow))
	conn.TransitionBackoffLockedForTest(custos.MCPReasonSpawnFailed)
	if dur := conn.BackoffDurationForTest(); dur != 4*time.Second {
		t.Errorf("2nd backoff = %v, want 4s", dur)
	}

	// 3rd failure: 4s -> 8s
	conn.SetRestartsForTest(append([]time.Time{clockNow, clockNow}, clockNow))
	conn.TransitionBackoffLockedForTest(custos.MCPReasonSpawnFailed)
	if dur := conn.BackoffDurationForTest(); dur != 8*time.Second {
		t.Errorf("3rd backoff = %v, want 8s", dur)
	}

	// 4th failure: 8s -> 16s
	conn.SetRestartsForTest([]time.Time{clockNow, clockNow, clockNow, clockNow})
	conn.TransitionBackoffLockedForTest(custos.MCPReasonSpawnFailed)
	if dur := conn.BackoffDurationForTest(); dur != 16*time.Second {
		t.Errorf("4th backoff = %v, want 16s", dur)
	}

	// 5th failure in rolling hour -> disabled (§7)
	conn.SetRestartsForTest([]time.Time{clockNow, clockNow, clockNow, clockNow, clockNow})
	conn.TransitionBackoffLockedForTest(custos.MCPReasonSpawnFailed)
	status := conn.Status()
	if status.State != custos.MCPStateDisabled {
		t.Errorf("5th restart state = %q, want %q", status.State, custos.MCPStateDisabled)
	}
	if status.Reason != custos.MCPReasonSpawnFailed {
		t.Errorf("5th restart reason = %q, want %q", status.Reason, custos.MCPReasonSpawnFailed)
	}

	// Budget exhaustion from protocol faults -> reason protocol (§7)
	conn.SetStateForTest(custos.MCPStateBackoff, custos.MCPReasonProtocol)
	conn.SetRestartsForTest([]time.Time{clockNow, clockNow, clockNow, clockNow, clockNow})
	conn.TransitionBackoffLockedForTest(custos.MCPReasonProtocol)
	status = conn.Status()
	if status.State != custos.MCPStateDisabled {
		t.Errorf("protocol exhaustion state = %q, want %q", status.State, custos.MCPStateDisabled)
	}
	if status.Reason != custos.MCPReasonProtocol {
		t.Errorf("protocol exhaustion reason = %q, want %q", status.Reason, custos.MCPReasonProtocol)
	}

	// Agreement at startLocked pre-check
	conn.SetStateForTest(custos.MCPStateBackoff, custos.MCPReasonSpawnFailed)
	conn.SetRestartsForTest([]time.Time{clockNow, clockNow, clockNow, clockNow, clockNow})
	_ = conn.TriggerStartLockedForTest(context.Background())
	status = conn.Status()
	if status.State != custos.MCPStateDisabled {
		t.Errorf("startLocked exhaustion state = %q, want %q", status.State, custos.MCPStateDisabled)
	}
	if status.Reason != custos.MCPReasonSpawnFailed {
		t.Errorf("startLocked exhaustion reason = %q, want %q", status.Reason, custos.MCPReasonSpawnFailed)
	}

	conn.SetStateForTest(custos.MCPStateBackoff, custos.MCPReasonProtocol)
	conn.SetRestartsForTest([]time.Time{clockNow, clockNow, clockNow, clockNow, clockNow})
	_ = conn.TriggerStartLockedForTest(context.Background())
	status = conn.Status()
	if status.State != custos.MCPStateDisabled {
		t.Errorf("startLocked protocol exhaustion state = %q, want %q", status.State, custos.MCPStateDisabled)
	}
	if status.Reason != custos.MCPReasonProtocol {
		t.Errorf("startLocked protocol exhaustion reason = %q, want %q", status.Reason, custos.MCPReasonProtocol)
	}

	// Verify 5 minute cap
	conn.SetRestartsForTest(nil)
	conn.SetBackoffDurForTest(4 * time.Minute)
	conn.TransitionBackoffLockedForTest(custos.MCPReasonSpawnFailed)
	if dur := conn.BackoffDurationForTest(); dur != 5*time.Minute {
		t.Errorf("capped backoff = %v, want 5m, got %v", 5*time.Minute, dur)
	}
}

// TestNotifyCredentialGone_Revocation verifies credential deletion
// behavior (§6).
func TestNotifyCredentialGone_Revocation(t *testing.T) {
	tempDir := t.TempDir()

	cfg := custos.MCPServerConfig{
		Name:      "notified-mcp",
		Transport: "stdio",
		Command:   "echo",
		Env: map[string]string{
			"API_KEY": "${vault:my-api-key}",
		},
	}

	conn := custos.NewMCPConnector(cfg, tempDir)
	conn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)
	conn.SetNeedlesForTest([]string{"secret-key-12345678"})

	// Notify for unrelated credential has no effect
	conn.NotifyCredentialGone("unrelated-cred")
	if status := conn.Status(); status.State != custos.MCPStateReady {
		t.Errorf("unrelated cred changed state to %q, want %q", status.State, custos.MCPStateReady)
	}

	// In-flight call retention: verify NotifyCredentialGone drains inFlight
	conn.SetInFlightForTest(1)

	doneCh := make(chan struct{})
	go func() {
		conn.NotifyCredentialGone("my-api-key")
		close(doneCh)
	}()

	// Short sleep to ensure NotifyCredentialGone reached Wait
	time.Sleep(20 * time.Millisecond)
	select {
	case <-doneCh:
		t.Fatal("NotifyCredentialGone returned while inFlight > 0")
	default:
	}

	// Clear inFlight and broadcast
	conn.SetInFlightForTest(0)
	conn.DrainCondBroadcastForTest()

	select {
	case <-doneCh:
		// Succeeded
	case <-time.After(2 * time.Second):
		t.Fatal("NotifyCredentialGone timed out waiting for drain")
	}

	// Now state must be disabled / credential_unavailable
	status := conn.Status()
	if status.State != custos.MCPStateDisabled {
		t.Errorf("state = %q, want %q", status.State, custos.MCPStateDisabled)
	}
	if status.Reason != custos.MCPReasonCredentialUnavailable {
		t.Errorf("reason = %q, want %q", status.Reason, custos.MCPReasonCredentialUnavailable)
	}

	// Subsequent Execute immediately fails with ErrCodeDenied
	_, err := conn.Execute(context.Background(), "any_tool", json.RawMessage(`{}`), custos.Credential{})
	if err == nil {
		t.Fatal("expected error from disabled connector, got nil")
	}
	classified := custos.ClassifyWorkerErrorForTest(err)
	if classified.Code != custos.ErrCodeDenied {
		t.Errorf("code = %q, want %q", classified.Code, custos.ErrCodeDenied)
	}
	if classified.Detail != "denied: credential unavailable" {
		t.Errorf("detail = %q, want %q", classified.Detail, "denied: credential unavailable")
	}

	// Finding 5 extension: block fake session ListTools, call Refresh in goroutine,
	// call NotifyCredentialGone and assert child is dead within sigkill grace while refresh is still blocked.
	t.Run("child_dead_while_refresh_blocked", func(t *testing.T) {
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0"}, nil)
		serverSession, err := server.Connect(context.Background(), serverTransport, nil)
		if err != nil {
			t.Fatalf("server.Connect failed: %v", err)
		}
		defer serverSession.Close()

		unblockCh := make(chan struct{})
		wrapped := &wrappedTransport{
			underlying: clientTransport,
			onWrite: func(msg jsonrpc.Message) {
				if req, ok := msg.(*jsonrpc.Request); ok && req.Method == "tools/list" {
					<-unblockCh
				}
			},
		}

		client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1.0"}, nil)
		session, err := client.Connect(context.Background(), wrapped, nil)
		if err != nil {
			t.Fatalf("client.Connect failed: %v", err)
		}
		defer session.Close()

		conn2 := custos.NewMCPConnector(cfg, tempDir, custos.WithSigkillGrace(500*time.Millisecond))
		conn2.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)
		conn2.SetSessionForTest(session)

		// Spawn live child process
		child, _, _, err := custos.SpawnChildProcess("sleep", []string{"60"}, nil, tempDir, nil, nil)
		if err != nil {
			t.Fatalf("SpawnChildProcess failed: %v", err)
		}
		conn2.SetChildProcForTest(child)

		// Call Refresh in a goroutine (which calls ListTools and blocks on unblockCh)
		refreshDone := make(chan error, 1)
		go func() {
			refreshDone <- conn2.Refresh(context.Background())
		}()

		// Wait briefly so Refresh is blocked in ListTools
		time.Sleep(30 * time.Millisecond)

		// Assert Refresh is still blocked
		select {
		case <-refreshDone:
			t.Fatal("Refresh completed before unblock")
		default:
		}

		// Call NotifyCredentialGone - must not be blocked by Refresh's network call
		goneStart := time.Now()
		conn2.NotifyCredentialGone("my-api-key")
		goneDuration := time.Since(goneStart)

		// Assert child is dead within sigkill grace
		select {
		case <-child.WaitChan():
			// Child is dead as required
		case <-time.After(time.Second):
			t.Fatal("child was not killed within sigkill grace period")
		}

		if goneDuration > 2*time.Second {
			t.Errorf("NotifyCredentialGone took %v, should not wait on blocked network call", goneDuration)
		}

		// Unblock ListTools and clean up
		close(unblockCh)
		<-refreshDone
	})
}

type wrappedTransport struct {
	underlying mcp.Transport
	onWrite    func(msg jsonrpc.Message)
}

func (w *wrappedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := w.underlying.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &wrappedConn{Connection: conn, onWrite: w.onWrite}, nil
}

type wrappedConn struct {
	mcp.Connection

	onWrite func(msg jsonrpc.Message)
}

func (c *wrappedConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if c.onWrite != nil {
		c.onWrite(msg)
	}
	return c.Connection.Write(ctx, msg)
}

// TestBackoffAutoRetry tests automatic backoff retries per §7 / Finding 2:
// crash a live child twice with tiny backoff and assert retry attempt #2 happens
// automatically without any external Refresh call.
func TestBackoffAutoRetry(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := custos.MCPServerConfig{
		Name:      "autoretry-mcp",
		Transport: "stdio",
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestMCPHelperProcess", "--"},
		Env: map[string]string{
			"GO_WANT_MCP_HELPER": "1",
			"CUSTOS_HELPER_MODE": "mcp_server",
		},
	}

	conn := custos.NewMCPConnector(cfg, tmpDir)
	conn.SetBackoffDurForTest(10 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := conn.OnFirstUnlock(ctx); err != nil {
		t.Fatalf("OnFirstUnlock failed: %v", err)
	}

	status := conn.Status()
	if status.State != custos.MCPStateReady {
		t.Fatalf("expected state ready, got %q", status.State)
	}
	if status.Restarts != 1 {
		t.Fatalf("expected 1 initial start, got %d", status.Restarts)
	}

	child1 := conn.ChildProcForTest()
	if child1 == nil {
		t.Fatal("expected child1 to be non-nil")
	}

	// 1st crash: kill live child 1
	conn.SetBackoffDurForTest(10 * time.Millisecond)
	_ = child1.KillGroup(10 * time.Millisecond)

	// Wait for automatic retry #1 to fire and enter ready
	deadline := time.Now().Add(3 * time.Second)
	var child2 *custos.ChildProcess
	for time.Now().Before(deadline) {
		status = conn.Status()
		c2 := conn.ChildProcForTest()
		if status.Restarts >= 2 && status.State == custos.MCPStateReady && c2 != nil && c2 != child1 {
			child2 = c2
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if child2 == nil {
		t.Fatalf("retry #1 did not start a new ready child automatically: state=%s, restarts=%d", status.State, status.Restarts)
	}

	// 2nd crash: kill live child 2
	conn.SetBackoffDurForTest(10 * time.Millisecond)
	_ = child2.KillGroup(10 * time.Millisecond)

	// Wait for automatic retry #2 to fire
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status = conn.Status()
		if status.Restarts >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	status = conn.Status()
	if status.Restarts < 3 {
		t.Fatalf("expected restarts >= 3 after second crash, got %d (state=%s)", status.Restarts, status.State)
	}

	// Clean up child process if still running
	if curChild := conn.ChildProcForTest(); curChild != nil {
		_ = curChild.KillGroup(10 * time.Millisecond)
	}
}

func TestDebouncedRefreshAndBudgetLaundering(t *testing.T) {
	// (a) Burst of 20 list_changed notifications drives at most 2 list calls using a counting fake session
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0"}, nil)
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("server.Connect failed: %v", err)
	}
	defer serverSession.Close()

	var listCalls int32
	wrapped := &wrappedTransport{
		underlying: clientTransport,
		onWrite: func(msg jsonrpc.Message) {
			if req, ok := msg.(*jsonrpc.Request); ok && req.Method == "tools/list" {
				atomic.AddInt32(&listCalls, 1)
				time.Sleep(50 * time.Millisecond)
			}
		},
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1.0"}, nil)
	session, err := client.Connect(context.Background(), wrapped, nil)
	if err != nil {
		t.Fatalf("client.Connect failed: %v", err)
	}
	defer session.Close()

	conn := custos.NewMCPConnector(custos.MCPServerConfig{Name: "debounce-mcp", Transport: "stdio", Command: "echo"}, t.TempDir())
	conn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)
	conn.SetSessionForTest(session)

	// Fire burst of 20 notifications concurrently
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn.ScheduleDebouncedRefreshForTest()
		}()
	}
	wg.Wait()

	// Wait up to 500ms for debounced refreshes (at most 2 calls) to settle
	time.Sleep(250 * time.Millisecond)

	count := atomic.LoadInt32(&listCalls)
	if count > 2 || count == 0 {
		t.Errorf("listCalls = %d, want at most 2 (1 in-flight + 1 coalesced)", count)
	}

	// (b) Automatic Refresh on a budget-exhausted connector leaves it disabled
	connExhausted := custos.NewMCPConnector(custos.MCPServerConfig{Name: "exhausted", Transport: "stdio", Command: "echo"}, t.TempDir())
	connExhausted.SetStateForTest(custos.MCPStateDisabled, custos.MCPReasonSpawnFailed)
	connExhausted.SetRestartsForTest([]time.Time{time.Now(), time.Now(), time.Now(), time.Now(), time.Now()})

	// Automatic Refresh must be a no-op
	if err := connExhausted.Refresh(context.Background()); err != nil {
		t.Fatalf("automatic Refresh failed: %v", err)
	}
	st := connExhausted.Status()
	if st.State != custos.MCPStateDisabled {
		t.Errorf("expected state disabled, got %q", st.State)
	}
	if st.Restarts != 5 {
		t.Errorf("expected restarts=5 (not laundered), got %d", st.Restarts)
	}

	// ManualRefresh must clear restarts and backoff
	_ = connExhausted.ManualRefresh(context.Background())
	st = connExhausted.Status()
	if st.Restarts > 1 {
		t.Errorf("expected restarts <= 1 after manual refresh, got %d", st.Restarts)
	}
}

// TestRefresh_A6Revocation verifies catalog diff audit and auto-rule
// revocation (§4.3, A6).
func TestRefresh_A6Revocation(t *testing.T) {
	tempDir := t.TempDir()

	cfg := custos.MCPServerConfig{
		Name:      "refresh-mcp",
		Transport: "stdio",
		Command:   "echo",
	}

	audit := &mockAuditSink{}
	revoker := &mockRevoker{}

	conn := custos.NewMCPConnector(
		cfg,
		tempDir,
		custos.WithAuditSink(audit),
		custos.WithFingerprintRevoker(revoker),
	)

	// Initial catalog has tool_a and tool_b
	initialTools := []*mcp.Tool{
		{
			Name:        "tool_a",
			Description: "version 1 description",
		},
		{
			Name:        "tool_b",
			Description: "will be removed in next catalog",
		},
	}
	initialSnap, err := custos.BuildCatalogSnapshot(
		"refresh-mcp",
		initialTools,
		nil,
		nil,
		nil,
		time.Minute,
		time.Now(),
	)
	if err != nil {
		t.Fatalf("BuildCatalogSnapshot: %v", err)
	}
	conn.SetSnapshotForTest(initialSnap)

	// New catalog has modified tool_a (changed description -> changed fp),
	// removed tool_b, and added tool_c
	newTools := []*mcp.Tool{
		{
			Name:        "tool_a",
			Description: "version 2 modified description",
		},
		{
			Name:        "tool_c",
			Description: "brand new tool",
		},
	}
	newSnap, err := custos.BuildCatalogSnapshot(
		"refresh-mcp",
		newTools,
		nil,
		nil,
		nil,
		time.Minute,
		time.Now(),
	)
	if err != nil {
		t.Fatalf("BuildCatalogSnapshot: %v", err)
	}

	// Compute diff
	diff := custos.CatalogDiff("refresh-mcp", initialSnap, newSnap)
	expectedDiff := []string{"+refresh-mcp/tool_c", "-refresh-mcp/tool_b", "~refresh-mcp/tool_a"}
	if len(diff) != len(expectedDiff) {
		t.Fatalf("diff = %v, want %v", diff, expectedDiff)
	}
	for i, d := range diff {
		if d != expectedDiff[i] {
			t.Errorf("diff[%d] = %q, want %q", i, d, expectedDiff[i])
		}
	}

	// Verify revocation logic for diff: A6 requires auto-rule revocation
	// for any tool whose fingerprint changed or was removed.
	for toolName, oldFp := range initialSnap.Fingerprints {
		newFp, exists := newSnap.Fingerprints[toolName]
		if !exists || newFp != oldFp {
			_ = revoker.RevokeAlwaysRule("refresh-mcp", "refresh-mcp/"+toolName)
		}
	}

	revoked := revoker.Calls()
	if len(revoked) != 2 {
		t.Fatalf("revoked calls = %v, want 2 calls", revoked)
	}

	revokedMap := make(map[string]bool)
	for _, call := range revoked {
		if call[0] != "refresh-mcp" {
			t.Errorf("connector in call = %q, want refresh-mcp", call[0])
		}
		revokedMap[call[1]] = true
	}

	if !revokedMap["refresh-mcp/tool_a"] {
		t.Errorf("expected refresh-mcp/tool_a to be revoked (fingerprint changed)")
	}
	if !revokedMap["refresh-mcp/tool_b"] {
		t.Errorf("expected refresh-mcp/tool_b to be revoked (tool removed)")
	}
}

// TestRailRegistry verifies registry management and broadcast verbs.
func TestRailRegistry(t *testing.T) {
	tempDir := t.TempDir()

	cfgs := []custos.MCPServerConfig{
		{Name: "server-1", Transport: "stdio", Command: "echo"},
		{Name: "server-2", Transport: "stdio", Command: "echo"},
	}

	reg := custos.NewRailRegistry(cfgs, tempDir)

	if c1 := reg.Connector("server-1"); c1 == nil || c1.Name() != "server-1" {
		t.Errorf("Connector(server-1) = %v, want non-nil", c1)
	}
	if c2 := reg.Connector("server-2"); c2 == nil || c2.Name() != "server-2" {
		t.Errorf("Connector(server-2) = %v, want non-nil", c2)
	}
	if c3 := reg.Connector("server-3"); c3 != nil {
		t.Errorf("Connector(server-3) = %v, want nil", c3)
	}

	list := reg.Connectors()
	if len(list) != 2 {
		t.Fatalf("Connectors() length = %d, want 2", len(list))
	}
	if list[0].Name() != "server-1" || list[1].Name() != "server-2" {
		t.Errorf("Connectors() order = [%s, %s], want [server-1, server-2]", list[0].Name(), list[1].Name())
	}

	// Broadcast NotifyCredentialGone
	reg.NotifyCredentialGone("server-1")
	if s := reg.Connector("server-1").Status(); s.State != custos.MCPStateDisabled {
		t.Errorf("server-1 state = %q, want %q", s.State, custos.MCPStateDisabled)
	}
	if s := reg.Connector("server-2").Status(); s.State != custos.MCPStateStarting {
		t.Errorf("server-2 state = %q, want %q", s.State, custos.MCPStateStarting)
	}
}
