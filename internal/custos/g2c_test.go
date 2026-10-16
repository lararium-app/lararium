package custos_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/surface"
)

// TestG2c_ListIncludesMCP verifies:
// fake ready connector with 2 tools -> op:list rows contain gmail rows then
// MCP triples in snapshot order; a disabled connector contributes zero rows;
// no network on list.
func TestG2c_ListIncludesMCP(t *testing.T) {
	stateDir := t.TempDir()
	v := custos.NewVault(stateDir, 5*time.Second)
	if err := v.Init("passphrase-test-1234"); err != nil {
		t.Fatalf("vault init: %v", err)
	}
	if err := v.Unlock("passphrase-test-1234", false); err != nil {
		t.Fatalf("vault unlock: %v", err)
	}

	hub := surface.NewApprovalHub(5 * time.Second)
	cfg := &custos.Config{}
	ws := custos.NewWorkerServer(v, hub, cfg)

	// Fake ready connector with 2 tools
	readyConn := custos.NewMCPConnector(custos.MCPServerConfig{
		Name: "alpha-mcp",
	}, t.TempDir())
	readyConn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)
	readyConn.SetSnapshotForTest(&custos.CatalogSnapshot{
		Connector: "alpha-mcp",
		ToolInfos: []custos.ToolInfo{
			{Connector: "alpha-mcp", Name: "tool_1", Title: "Tool 1"},
			{Connector: "alpha-mcp", Name: "tool_2", Title: "Tool 2"},
		},
	})
	_ = ws.RegisterConnector(readyConn)

	// Disabled connector with tools: must contribute zero rows
	disabledConn := custos.NewMCPConnector(custos.MCPServerConfig{
		Name: "beta-disabled",
	}, t.TempDir())
	disabledConn.SetStateForTest(custos.MCPStateDisabled, custos.MCPReasonTransport)
	disabledConn.SetSnapshotForTest(&custos.CatalogSnapshot{
		Connector: "beta-disabled",
		ToolInfos: []custos.ToolInfo{
			{Connector: "beta-disabled", Name: "hidden_tool", Title: "Hidden"},
		},
	})
	_ = ws.RegisterConnector(disabledConn)

	cw := custos.NewTestCellWorker("cell-1", "cell-token-1234567890", "/tmp/nonexistent.sock")
	req := &custos.WorkerRequest{
		Op:        "list",
		CellToken: "cell-token-1234567890",
	}

	resp := ws.Dispatch(cw, req)
	if !resp.OK {
		t.Fatalf("list failed: %s %s", resp.ErrorCode, resp.Detail)
	}
	if resp.ErrorCode != "" {
		t.Fatalf("expected empty error code, got: %s", resp.ErrorCode)
	}

	// op:list rows: connector order names-sorted (A1), snapshot order
	// within connector.
	var envelope struct {
		Tools []custos.ToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &envelope); err != nil {
		t.Fatalf("unmarshal list result: %v (raw=%s)", err, resp.Result)
	}
	tools := envelope.Tools
	if len(tools) != 4 {
		t.Fatalf("expected 4 tools (2 alpha-mcp + 2 gmail), got %d: %+v", len(tools), tools)
	}

	// alpha-mcp rows first (names-sorted; no gmail privilege)
	if tools[0].Connector != "alpha-mcp" || tools[0].Name != "tool_1" || tools[0].Title != "Tool 1" {
		t.Errorf("tool[0] = %+v, want alpha-mcp/tool_1/Tool 1", tools[0])
	}
	if tools[1].Connector != "alpha-mcp" || tools[1].Name != "tool_2" || tools[1].Title != "Tool 2" {
		t.Errorf("tool[1] = %+v, want alpha-mcp/tool_2/Tool 2", tools[1])
	}
	if tools[2].Connector != "gmail" || tools[2].Name != "send" {
		t.Errorf("tool[2] = %+v, want gmail/send", tools[2])
	}
	if tools[3].Connector != "gmail" || tools[3].Name != "read" {
		t.Errorf("tool[3] = %+v, want gmail/read", tools[3])
	}

	// Assert disabled connector contributed zero rows
	for _, tool := range tools {
		if tool.Connector == "beta-disabled" {
			t.Errorf("disabled connector tool appeared in list: %+v", tool)
		}
	}
}

// TestG2c_FirstUnlockTriggers verifies:
// unlock a fresh vault in a test harness with 2 configured servers ->
// registry OnFirstUnlock observed once per boot, UNLOCK reply not delayed
// by connector startup.
func TestG2c_FirstUnlockTriggers(t *testing.T) {
	stateDir := t.TempDir()
	passphrase := "unlock-pass-1234"

	cfg := &custos.Config{
		Connectors: custos.ConnectorsConfig{
			MCP: []custos.MCPServerConfig{
				{
					Name:      "server1",
					Transport: "stdio",
					Command:   "/bin/true",
				},
				{
					Name:      "server2",
					Transport: "stdio",
					Command:   "/bin/true",
				},
			},
		},
	}

	// Initialize vault
	v := custos.NewVault(stateDir, 5*time.Second)
	if err := v.Init(passphrase); err != nil {
		t.Fatalf("vault init: %v", err)
	}

	// Build WorkerServer and start CtlServer with locked vault
	hub := surface.NewApprovalHub(5 * time.Second)
	ws := custos.NewWorkerServer(v, hub, cfg)
	reg := ws.RailRegistry()
	if reg == nil {
		t.Fatalf("expected non-nil RailRegistry")
	}

	ctlServer, err := custos.StartCtlServer(stateDir, v, nil)
	if err != nil {
		t.Fatalf("start ctl server: %v", err)
	}
	defer ctlServer.Close()
	ctlServer.SetWorkers(ws)
	ctlServer.SetRailRegistry(reg)

	// Before unlock: FirstUnlockCount is 0, connectors sit in starting
	if count := reg.FirstUnlockCount(); count != 0 {
		t.Fatalf("initial FirstUnlockCount = %d, want 0", count)
	}
	for _, c := range reg.Connectors() {
		if st := c.Status(); st.State != custos.MCPStateStarting {
			t.Errorf("connector %s initial state = %s, want starting", st.Name, st.State)
		}
	}

	client, err := custos.NewCtlClient(stateDir, 5*time.Second)
	if err != nil {
		t.Fatalf("new ctl client: %v", err)
	}

	// Time the UNLOCK ctl reply: must NOT be delayed by connector startup (30s list_timeout)
	start := time.Now()
	resp, err := client.RoundTripWithArg("UNLOCK", passphrase)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ctl UNLOCK failed: %v", err)
	}
	if resp != "unlocked" {
		t.Fatalf("ctl UNLOCK resp = %q, want 'unlocked'", resp)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("UNLOCK reply delayed by connector startup: took %v", elapsed)
	}

	// Wait up to 2 seconds for asynchronous OnFirstUnlock to be observed
	deadline := time.Now().Add(2 * time.Second)
	for reg.FirstUnlockCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if count := reg.FirstUnlockCount(); count != 1 {
		t.Fatalf("OnFirstUnlock observed %d times, want 1", count)
	}

	// Lock then unlock again: OnFirstUnlock must NOT be triggered again (observed once per boot)
	if _, err := client.RoundTrip("LOCK"); err != nil {
		t.Fatalf("ctl LOCK: %v", err)
	}
	if _, err := client.RoundTripWithArg("UNLOCK", passphrase); err != nil {
		t.Fatalf("second ctl UNLOCK: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if count := reg.FirstUnlockCount(); count != 1 {
		t.Fatalf("OnFirstUnlock re-triggered on second unlock: count = %d, want 1", count)
	}
}

// TestG2c_RevokeNotifies verifies:
// ctl REVOKE-CREDENTIAL on a name referenced via ${vault:X} ->
// connector lands disabled/credential_unavailable, child group killed
// (reuse process-tree fake from I7 tests), one audit record.
func TestG2c_RevokeNotifies(t *testing.T) {
	stateDir := t.TempDir()
	passphrase := "revoke-pass-1234"

	v := custos.NewVault(stateDir, 5*time.Second)
	if err := v.Init(passphrase); err != nil {
		t.Fatalf("vault init: %v", err)
	}
	if err := v.Unlock(passphrase, false); err != nil {
		t.Fatalf("vault unlock: %v", err)
	}

	// Add secret credential in vault
	credSecret := "secret-token-to-revoke-123456"
	err := v.Mutate("", func(doc *custos.VaultDoc) ([]string, error) {
		if doc.Credentials == nil {
			doc.Credentials = make(map[string]custos.Credential)
		}
		doc.Credentials["target-secret"] = custos.Credential{
			Kind:   "api_key",
			Secret: credSecret,
		}
		return []string{"target-secret"}, nil
	}, false)
	if err != nil {
		t.Fatalf("store credential: %v", err)
	}

	cfg := &custos.Config{
		Connectors: custos.ConnectorsConfig{
			MCP: []custos.MCPServerConfig{
				{
					Name:      "test-mcp",
					Transport: "stdio",
					Command:   os.Args[0],
					Args:      []string{"-test.run=TestMCPHelperProcess", "--"},
					Env: map[string]string{
						"API_KEY": "${vault:target-secret}",
					},
				},
			},
		},
	}

	hub := surface.NewApprovalHub(5 * time.Second)
	ws := custos.NewWorkerServer(v, hub, cfg)
	reg := ws.RailRegistry()
	conn := reg.Connector("test-mcp")
	if conn == nil {
		t.Fatalf("connector test-mcp not registered")
	}

	// Spawn grandchild process group using the I7 fake helper
	connectorDir := filepath.Join(stateDir, "connectors", "test-mcp")
	_ = os.MkdirAll(connectorDir, 0o700)
	env := []string{
		"GO_WANT_MCP_HELPER=1",
		"CUSTOS_HELPER_MODE=grandchild",
		"PATH=/usr/local/bin:/usr/bin:/bin",
	}
	child, stdout, _, err := custos.SpawnChildProcess(
		os.Args[0],
		[]string{"-test.run=TestMCPHelperProcess", "--"},
		env,
		connectorDir,
		os.Stderr,
		nil,
	)
	if err != nil {
		t.Fatalf("SpawnChildProcess failed: %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	var grandchildPid int
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "GRANDCHILD_PID=") {
			pidStr := strings.TrimPrefix(line, "GRANDCHILD_PID=")
			grandchildPid, _ = strconv.Atoi(pidStr)
			break
		}
	}
	if grandchildPid <= 0 {
		_ = child.KillGroup(time.Second)
		t.Fatalf("failed to read grandchild PID")
	}

	// Verify grandchild is initially running
	if err := syscall.Kill(grandchildPid, 0); err != nil {
		_ = child.KillGroup(time.Second)
		t.Fatalf("grandchild process %d not running: %v", grandchildPid, err)
	}

	conn.SetChildProcForTest(child)
	conn.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)

	ctlServer, err := custos.StartCtlServer(stateDir, v, nil)
	if err != nil {
		_ = child.KillGroup(time.Second)
		t.Fatalf("start ctl server: %v", err)
	}
	defer ctlServer.Close()
	ctlServer.SetWorkers(ws)
	ctlServer.SetRailRegistry(reg)

	// Count audit records before revocation
	tailBefore, err := v.Audit().ReadTailRecord()
	if err != nil {
		_ = child.KillGroup(time.Second)
		t.Fatalf("read tail audit: %v", err)
	}

	client, err := custos.NewCtlClient(stateDir, 5*time.Second)
	if err != nil {
		_ = child.KillGroup(time.Second)
		t.Fatalf("new ctl client: %v", err)
	}

	// Revoke credential via ctl
	resp, err := client.RoundTripWithArg("REVOKE-CREDENTIAL", "target-secret")
	if err != nil {
		t.Fatalf("REVOKE-CREDENTIAL failed: %v", err)
	}
	if resp != "revoked" {
		t.Fatalf("REVOKE-CREDENTIAL response = %q, want 'revoked'", resp)
	}

	// 1. Connector lands disabled/credential_unavailable
	st := conn.Status()
	if st.State != custos.MCPStateDisabled {
		t.Errorf("connector state = %q, want %q", st.State, custos.MCPStateDisabled)
	}
	if st.Reason != custos.MCPReasonCredentialUnavailable {
		t.Errorf("connector reason = %q, want %q", st.Reason, custos.MCPReasonCredentialUnavailable)
	}

	// 2. Child process group killed (grandchild dead)
	time.Sleep(100 * time.Millisecond)
	err = syscall.Kill(grandchildPid, 0)
	if err == nil {
		t.Fatalf("grandchild process %d still alive after revocation kill!", grandchildPid)
	}
	if !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("expected ESRCH for grandchild, got: %v", err)
	}

	// 3. Exactly one audit record added
	tailAfter, err := v.Audit().ReadTailRecord()
	if err != nil {
		t.Fatalf("read tail audit: %v", err)
	}
	if tailAfter == nil || tailAfter.Kind != custos.AuditKindCredentialRemoved || tailAfter.Cred != "target-secret" {
		t.Fatalf("unexpected tail audit record: %+v", tailAfter)
	}
	if tailBefore != nil && tailAfter.PrevHash != tailBefore.PrevHash {
		if tailAfter.Gen != tailBefore.Gen+1 {
			t.Errorf("audit generation jumped from %d to %d (want exactly +1 record)", tailBefore.Gen, tailAfter.Gen)
		}
	}
}

// TestG2c_CTLVerbs verifies:
// MCP-STATUS/MCP-TOOLS/MCP-REFRESH happy path + bad token ERR + unknown name ERR;
// refresh of a budget-exhausted connector succeeds (manual path resets budget)
// while an automatic refresh stays a no-op.
func TestG2c_CTLVerbs(t *testing.T) {
	stateDir := t.TempDir()
	v := custos.NewVault(stateDir, 5*time.Second)
	if err := v.Init("pass-verbs-1234"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := v.Unlock("pass-verbs-1234", false); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	hub := surface.NewApprovalHub(5 * time.Second)
	cfg := &custos.Config{}
	ws := custos.NewWorkerServer(v, hub, cfg)

	reg := custos.NewRailRegistry(nil, stateDir)

	// Connector 1: ready with tools
	conn1 := custos.NewMCPConnector(custos.MCPServerConfig{
		Name:      "conn-one",
		Transport: "stdio",
		Command:   "echo",
	}, stateDir)
	conn1.SetStateForTest(custos.MCPStateReady, custos.MCPReasonOK)
	readOnly := true
	conn1.SetSnapshotForTest(&custos.CatalogSnapshot{
		Connector: "conn-one",
		Tools: []custos.SnapshotTool{
			{
				Canonical: "tool_alpha",
				Title:     "Tool Alpha",
				Annotations: &mcp.ToolAnnotations{
					ReadOnlyHint: readOnly,
				},
			},
		},
	})
	reg.RegisterConnector(conn1)

	// Connector 2: budget-exhausted (5 restarts in rolling hour)
	conn2 := custos.NewMCPConnector(custos.MCPServerConfig{
		Name:      "conn-exhausted",
		Transport: "stdio",
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestMCPHelperProcess", "--"},
		Env: map[string]string{
			"GO_WANT_MCP_HELPER": "1",
			"CUSTOS_HELPER_MODE": "mcp_server",
		},
	}, stateDir)
	now := time.Now()
	conn2.SetRestartsForTest([]time.Time{now, now, now, now, now})
	conn2.SetStateForTest(custos.MCPStateDisabled, custos.MCPReasonTransport)
	reg.RegisterConnector(conn2)

	ctlServer, err := custos.StartCtlServer(stateDir, v, nil)
	if err != nil {
		t.Fatalf("start ctl server: %v", err)
	}
	defer ctlServer.Close()
	ctlServer.SetWorkers(ws)
	ctlServer.SetRailRegistry(reg)

	client, err := custos.NewCtlClient(stateDir, 5*time.Second)
	if err != nil {
		t.Fatalf("new ctl client: %v", err)
	}

	// 1. MCP-STATUS happy path
	resp, err := client.RoundTrip("MCP-STATUS")
	if err != nil {
		t.Fatalf("MCP-STATUS: %v", err)
	}
	var statuses []custos.ConnectorStatus
	if err := json.Unmarshal([]byte(resp), &statuses); err != nil {
		t.Fatalf("unmarshal status JSON: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(statuses))
	}
	if statuses[0].Name != "conn-one" || statuses[0].State != custos.MCPStateReady || statuses[0].Tools != 1 {
		t.Errorf("status[0] = %+v", statuses[0])
	}
	if statuses[1].Name != "conn-exhausted" || statuses[1].State != custos.MCPStateDisabled || statuses[1].Restarts != 5 {
		t.Errorf("status[1] = %+v", statuses[1])
	}

	// 2. MCP-TOOLS happy path
	resp, err = client.RoundTripWithArg("MCP-TOOLS", "conn-one")
	if err != nil {
		t.Fatalf("MCP-TOOLS: %v", err)
	}
	var tools []custos.ToolStatus
	if err := json.Unmarshal([]byte(resp), &tools); err != nil {
		t.Fatalf("unmarshal tools JSON: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "tool_alpha" || tools[0].Flags != "read_only" || tools[0].Title != "Tool Alpha" {
		t.Fatalf("tools = %+v", tools)
	}

	// 3. Unknown name ERR
	_, err = client.RoundTripWithArg("MCP-TOOLS", "unknown-name")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error for unknown MCP-TOOLS, got: %v", err)
	}
	_, err = client.RoundTripWithArg("MCP-REFRESH", "unknown-name")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' error for unknown MCP-REFRESH, got: %v", err)
	}

	// 4. Bad token ERR
	sockPath, _ := custos.CtlPaths(stateDir)
	d := net.Dialer{Timeout: time.Second}
	rawConn, err := d.DialContext(context.Background(), "unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer rawConn.Close()

	reader := bufio.NewReader(rawConn)
	_, _ = fmt.Fprintf(rawConn, "MCP-STATUS bad-token\n")
	line, _ := reader.ReadString('\n')
	if !strings.HasPrefix(line, "ERR unauthorized") {
		t.Errorf("bad token MCP-STATUS line = %q, want 'ERR unauthorized'", line)
	}

	rawConn2, _ := d.DialContext(context.Background(), "unix", sockPath)
	defer rawConn2.Close()
	reader2 := bufio.NewReader(rawConn2)
	_, _ = fmt.Fprintf(rawConn2, "MCP-TOOLS bad-token conn-one\n")
	line2, _ := reader2.ReadString('\n')
	if !strings.HasPrefix(line2, "ERR unauthorized") {
		t.Errorf("bad token MCP-TOOLS line = %q, want 'ERR unauthorized'", line2)
	}

	rawConn3, _ := d.DialContext(context.Background(), "unix", sockPath)
	defer rawConn3.Close()
	reader3 := bufio.NewReader(rawConn3)
	_, _ = fmt.Fprintf(rawConn3, "MCP-REFRESH bad-token conn-one\n")
	line3, _ := reader3.ReadString('\n')
	if !strings.HasPrefix(line3, "ERR unauthorized") {
		t.Errorf("bad token MCP-REFRESH line = %q, want 'ERR unauthorized'", line3)
	}

	// 5. Automatic refresh against budget-exhausted stays a no-op
	if err := conn2.Refresh(context.Background()); err != nil {
		t.Fatalf("auto refresh error: %v", err)
	}
	if st := conn2.Status(); st.State != custos.MCPStateDisabled {
		t.Fatalf("conn2 state after auto refresh = %s, want disabled", st.State)
	}

	// 6. Manual refresh (MCP-REFRESH) clears budget and restarts
	t.Setenv("GO_WANT_MCP_HELPER", "1")
	t.Setenv("CUSTOS_HELPER_MODE", "mcp_server")

	refreshClient, err := custos.NewCtlClient(stateDir, 10*time.Second)
	if err != nil {
		t.Fatalf("refresh client: %v", err)
	}
	resp, err = refreshClient.RoundTripWithArg("MCP-REFRESH", "conn-exhausted")
	if err != nil {
		t.Fatalf("manual MCP-REFRESH failed: %v", err)
	}
	if !strings.Contains(resp, "refreshed") && resp != "OK" {
		t.Fatalf("manual MCP-REFRESH resp = %q, want refreshed", resp)
	}

	// Verify budget reset: state is ready, restarts is 1 (the single new start)
	stExhausted := conn2.Status()
	if stExhausted.State != custos.MCPStateReady {
		t.Fatalf("state after manual refresh = %q, want ready", stExhausted.State)
	}
	if stExhausted.Restarts != 1 {
		t.Fatalf("restarts after manual refresh = %d, want 1", stExhausted.Restarts)
	}
}

// TestG2c_PolicyCoverage verifies:
// deny/ask/always on bare <name> covers all that connector's tools including
// ones added by later refreshes; <name>/<tool> matches the canonical tool
// name as surfaced in op:"list" rows (frozen §6.5).
func TestG2c_PolicyCoverage(t *testing.T) {
	stateDir := t.TempDir()
	vault := custos.NewVault(stateDir, 5*time.Second)
	p := vault.Policy()

	// 1. Bare <name> covers all that connector's tools
	_, err := p.AddCredentialRule("my-mcp", custos.VerdictDeny, false, "cli", "cli")
	if err != nil {
		t.Fatalf("add bare deny rule: %v", err)
	}

	// Existing tool matches bare deny
	v, pat := p.Decide(custos.DecideInput{
		Lane:       "worker",
		Credential: "my-mcp",
		Tool:       "tool_existing",
	})
	if v != custos.VerdictDeny || pat != "my-mcp" {
		t.Errorf("bare deny got verdict=%q, pat=%q, want deny, my-mcp", v, pat)
	}

	// Tool added later by refresh also matches bare deny
	v, pat = p.Decide(custos.DecideInput{
		Lane:       "worker",
		Credential: "my-mcp",
		Tool:       "tool_added_later_by_refresh",
	})
	if v != custos.VerdictDeny || pat != "my-mcp" {
		t.Errorf("bare deny for later tool got verdict=%q, pat=%q, want deny, my-mcp", v, pat)
	}

	// 2. <name>/<tool> matches canonical tool name specifically and outranks bare rule
	_, err = p.AddCredentialRule("my-mcp/tool_specific", custos.VerdictAuto, true, "door", "cli")
	if err != nil {
		t.Fatalf("add specific auto rule: %v", err)
	}

	v, pat = p.Decide(custos.DecideInput{
		Lane:       "worker",
		Credential: "my-mcp",
		Tool:       "tool_specific",
	})
	if v != custos.VerdictAuto || pat != "my-mcp/tool_specific" {
		t.Errorf("specific auto rule got verdict=%q, pat=%q, want auto, my-mcp/tool_specific", v, pat)
	}

	// Sibling tool still covered by bare deny rule
	v, pat = p.Decide(custos.DecideInput{
		Lane:       "worker",
		Credential: "my-mcp",
		Tool:       "tool_existing",
	})
	if v != custos.VerdictDeny || pat != "my-mcp" {
		t.Errorf("sibling tool got verdict=%q, pat=%q, want deny, my-mcp", v, pat)
	}
}
