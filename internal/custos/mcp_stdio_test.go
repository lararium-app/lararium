package custos_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lararium-app/lararium/internal/custos"
)

// TestMCPHelperProcess is a subprocess helper for I1, I2, I7 stdio probes.
func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_HELPER") != "1" {
		return
	}

	mode := os.Getenv("CUSTOS_HELPER_MODE")
	switch mode {
	case "dump_environ":
		// I1: dump /proc/self/environ or os.Environ()
		for _, e := range os.Environ() {
			// Ignore internal Go test flags
			if !strings.HasPrefix(e, "GO_") && !strings.HasPrefix(e, "CUSTOS_HELPER_") {
				fmt.Println(e)
			}
		}
		os.Exit(0)

	case "echo_sentinel":
		// I2: echo sentinel secret to stdout and stderr
		sentinel := os.Getenv("SENTINEL_SECRET")
		fmt.Fprintf(os.Stderr, "child stderr sentinel: %s\n", sentinel)
		fmt.Printf("child stdout sentinel: %s\n", sentinel)
		os.Exit(0)

	case "grandchild":
		// I7: spawn grandchild process and report its pid via stdout
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to start grandchild: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("GRANDCHILD_PID=%d\n", cmd.Process.Pid)
		// Wait indefinitely until killed
		select {}

	case "mcp_server":
		// Real MCP server running over stdio
		server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0"}, nil)
		mcp.AddTool(server, &mcp.Tool{
			Name:        "read_file",
			Description: "read file contents",
		}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			path, _ := args["path"].(string)
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "contents of " + path}},
			}, nil, nil
		})
		_ = server.Run(context.Background(), &mcp.StdioTransport{})
		os.Exit(0)

	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode: %s\n", mode)
		os.Exit(2)
	}
}

// I1: Exact env probe per MCP-SPEC §6, §11 I1, §12 V16a.
func TestI1_ExactEnvProbe(t *testing.T) {
	tmpDir := t.TempDir()
	connectorDir := filepath.Join(tmpDir, "connectors", "myconn")
	_ = os.MkdirAll(connectorDir, 0o700)

	cfg := &custos.MCPServerConfig{
		Name:    "myconn",
		Command: os.Args[0],
		Env: map[string]string{
			"API_KEY": "${vault:secret_key}",
			"MY_ENV":  "${env:HOST_VAR}",
			"LITERAL": "lit_val",
		},
	}

	resolver := func(ref string) (string, error) {
		if ref == "${vault:secret_key}" {
			return "vault_secret_12345", nil
		}
		if ref == "${env:HOST_VAR}" {
			return "host_resolved_val", nil
		}
		return "", fmt.Errorf("unknown ref: %s", ref)
	}

	childEnv, needles, literals, err := custos.ComposeChildEnv(cfg, connectorDir, resolver)
	if err != nil {
		t.Fatalf("ComposeChildEnv failed: %v", err)
	}

	needleSet := make(map[string]bool)
	for _, n := range needles {
		needleSet[n] = true
	}
	if len(needles) != 2 || !needleSet["vault_secret_12345"] || !needleSet["host_resolved_val"] {
		t.Fatalf("expected 2 needles (vault and env), got: %v", needles)
	}
	if len(literals) != 1 || literals[0] != "LITERAL" {
		t.Fatalf("expected 1 literal, got: %v", literals)
	}

	// Add helper env flags
	execEnv := append(append([]string(nil), childEnv...), "GO_WANT_MCP_HELPER=1", "CUSTOS_HELPER_MODE=dump_environ")

	var stdout bytes.Buffer
	child, r, _, err := custos.SpawnChildProcess(
		os.Args[0],
		[]string{"-test.run=TestMCPHelperProcess", "--"},
		execEnv,
		connectorDir,
		&stdout,
		nil,
	)
	if err != nil {
		t.Fatalf("SpawnChildProcess failed: %v", err)
	}
	defer func() { _ = child.KillGroup(time.Second) }()

	scanner := bufio.NewScanner(r)
	var dumpedLines []string
	for scanner.Scan() {
		dumpedLines = append(dumpedLines, scanner.Text())
	}
	_ = child.KillGroup(time.Second)

	dumpedMap := make(map[string]string)
	for _, l := range dumpedLines {
		parts := strings.SplitN(l, "=", 2)
		if len(parts) == 2 {
			dumpedMap[parts[0]] = parts[1]
		}
	}

	// Must contain exactly: HOME, TMPDIR, PATH, API_KEY, MY_ENV, LITERAL
	wantKeys := []string{"HOME", "TMPDIR", "PATH", "API_KEY", "MY_ENV", "LITERAL"}
	for _, k := range wantKeys {
		if _, ok := dumpedMap[k]; !ok {
			t.Errorf("missing expected env key %q in child env: %v", k, dumpedMap)
		}
	}

	if dumpedMap["HOME"] != connectorDir {
		t.Errorf("HOME = %q, want %q", dumpedMap["HOME"], connectorDir)
	}
	if dumpedMap["TMPDIR"] != connectorDir {
		t.Errorf("TMPDIR = %q, want %q", dumpedMap["TMPDIR"], connectorDir)
	}
	if !strings.Contains(dumpedMap["PATH"], filepath.Dir(os.Args[0])) {
		t.Errorf("PATH %q does not contain dirname(command)", dumpedMap["PATH"])
	}
	if dumpedMap["API_KEY"] != "vault_secret_12345" {
		t.Errorf("API_KEY = %q, want %q", dumpedMap["API_KEY"], "vault_secret_12345")
	}

	// Must NOT contain daemon env variables
	forbiddenKeys := []string{"USER", "SHELL", "SSH_AUTH_SOCK", "LOGNAME"}
	for _, fk := range forbiddenKeys {
		if os.Getenv(fk) != "" {
			if _, exists := dumpedMap[fk]; exists {
				t.Errorf("forbidden daemon env key %q leaked into child env", fk)
			}
		}
	}
}

// I2: Scrub probe on child stderr capture per MCP-SPEC §6, §11 I2, §12 V16b.
func TestI2_ScrubProbe(t *testing.T) {
	tmpDir := t.TempDir()
	connectorDir := filepath.Join(tmpDir, "connectors", "myconn")
	_ = os.MkdirAll(connectorDir, 0o700)

	sentinel := "super-secret-sentinel-vault-value"
	needles := []string{sentinel}

	rotWriter, err := custos.NewRotatingStderrWriter(connectorDir, needles)
	if err != nil {
		t.Fatalf("NewRotatingStderrWriter failed: %v", err)
	}
	defer rotWriter.Close()

	env := []string{
		"GO_WANT_MCP_HELPER=1",
		"CUSTOS_HELPER_MODE=echo_sentinel",
		"SENTINEL_SECRET=" + sentinel,
	}

	child, _, _, err := custos.SpawnChildProcess(
		os.Args[0],
		[]string{"-test.run=TestMCPHelperProcess", "--"},
		env,
		connectorDir,
		rotWriter,
		rotWriter,
	)
	if err != nil {
		t.Fatalf("SpawnChildProcess failed: %v", err)
	}

	// Wait for process to exit
	select {
	case <-child.WaitChan():
	case <-time.After(5 * time.Second):
		_ = child.KillGroup(time.Second)
		t.Fatalf("child process timed out")
	}
	_ = rotWriter.Close()

	// Read stderr.log
	stderrPath := filepath.Join(connectorDir, "stderr.log")
	b, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatalf("read stderr.log: %v", err)
	}
	content := string(b)

	// Must NOT contain raw sentinel
	if strings.Contains(content, sentinel) {
		t.Fatalf("stderr file leaked raw sentinel secret: %q", content)
	}
	// Must contain [redacted]
	if !strings.Contains(content, "[redacted]") {
		t.Fatalf("stderr file missing [redacted]: %q", content)
	}
}

// TestEnvNeedleScrub tests that ${env:} secrets become needles and are scrubbed
// across stderr, review cards, and tool results per Finding 1.
//
//nolint:gosec // test mock env secrets
func TestEnvNeedleScrub(t *testing.T) {
	secretVal := "super-secret-env-needle-9999"
	t.Setenv("SECRET_NAME", secretVal)

	tmpDir := t.TempDir()
	connectorDir := filepath.Join(tmpDir, "connectors", "myconn")
	_ = os.MkdirAll(connectorDir, 0o700)

	cfg := &custos.MCPServerConfig{
		Name:    "myconn",
		Command: os.Args[0],
		Env: map[string]string{
			"SENTINEL_SECRET": "${env:SECRET_NAME}",
		},
	}

	childEnv, needles, _, err := custos.ComposeChildEnv(cfg, connectorDir, nil)
	if err != nil {
		t.Fatalf("ComposeChildEnv failed: %v", err)
	}

	if len(needles) != 1 || needles[0] != secretVal {
		t.Fatalf("expected needle %q, got: %v", secretVal, needles)
	}

	// 1. Stderr sink: test RotatingStderrWriter with actual child process echoing the env secret
	rotWriter, err := custos.NewRotatingStderrWriter(connectorDir, needles)
	if err != nil {
		t.Fatalf("NewRotatingStderrWriter failed: %v", err)
	}
	defer rotWriter.Close()

	env := append(append([]string(nil), childEnv...),
		"GO_WANT_MCP_HELPER=1",
		"CUSTOS_HELPER_MODE=echo_sentinel",
	)

	child, _, _, err := custos.SpawnChildProcess(
		os.Args[0],
		[]string{"-test.run=TestMCPHelperProcess", "--"},
		env,
		connectorDir,
		rotWriter,
		rotWriter,
	)
	if err != nil {
		t.Fatalf("SpawnChildProcess failed: %v", err)
	}

	select {
	case <-child.WaitChan():
	case <-time.After(5 * time.Second):
		_ = child.KillGroup(time.Second)
		t.Fatalf("child process timed out")
	}
	_ = rotWriter.Close()

	stderrPath := filepath.Join(connectorDir, "stderr.log")
	b, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatalf("read stderr.log: %v", err)
	}
	content := string(b)
	if strings.Contains(content, secretVal) {
		t.Fatalf("stderr file leaked raw env secret: %q", content)
	}
	if !strings.Contains(content, "[redacted]") {
		t.Fatalf("stderr file missing [redacted]: %q", content)
	}

	// 2. Review cards sink: BuildReviewLine
	args := json.RawMessage(fmt.Sprintf(`{"token":"%s"}`, secretVal))
	review := custos.BuildReviewLine(args, false, needles)
	if strings.Contains(review, secretVal) {
		t.Fatalf("review card leaked raw env secret: %q", review)
	}
	if !strings.Contains(review, "[redacted]") {
		t.Fatalf("review card missing [redacted]: %q", review)
	}

	// 3. Tool results sink: ScrubString
	scrubbed := custos.ScrubString("tool result with secret: "+secretVal, needles)
	if strings.Contains(scrubbed, secretVal) {
		t.Fatalf("tool result leaked raw env secret: %q", scrubbed)
	}
	if !strings.Contains(scrubbed, "[redacted]") {
		t.Fatalf("tool result missing [redacted]: %q", scrubbed)
	}
}

// I7: Grandchild kill on group termination per MCP-SPEC §7, §11 I7, §12 V16d.
func TestI7_GrandchildKill(t *testing.T) {
	tmpDir := t.TempDir()
	connectorDir := filepath.Join(tmpDir, "connectors", "myconn")
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

	// Read grandchild pid
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
		t.Fatalf("failed to read grandchild PID from child process")
	}

	// Verify grandchild is initially running
	if err := syscall.Kill(grandchildPid, 0); err != nil {
		t.Fatalf("grandchild process %d not running: %v", grandchildPid, err)
	}

	// Kill the child's process group with short grace
	_ = child.KillGroup(500 * time.Millisecond)

	// Verify grandchild is DEAD (ESRCH)
	// Give kernel a moment to reap
	time.Sleep(100 * time.Millisecond)
	err = syscall.Kill(grandchildPid, 0)
	if err == nil {
		t.Fatalf("grandchild process %d still alive after group kill!", grandchildPid)
	}
	if !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("expected ESRCH for grandchild, got: %v", err)
	}
}

// V14: Stderr rotation 1 MiB x 3 per MCP-SPEC §6, §12 V14.
func TestV14_StderrRotation(t *testing.T) {
	tmpDir := t.TempDir()
	connectorDir := filepath.Join(tmpDir, "connectors", "myconn")
	_ = os.MkdirAll(connectorDir, 0o700)

	sentinel := "sentinel-vault-12345"
	w, err := custos.NewRotatingStderrWriter(connectorDir, []string{sentinel})
	if err != nil {
		t.Fatalf("NewRotatingStderrWriter: %v", err)
	}

	// Write ~3.5 MiB of stderr churn containing the sentinel
	chunk := strings.Repeat("A", 1000) + " " + sentinel + "\n"
	iterations := 3500 // ~3.5 MiB total
	for range iterations {
		_, err := w.Write([]byte(chunk))
		if err != nil {
			t.Fatalf("write chunk: %v", err)
		}
	}
	_ = w.Close()

	// Assert rotating files <= 3
	entries, err := os.ReadDir(connectorDir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	var stderrFiles []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "stderr.log") {
			stderrFiles = append(stderrFiles, e.Name())
		}
	}

	if len(stderrFiles) > 3 {
		t.Fatalf("expected <= 3 stderr files, got %d: %v", len(stderrFiles), stderrFiles)
	}

	// Verify sentinel was scrubbed across all files
	for _, fname := range stderrFiles {
		b, err := os.ReadFile(filepath.Join(connectorDir, fname))
		if err != nil {
			t.Fatalf("read %s: %v", fname, err)
		}
		if bytes.Contains(b, []byte(sentinel)) {
			t.Fatalf("file %s leaked raw sentinel: %s", fname, string(b[:100]))
		}
		if !bytes.Contains(b, []byte("[redacted]")) {
			t.Fatalf("file %s missing [redacted]", fname)
		}
	}
}
