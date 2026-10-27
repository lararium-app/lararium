package custosdoor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func setupTestClient(t *testing.T) (*Client, *fakeServer, func()) {
	t.Helper()
	d := shortDir(t)
	sock := filepath.Join(d, "doors.sock")
	tok := filepath.Join(d, "door.token")
	if err := os.WriteFile(tok, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fs := newFakeServer(t, sock)
	cl, err := New(Options{
		SockPath:   sock,
		TokenPath:  tok,
		BaseDelay:  5 * time.Millisecond,
		MaxDelay:   20 * time.Millisecond,
		AckTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go cl.Run(ctx)

	cleanup := func() {
		cancel()
		<-cl.Done()
	}
	return cl, fs, cleanup
}

func waitAttached(t *testing.T, cl *Client, fs *fakeServer) *fakeConn {
	t.Helper()
	fc := fs.hello(t, "[]")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cl.mu.Lock()
		attached := cl.sess != nil
		cl.mu.Unlock()
		if attached {
			return fc
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("client never attached session")
	return nil
}

func TestMCPClient_Status(t *testing.T) {
	cl, fs, cleanup := setupTestClient(t)
	defer cleanup()

	fc := waitAttached(t, cl, fs)

	// Goroutine to handle STATUS command on fake server
	go func() {
		line := fc.recv(t)
		if line != "MCP-STATUS" {
			t.Errorf("expected STATUS, got %q", line)
		}
		fc.send(`OK [{"name":"test-conn","state":"ready","tools":2,"restarts":0,"reason":"ok"}]`)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	statuses, err := cl.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Name != "test-conn" || statuses[0].State != "ready" {
		t.Fatalf("unexpected statuses: %+v", statuses)
	}

	// Test locked error mapping
	go func() {
		_ = fc.recv(t)
		fc.send("ERR locked")
	}()

	_, err = cl.Status(ctx)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
}

func TestMCPClient_Tools(t *testing.T) {
	cl, fs, cleanup := setupTestClient(t)
	defer cleanup()

	fc := waitAttached(t, cl, fs)

	go func() {
		line := fc.recv(t)
		if line != "MCP-TOOLS test-conn" {
			t.Errorf("expected TOOLS test-conn, got %q", line)
		}
		fc.send(`OK [{"name":"op1","flags":"none","title":"Operation 1"},{"name":"op2","flags":"read_only","title":"Operation 2"}]`)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	tools, err := cl.Tools(ctx, "test-conn")
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 2 || tools[0].Connector != "test-conn" || tools[0].Name != "op1" || tools[1].Flags != "read_only" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
}

func TestMCPClient_Refresh(t *testing.T) {
	cl, fs, cleanup := setupTestClient(t)
	defer cleanup()

	fc := waitAttached(t, cl, fs)

	// Happy path
	go func() {
		line := fc.recv(t)
		if line != "MCP-REFRESH test-conn" {
			t.Errorf("expected REFRESH test-conn, got %q", line)
		}
		fc.send("OK refreshed")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := cl.Refresh(ctx, "test-conn"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// Error path
	go func() {
		_ = fc.recv(t)
		fc.send(`ERR connector "missing" not found`)
	}()

	err := cl.Refresh(ctx, "missing")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestMCPClient_PushedFramesAndConvergence(t *testing.T) {
	cl, fs, cleanup := setupTestClient(t)
	defer cleanup()

	fc := waitAttached(t, cl, fs)

	// Channel to observe subscriber updates
	var (
		mu         sync.Mutex
		receivedSt []ConnectorStatus
		receivedTl []ToolInfo
		updateSig  = make(chan struct{}, 10)
	)

	unsub := cl.SubscribeMCP(func(st []ConnectorStatus, tl []ToolInfo) {
		mu.Lock()
		receivedSt = st
		receivedTl = tl
		mu.Unlock()
		select {
		case updateSig <- struct{}{}:
		default:
		}
	})
	defer unsub()

	// Initial empty snapshot on subscribe
	select {
	case <-updateSig:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial subscribe callback")
	}

	// Server pushes MCP-STATUS
	fc.send(`MCP-STATUS [{"name":"alpha","state":"ready","tools":1,"restarts":0,"reason":"ok"}]`)

	select {
	case <-updateSig:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pushed MCP-STATUS")
	}

	mu.Lock()
	if len(receivedSt) != 1 || receivedSt[0].Name != "alpha" {
		t.Fatalf("unexpected received status: %+v", receivedSt)
	}
	mu.Unlock()

	// Server pushes MCP-TOOLS
	fc.send(`MCP-TOOLS alpha [{"name":"tool_a","flags":"none","title":"Tool A"}]`)

	select {
	case <-updateSig:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pushed MCP-TOOLS")
	}

	mu.Lock()
	if len(receivedTl) != 1 || receivedTl[0].Name != "tool_a" || receivedTl[0].Connector != "alpha" {
		t.Fatalf("unexpected received tools: %+v", receivedTl)
	}
	mu.Unlock()

	// Assert Client getters match converged state
	statuses := cl.Statuses()
	if len(statuses) != 1 || statuses[0].Name != "alpha" {
		t.Fatalf("Statuses() = %+v", statuses)
	}
	listTools := cl.ListTools()
	if len(listTools) != 1 || listTools[0].Name != "tool_a" {
		t.Fatalf("ListTools() = %+v", listTools)
	}

	// A new subscriber attaching after convergence immediately gets the full snapshot
	convergedSig := make(chan struct{})
	unsub2 := cl.Rail().Subscribe(func(st []ConnectorStatus, tl []ToolInfo) {
		if len(st) == 1 && len(tl) == 1 {
			select {
			case <-convergedSig:
			default:
				close(convergedSig)
			}
		}
	})
	defer unsub2()

	select {
	case <-convergedSig:
	case <-time.After(time.Second):
		t.Fatal("late subscriber did not immediately receive converged snapshot")
	}
}

func TestMCPClient_DoorDown(t *testing.T) {
	d := shortDir(t)
	sock := filepath.Join(d, "doors.sock")
	tok := filepath.Join(d, "door.token")
	if err := os.WriteFile(tok, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cl, err := New(Options{
		SockPath:  sock,
		TokenPath: tok,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	_, err = cl.Status(ctx)
	if !errors.Is(err, ErrDoorDown) {
		t.Fatalf("expected ErrDoorDown, got %v", err)
	}
	_, err = cl.Tools(ctx, "any")
	if !errors.Is(err, ErrDoorDown) {
		t.Fatalf("expected ErrDoorDown, got %v", err)
	}
	err = cl.Refresh(ctx, "any")
	if !errors.Is(err, ErrDoorDown) {
		t.Fatalf("expected ErrDoorDown, got %v", err)
	}
}

// TestMCPStatusEmptyPrunesTools verifies that an empty MCP-STATUS frame clears
// both cached statuses and the tool rows of connectors no longer reported (#96).
func TestMCPStatusEmptyPrunesTools(t *testing.T) {
	cl, fs, cleanup := setupTestClient(t)
	defer cleanup()
	fc := waitAttached(t, cl, fs)
	fc.send(`MCP-STATUS [{"name":"alpha","state":"ready","tools":1,"restarts":0,"reason":"ok"}]`)
	fc.send(`MCP-TOOLS alpha [{"name":"tool_a","flags":"none","title":"Tool A"}]`)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(cl.Statuses()) == 1 && len(cl.ListTools()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(cl.Statuses()) != 1 || len(cl.ListTools()) != 1 {
		t.Fatalf("warm-up failed: statuses=%+v tools=%+v", cl.Statuses(), cl.ListTools())
	}

	fc.send(`MCP-STATUS []`)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(cl.Statuses()) == 0 && len(cl.ListTools()) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stale state survived empty snapshot: statuses=%+v tools=%+v", cl.Statuses(), cl.ListTools())
}

// TestDetachClearsMCPState verifies that losing the door session prunes all
// cached connector cards instead of leaving the previous daemon's set on
// screen forever (#96).
func TestDetachClearsMCPState(t *testing.T) {
	cl, fs, cleanup := setupTestClient(t)
	defer cleanup()
	fc := waitAttached(t, cl, fs)

	fc.send(`MCP-STATUS [{"name":"alpha","state":"ready","tools":0,"restarts":0,"reason":"ok"}]`)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(cl.Statuses()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(cl.Statuses()) != 1 {
		t.Fatalf("warm-up failed: statuses=%+v", cl.Statuses())
	}

	// Kill the session from the server side; Run() must detach and clear.
	_ = fc.c.Close()

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(cl.Statuses()) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("MCP state survived detach: statuses=%+v", cl.Statuses())
}

// TestMCPClient_CallDoesNotBlockResolve verifies that a parked MCP-CALL uses
// its own dedicated connection and does not hold cmdMu on c.sess, allowing
// ResolveVia to settle a card immediately while Call is waiting (A-2).
func TestMCPClient_CallDoesNotBlockResolve(t *testing.T) {
	cl, fs, cleanup := setupTestClient(t)
	defer cleanup()
	mainConn := waitAttached(t, cl, fs)

	cardID := "a_1234567890abcdef"
	callResultCh := make(chan string, 1)
	callErrCh := make(chan error, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		res, err := cl.Call(ctx, "alpha", "echo", json.RawMessage(`{"message":"hi"}`))
		if err != nil {
			callErrCh <- err
			return
		}
		callResultCh <- string(res)
	}()

	// Accept the dedicated MCP-CALL connection on the fake server.
	callConn := fs.hello(t, "[]")
	if got := callConn.recv(t); got != `MCP-CALL alpha echo {"message":"hi"}` {
		t.Fatalf("Call command = %q", got)
	}

	// While Call is parked on callConn, Resolve on the main session must
	// complete immediately and release callConn upon receiving APPROVE.
	go func() {
		approveLine := mainConn.recv(t)
		if approveLine != "APPROVE "+cardID+" once via web" {
			t.Errorf("expected APPROVE on mainConn, got %q", approveLine)
		}
		mainConn.send("OK")
		callConn.send(`OK {"content":[{"type":"text","text":"echo:hi"}]}`)
	}()

	state, err := cl.Resolve(cardID, VerdictOnce)
	if err != nil || state != StateApproved {
		t.Fatalf("Resolve = (%q, %v), want (%q, nil)", state, err, StateApproved)
	}

	select {
	case err := <-callErrCh:
		t.Fatalf("Call returned error: %v", err)
	case got := <-callResultCh:
		if !strings.Contains(got, "echo:hi") {
			t.Fatalf("Call result = %q, want echo:hi", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not finish after Resolve")
	}
}
