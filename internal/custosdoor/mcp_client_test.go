package custosdoor

import (
	"context"
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
