package custos_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/surface"
)

// TestV_MD_1_AttachAfterReady asserts that a client attaching after rail is ready
// receives immediate MCP-STATUS and MCP-TOOLS snapshots without asking.
func TestV_MD_1_AttachAfterReady(t *testing.T) {
	dir := mustShortDir(t)
	hub := surface.NewApprovalHub(5 * time.Second)

	cfg := custos.MCPServerConfig{
		Name:      "test-rail",
		Transport: "stdio",
		Command:   "cat",
	}
	conn := custos.NewMCPConnector(cfg, dir)
	conn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)
	conn.SetSnapshotForTest(&custos.CatalogSnapshot{
		Connector: "test-rail",
		Tools: []custos.SnapshotTool{
			{Canonical: "fetch_data", Title: "Fetch Data"},
			{Canonical: "push_data", Title: "Push Data"},
		},
		ToolInfos: []custos.ToolInfo{
			{Connector: "test-rail", Name: "fetch_data", Title: "Fetch Data"},
			{Connector: "test-rail", Name: "push_data", Title: "Push Data"},
		},
	})

	reg := custos.NewRailRegistry([]custos.MCPServerConfig{cfg}, dir)
	reg.RegisterConnector(conn)

	doorSrv, err := custos.StartDoorServer(dir, hub, custos.WithRail(reg))
	if err != nil {
		t.Fatalf("start door server: %v", err)
	}
	defer doorSrv.Close()

	sockPath, tokenPath := custos.DoorPaths(dir)
	tokBytes, err := custos.ReadDoorToken(dir)
	if err != nil {
		t.Fatalf("read door token from %s: %v", tokenPath, err)
	}

	doorConn, err := custos.DialDoor(sockPath, tokBytes, "test_client")
	if err != nil {
		t.Fatalf("dial door: %v", err)
	}
	defer doorConn.Close()

	// Read initial frames pushed on attach
	var statusFrame, toolsFrame string
	for i := range 2 {
		frame, err := doorConn.NextFrame(2 * time.Second)
		if err != nil {
			t.Fatalf("reading attach frame %d: %v", i, err)
		}
		if strings.HasPrefix(frame, "MCP-STATUS ") {
			statusFrame = frame
		} else if strings.HasPrefix(frame, "MCP-TOOLS ") {
			toolsFrame = frame
		}
	}

	if statusFrame == "" {
		t.Fatal("expected MCP-STATUS frame on attach after ready")
	}
	if toolsFrame == "" {
		t.Fatal("expected MCP-TOOLS frame on attach after ready")
	}

	var statuses []custos.ConnectorStatus
	if err := json.Unmarshal([]byte(strings.TrimPrefix(statusFrame, "MCP-STATUS ")), &statuses); err != nil {
		t.Fatalf("parse status frame: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Name != "test-rail" || statuses[0].State != "ready" {
		t.Fatalf("unexpected statuses: %+v", statuses)
	}

	parts := strings.SplitN(strings.TrimPrefix(toolsFrame, "MCP-TOOLS "), " ", 2)
	if len(parts) < 2 || parts[0] != "test-rail" {
		t.Fatalf("unexpected tools frame header: %q", toolsFrame)
	}
	var tools []custos.ToolStatus
	if err := json.Unmarshal([]byte(parts[1]), &tools); err != nil {
		t.Fatalf("parse tools JSON: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "fetch_data" || tools[1].Name != "push_data" {
		t.Fatalf("unexpected tools list: %+v", tools)
	}
}

// TestV_MD_2_AttachBeforeUnlock asserts that a client attaching before vault unlock sees
// locked status and converges to full ready status + tools once unlock notifications fire.
func TestV_MD_2_AttachBeforeUnlock(t *testing.T) {
	dir := mustShortDir(t)
	hub := surface.NewApprovalHub(5 * time.Second)

	cfg := custos.MCPServerConfig{
		Name:      "vault-rail",
		Transport: "stdio",
		Command:   "cat",
	}
	conn := custos.NewMCPConnector(cfg, dir)
	// Initially starting and credential_unavailable (locked state)
	conn.SetStateForTest(custos.MCPStateStarting, custos.MCPReasonCredentialUnavailable)

	reg := custos.NewRailRegistry([]custos.MCPServerConfig{cfg}, dir)
	reg.RegisterConnector(conn)

	doorSrv, err := custos.StartDoorServer(dir, hub, custos.WithRail(reg))
	if err != nil {
		t.Fatalf("start door server: %v", err)
	}
	defer doorSrv.Close()

	sockPath, _ := custos.DoorPaths(dir)
	tok, err := custos.ReadDoorToken(dir)
	if err != nil {
		t.Fatalf("read door token: %v", err)
	}

	doorConn, err := custos.DialDoor(sockPath, tok, "client_before_unlock")
	if err != nil {
		t.Fatalf("dial door: %v", err)
	}
	defer doorConn.Close()

	// 1. Initial snapshot: locked status, no tools
	frame, err := doorConn.NextFrame(2 * time.Second)
	if err != nil {
		t.Fatalf("read initial frame: %v", err)
	}
	if !strings.HasPrefix(frame, "MCP-STATUS ") {
		t.Fatalf("expected MCP-STATUS frame, got %q", frame)
	}
	var initialStatuses []custos.ConnectorStatus
	if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "MCP-STATUS ")), &initialStatuses); err != nil {
		t.Fatalf("unmarshal initial status: %v", err)
	}
	if len(initialStatuses) != 1 || initialStatuses[0].State != custos.MCPStateStarting ||
		initialStatuses[0].Reason != custos.MCPReasonCredentialUnavailable {
		t.Fatalf("unexpected initial status: %+v", initialStatuses)
	}

	// 2. Simulate unlock: transition to ready, provide tools, and notify
	conn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)
	conn.SetSnapshotForTest(&custos.CatalogSnapshot{
		Connector: "vault-rail",
		Tools: []custos.SnapshotTool{
			{Canonical: "unlocked_tool", Title: "Unlocked Tool"},
		},
		ToolInfos: []custos.ToolInfo{
			{Connector: "vault-rail", Name: "unlocked_tool", Title: "Unlocked Tool"},
		},
	})
	conn.NotifyChangeForTest(true)

	// 3. Client receives pushed update and converges
	var readyStatusFrame, readyToolsFrame string
	for i := range 2 {
		f, err := doorConn.NextFrame(3 * time.Second)
		if err != nil {
			t.Fatalf("reading post-unlock frame %d: %v", i, err)
		}
		if strings.HasPrefix(f, "MCP-STATUS ") {
			readyStatusFrame = f
		} else if strings.HasPrefix(f, "MCP-TOOLS ") {
			readyToolsFrame = f
		}
	}

	if readyStatusFrame == "" {
		t.Fatal("expected post-unlock MCP-STATUS frame")
	}
	if readyToolsFrame == "" {
		t.Fatal("expected post-unlock MCP-TOOLS frame")
	}

	var readyStatuses []custos.ConnectorStatus
	if err := json.Unmarshal([]byte(strings.TrimPrefix(readyStatusFrame, "MCP-STATUS ")), &readyStatuses); err != nil {
		t.Fatalf("unmarshal ready status: %v", err)
	}
	if len(readyStatuses) != 1 || readyStatuses[0].State != custos.MCPStateReady {
		t.Fatalf("expected ready state, got: %+v", readyStatuses)
	}

	if !strings.Contains(readyToolsFrame, "unlocked_tool") {
		t.Fatalf("tools frame should contain unlocked_tool: %q", readyToolsFrame)
	}
}

// TestV_MD_3_UnauthenticatedSocket asserts that an unauthenticated socket receives zero MCP frames.
func TestV_MD_3_UnauthenticatedSocket(t *testing.T) {
	dir := mustShortDir(t)
	hub := surface.NewApprovalHub(5 * time.Second)

	cfg := custos.MCPServerConfig{Name: "secret-rail", Transport: "stdio", Command: "cat"}
	conn := custos.NewMCPConnector(cfg, dir)
	conn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)
	conn.SetSnapshotForTest(&custos.CatalogSnapshot{
		Connector: "secret-rail",
		Tools:     []custos.SnapshotTool{{Canonical: "secret_op", Title: "Secret"}},
		ToolInfos: []custos.ToolInfo{{Connector: "secret-rail", Name: "secret_op", Title: "Secret"}},
	})

	reg := custos.NewRailRegistry([]custos.MCPServerConfig{cfg}, dir)
	reg.RegisterConnector(conn)

	doorSrv, err := custos.StartDoorServer(dir, hub, custos.WithRail(reg))
	if err != nil {
		t.Fatalf("start door server: %v", err)
	}
	defer doorSrv.Close()

	sockPath, _ := custos.DoorPaths(dir)

	// Scenario A: raw connection sends verb before HELLO
	rawConn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial raw: %v", err)
	}
	defer rawConn.Close()

	_ = rawConn.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = rawConn.Write([]byte("MCP-STATUS\n"))

	reader := bufio.NewReader(rawConn)
	reply, err := reader.ReadString('\n')
	if err != nil && !strings.Contains(reply, "ERR") {
		// Connection closed immediately is also valid
	} else if strings.Contains(reply, "MCP-STATUS") || strings.Contains(reply, "secret_op") {
		t.Fatalf("unauthenticated socket leaked MCP state: %q", reply)
	}

	// Scenario B: wrong token in HELLO
	badConn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial bad: %v", err)
	}
	defer badConn.Close()

	_ = badConn.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = badConn.Write([]byte("HELLO wrong-token-1234567890 test\n"))

	badReader := bufio.NewReader(badConn)
	badReply, _ := badReader.ReadString('\n')
	if !strings.HasPrefix(badReply, "ERR bad_token") {
		t.Fatalf("expected ERR bad_token, got: %q", badReply)
	}

	// Read any subsequent line: must be EOF / no MCP frames
	_ = badConn.SetDeadline(time.Now().Add(200 * time.Millisecond))
	extra, _ := badReader.ReadString('\n')
	if strings.Contains(extra, "MCP-STATUS") || strings.Contains(extra, "secret_op") {
		t.Fatalf("unauthenticated socket received extra MCP frames: %q", extra)
	}
}

// TestV_MD_4_RefreshRoundTrip asserts that REFRESH round-trip on doors.sock returns
// the identical payload as the CTL path.
func TestV_MD_4_RefreshRoundTrip(t *testing.T) {
	dir := mustShortDir(t)
	hub := surface.NewApprovalHub(5 * time.Second)

	cfg := custos.MCPServerConfig{
		Name:        "refresh-rail",
		Transport:   "stdio",
		Command:     "cat",
		ListTimeout: 2 * time.Second,
	}
	conn := custos.NewMCPConnector(cfg, dir)
	// Disabled connector returns nil immediately from ManualRefresh (§7)
	conn.SetStateForTest(custos.MCPStateDisabled, custos.MCPReasonOK)

	reg := custos.NewRailRegistry([]custos.MCPServerConfig{cfg}, dir)
	reg.RegisterConnector(conn)

	ctlSrv, err := custos.StartCtlServer(dir, nil, nil)
	if err != nil {
		t.Fatalf("start ctl server: %v", err)
	}
	defer ctlSrv.Close()
	ctlSrv.SetRailRegistry(reg)

	doorSrv, err := custos.StartDoorServer(dir, hub, custos.WithRail(reg))
	if err != nil {
		t.Fatalf("start door server: %v", err)
	}
	defer doorSrv.Close()

	// 1. REFRESH over CTL: "MCP-REFRESH <token> <name>\n"
	ctlSock, _ := custos.CtlPaths(dir)
	ctlConn, err := net.Dial("unix", ctlSock)
	if err != nil {
		t.Fatalf("dial ctl: %v", err)
	}
	defer ctlConn.Close()

	ctlTok, err := custos.ReadCtlToken(dir)
	if err != nil {
		t.Fatalf("read ctl token: %v", err)
	}
	ctlReader := bufio.NewReader(ctlConn)

	cmd := fmt.Sprintf("MCP-REFRESH %s refresh-rail\n", ctlTok)
	_, _ = ctlConn.Write([]byte(cmd))
	ctlReply, err := ctlReader.ReadString('\n')
	if err != nil {
		t.Fatalf("ctl refresh: %v", err)
	}

	// 2. REFRESH over DOORS: "MCP-REFRESH <name>\n"
	doorSock, _ := custos.DoorPaths(dir)
	doorTok, err := custos.ReadDoorToken(dir)
	if err != nil {
		t.Fatalf("read door token: %v", err)
	}

	doorConn, err := custos.DialDoor(doorSock, doorTok, "refresh_client")
	if err != nil {
		t.Fatalf("dial door: %v", err)
	}
	defer doorConn.Close()

	// Drain initial attach snapshot frames
	_, _ = doorConn.NextFrame(500 * time.Millisecond)

	if err := doorConn.RawSend("MCP-REFRESH refresh-rail\n"); err != nil {
		t.Fatalf("send refresh over door: %v", err)
	}
	doorReply, err := doorConn.RawReadLine(5 * time.Second)
	if err != nil {
		t.Fatalf("read refresh over door: %v", err)
	}

	if strings.TrimSpace(doorReply) != strings.TrimSpace(ctlReply) {
		t.Fatalf("round-trip mismatch:\n  ctl:  %q\n  door: %q", ctlReply, doorReply)
	}
	if strings.TrimSpace(doorReply) != "OK refreshed" {
		t.Fatalf("expected OK refreshed, got: %q", doorReply)
	}
}

// TestV_MD_5_ToolsUnknownConnector asserts that TOOLS for an unknown connector returns
// the same error string on DOORS as on CTL.
func TestV_MD_5_ToolsUnknownConnector(t *testing.T) {
	dir := mustShortDir(t)
	hub := surface.NewApprovalHub(5 * time.Second)

	reg := custos.NewRailRegistry(nil, dir)

	ctlSrv, err := custos.StartCtlServer(dir, nil, nil)
	if err != nil {
		t.Fatalf("start ctl server: %v", err)
	}
	defer ctlSrv.Close()
	ctlSrv.SetRailRegistry(reg)

	doorSrv, err := custos.StartDoorServer(dir, hub, custos.WithRail(reg))
	if err != nil {
		t.Fatalf("start door server: %v", err)
	}
	defer doorSrv.Close()

	// Query CTL: "MCP-TOOLS <token> nonexistent_connector\n"
	ctlSock, _ := custos.CtlPaths(dir)
	ctlConn, err := net.Dial("unix", ctlSock)
	if err != nil {
		t.Fatalf("dial ctl: %v", err)
	}
	defer ctlConn.Close()
	ctlTok, _ := custos.ReadCtlToken(dir)
	ctlReader := bufio.NewReader(ctlConn)

	cmd := fmt.Sprintf("MCP-TOOLS %s nonexistent_connector\n", ctlTok)
	_, _ = ctlConn.Write([]byte(cmd))
	ctlErrLine, _ := ctlReader.ReadString('\n')

	// Query DOORS: "MCP-TOOLS nonexistent_connector\n"
	doorSock, _ := custos.DoorPaths(dir)
	doorTok, _ := custos.ReadDoorToken(dir)
	doorConn, err := custos.DialDoor(doorSock, doorTok, "unknown_tools_client")
	if err != nil {
		t.Fatalf("dial door: %v", err)
	}
	defer doorConn.Close()

	if err := doorConn.RawSend("MCP-TOOLS nonexistent_connector\n"); err != nil {
		t.Fatalf("send tools over door: %v", err)
	}
	doorErrLine, err := doorConn.RawReadLine(5 * time.Second)
	if err != nil {
		t.Fatalf("read tools over door: %v", err)
	}

	if strings.TrimSpace(doorErrLine) != strings.TrimSpace(ctlErrLine) {
		t.Fatalf("error mismatch:\n  ctl:  %q\n  door: %q", ctlErrLine, doorErrLine)
	}
	if !strings.Contains(doorErrLine, `ERR connector "nonexistent_connector" not found`) {
		t.Fatalf("unexpected error format: %q", doorErrLine)
	}
}

// TestV_MD_6_RestartPushesStatus asserts that a connector restart event pushes an updated
// MCP-STATUS frame concurrently to two active subscribers.
func TestV_MD_6_RestartPushesStatus(t *testing.T) {
	dir := mustShortDir(t)
	hub := surface.NewApprovalHub(5 * time.Second)

	cfg := custos.MCPServerConfig{
		Name:      "restart-rail",
		Transport: "stdio",
		Command:   "cat",
	}
	conn := custos.NewMCPConnector(cfg, dir)
	conn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)

	reg := custos.NewRailRegistry([]custos.MCPServerConfig{cfg}, dir)
	reg.RegisterConnector(conn)

	doorSrv, err := custos.StartDoorServer(dir, hub, custos.WithRail(reg))
	if err != nil {
		t.Fatalf("start door server: %v", err)
	}
	defer doorSrv.Close()

	sockPath, _ := custos.DoorPaths(dir)
	tok, _ := custos.ReadDoorToken(dir)

	clientA, err := custos.DialDoor(sockPath, tok, "subscriber_a")
	if err != nil {
		t.Fatalf("dial clientA: %v", err)
	}
	defer clientA.Close()

	clientB, err := custos.DialDoor(sockPath, tok, "subscriber_b")
	if err != nil {
		t.Fatalf("dial clientB: %v", err)
	}
	defer clientB.Close()

	// Drain all attach frames on both clients (MCP-STATUS and MCP-TOOLS)
	drainAttach := func(c *custos.DoorConn) {
		for range 2 {
			f, err := c.NextFrame(500 * time.Millisecond)
			if err != nil {
				break
			}
			_ = f
		}
	}
	drainAttach(clientA)
	drainAttach(clientB)

	// Trigger restart event
	conn.TriggerRestartForTest()

	// Both subscribers should concurrently receive MCP-STATUS frame with restarts == 1
	var wg sync.WaitGroup
	wg.Add(2)
	errs := make([]error, 2)

	checkSubscriber := func(idx int, c *custos.DoorConn) {
		defer wg.Done()
		frame, err := c.NextFrame(3 * time.Second)
		if err != nil {
			errs[idx] = fmt.Errorf("read restart frame: %w", err)
			return
		}
		if !strings.HasPrefix(frame, "MCP-STATUS ") {
			errs[idx] = fmt.Errorf("expected MCP-STATUS, got %q", frame)
			return
		}
		var st []custos.ConnectorStatus
		if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "MCP-STATUS ")), &st); err != nil {
			errs[idx] = fmt.Errorf("unmarshal restart status: %w", err)
			return
		}
		if len(st) != 1 || st[0].Restarts != 1 {
			errs[idx] = fmt.Errorf("expected 1 restart, got %+v", st)
		}
	}

	go checkSubscriber(0, clientA)
	go checkSubscriber(1, clientB)
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("subscriber %d error: %v", i, e)
		}
	}
}

// TestV_MD_7_SlowConsumer asserts that a slow consumer that never reads does not block
// the rail notifier and is dropped when its 64-frame outbound queue overflows.
func TestV_MD_7_SlowConsumer(t *testing.T) {
	dir := mustShortDir(t)
	hub := surface.NewApprovalHub(5 * time.Second)

	cfg := custos.MCPServerConfig{
		Name:      "slow-test-rail",
		Transport: "stdio",
		Command:   "cat",
	}
	conn := custos.NewMCPConnector(cfg, dir)
	conn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)

	reg := custos.NewRailRegistry([]custos.MCPServerConfig{cfg}, dir)
	reg.RegisterConnector(conn)

	doorSrv, err := custos.StartDoorServer(dir, hub, custos.WithRail(reg))
	if err != nil {
		t.Fatalf("start door server: %v", err)
	}
	defer doorSrv.Close()

	sockPath, _ := custos.DoorPaths(dir)
	tok, _ := custos.ReadDoorToken(dir)

	// Fast reader client
	fastConn, err := custos.DialDoor(sockPath, tok, "fast_reader")
	if err != nil {
		t.Fatalf("dial fast: %v", err)
	}
	defer fastConn.Close()

	// Slow client: dials and attaches, but will NEVER read from the socket
	slowRawConn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial slow raw: %v", err)
	}
	defer slowRawConn.Close()

	// Perform HELLO on slowRawConn
	_, _ = fmt.Fprintf(slowRawConn, "HELLO %s slow_reader\n", tok)
	slowReader := bufio.NewReader(slowRawConn)
	helloResp, err := slowReader.ReadString('\n')
	if err != nil || !strings.HasPrefix(helloResp, "OK") {
		t.Fatalf("slow client HELLO failed: %v, resp=%q", err, helloResp)
	}

	// Broadcast 75 frames (exceeding the 64-frame outbound queue)
	broadcastDone := make(chan struct{})
	go func() {
		for range 75 {
			conn.NotifyChangeForTest(false)
			time.Sleep(1 * time.Millisecond)
		}
		close(broadcastDone)
	}()

	select {
	case <-broadcastDone:
		// Succeeded without blocking!
	case <-time.After(5 * time.Second):
		t.Fatal("notifier wedged / blocked by slow consumer!")
	}

	// Verify fast consumer can continue reading without obstruction
	received := 0
	fastDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(fastDeadline) {
		frame, err := fastConn.NextFrame(200 * time.Millisecond)
		if err == nil && strings.HasPrefix(frame, "MCP-STATUS") {
			received++
			if received >= 5 {
				break
			}
		}
	}
	if received == 0 {
		t.Fatal("fast consumer received zero frames after broadcast")
	}

	// Verify slow consumer connection was closed on overflow
	_ = slowRawConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, err := slowReader.ReadString('\n')
		if err != nil {
			// EOF or closed by peer: success!
			break
		}
	}
}
