// Tests for the T2 R1 connector lifecycle on custosd: MCP-ADD / MCP-PROBE /
// MCP-REMOVE / MCP-STATE door verbs, runtime store with tombstones and boot
// merge, vault promotion, and last_used (CONNECTOR-LIFE-SPEC D1/D2/D6/D9).

package custos_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lararium-app/lararium/internal/custos"
)

const lcPass = "lc-test-passphrase-77" //nolint:gosec // G101: fixture canary passphrase

// TestLifecycleHelperProcess is the fixture child for lifecycle tests: a real
// MCP stdio server (tools alpha[read-only], beta) that records its pid and,
// optionally, the resolved secret env value it was composed with.
func TestLifecycleHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_LC_HELPER") != "1" {
		return
	}
	if p := os.Getenv("LC_PIDFILE"); p != "" {
		_ = os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0o600) //nolint:gosec // G703: helper-process paths are fixture input, not attacker input
	}
	if p := os.Getenv("LC_OUTFILE"); p != "" {
		_ = os.WriteFile(p, []byte(os.Getenv("LC_SECRET")), 0o600) //nolint:gosec // G703: helper-process paths are fixture input, not attacker input
	}
	switch os.Getenv("LC_MODE") {
	case "die":
		os.Exit(3)
	case "hang":
		select {}
	default:
		server := mcp.NewServer(&mcp.Implementation{Name: "lc-server", Version: "1.0"}, nil)
		mcp.AddTool(server, &mcp.Tool{
			Name:        "alpha",
			Description: "read only tool",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "alpha ok"}}}, nil, nil
		})
		mcp.AddTool(server, &mcp.Tool{
			Name:        "beta",
			Description: "write tool",
		}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "beta ok"}}}, nil, nil
		})
		_ = server.Run(context.Background(), &mcp.StdioTransport{})
		os.Exit(0)
	}
}

// lcConfig renders the single-line JSON config for the fixture child.
func lcConfig(t *testing.T, name string, env map[string]string, extra map[string]any) string {
	t.Helper()
	e := map[string]string{"GO_WANT_LC_HELPER": "1"}
	for k, v := range env {
		e[k] = v
	}
	cfg := map[string]any{
		"name":      name,
		"transport": "stdio",
		"command":   os.Args[0],
		"args":      []string{"-test.run=TestLifecycleHelperProcess", "--"},
		"env":       e,
	}
	for k, v := range extra {
		cfg[k] = v
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type lcHarness struct {
	t        *testing.T
	dir      string
	stateDir string
	d        *custos.Daemon
}

func newLCDaemon(t *testing.T, dir string, seed []custos.MCPServerConfig) *custos.Daemon {
	t.Helper()
	stateDir := filepath.Join(dir, "custos")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &custos.Config{}
	cfg.Connectors.MCP = seed
	return custos.NewDaemon(stateDir, cfg)
}

// startLC boots an unlocked daemon (vault init'd once) with a door server.
func startLC(t *testing.T, dir string, seed []custos.MCPServerConfig) *lcHarness {
	t.Helper()
	d := newLCDaemon(t, dir, seed)
	v := d.Vault()
	if !v.Exists() {
		if err := v.Init(lcPass); err != nil {
			t.Fatalf("vault init: %v", err)
		}
	}
	if err := v.Unlock(lcPass, false); err != nil {
		t.Fatalf("vault unlock: %v", err)
	}
	if err := d.Start(); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	h := &lcHarness{t: t, dir: dir, stateDir: filepath.Join(dir, "custos"), d: d}
	t.Cleanup(h.stop)
	return h
}

// stop simulates process death: daemon stopped and every rail child killed.
func (h *lcHarness) stop() {
	h.d.Stop()
	for _, c := range h.d.Rail().Connectors() {
		c.Shutdown(false)
	}
}

type lcClient struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	all  []string
}

func (h *lcHarness) dial() *lcClient {
	h.t.Helper()
	tok, err := custos.ReadDoorToken(h.stateDir)
	if err != nil {
		h.t.Fatal(err)
	}
	sock, _ := custos.DoorPaths(h.stateDir)
	c := &lcClient{t: h.t}
	c.conn, err = net.Dial("unix", sock)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = c.conn.Close() })
	c.r = bufio.NewReader(c.conn)
	if _, err := c.conn.Write([]byte("HELLO " + tok + " lc-test\n")); err != nil {
		h.t.Fatal(err)
	}
	line := c.readLine()
	if !strings.HasPrefix(line, "OK ") {
		h.t.Fatalf("hello: %q", line)
	}
	return c
}

func (c *lcClient) readLine() string {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	line, err := c.r.ReadString('\n')
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	c.all = append(c.all, line)
	return strings.TrimRight(line, "\r\n")
}

// send writes one verb line and returns the first reply frame (broadcast
// MCP-STATUS / MCP-TOOLS / card frames are skipped).
func (c *lcClient) send(line string) string {
	c.t.Helper()
	if strings.ContainsAny(line, "\r\n") {
		c.t.Fatalf("verb line must be one line: %q", line)
	}
	if _, err := c.conn.Write([]byte(line + "\n")); err != nil {
		c.t.Fatal(err)
	}
	for {
		got := c.readLine()
		if strings.HasPrefix(got, "MCP-STATUS ") || strings.HasPrefix(got, "MCP-TOOLS ") ||
			strings.HasPrefix(got, "CARD ") || strings.HasPrefix(got, "GONE ") {
			continue
		}
		return got
	}
}

func waitState(t *testing.T, c *custos.MCPConnector) custos.ConnectorStatus {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if st := c.Status(); st.State == custos.MCPStateReady {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("connector %s never reached ready (now %+v)", c.Name(), c.Status())
	return custos.ConnectorStatus{}
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("pidfile: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("pidfile content %q: %v", b, err)
	}
	return pid
}

func requireGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still alive", pid)
}

func readStore(t *testing.T, stateDir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(stateDir, "connectors.json"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("store json: %v", err)
	}
	return m
}

func storeNames(t *testing.T, stateDir string) (conns []string, tombs []string) {
	t.Helper()
	m := readStore(t, stateDir)
	connList, ok := m["connectors"].([]any)
	if !ok {
		t.Fatalf("connectors key is %T, want []any", m["connectors"])
	}
	for _, c := range connList {
		rec, ok := c.(map[string]any)
		if !ok {
			t.Fatalf("connector entry %v is not an object", c)
		}
		nm, ok := rec["name"].(string)
		if !ok {
			t.Fatalf("connector name %v is not a string", rec["name"])
		}
		conns = append(conns, nm)
	}
	tombList, ok := m["tombstones"].([]any)
	if !ok {
		t.Fatalf("tombstones key is %T, want []any", m["tombstones"])
	}
	for _, n := range tombList {
		tb, ok := n.(string)
		if !ok {
			t.Fatalf("tombstone %v is not a string", n)
		}
		tombs = append(tombs, tb)
	}
	return conns, tombs
}

func TestLC_AuthRefusalAndGrammar(t *testing.T) {
	dir := shortGuardDir(t)
	h := startLC(t, dir, nil)

	// Wrong token: refused, and the verbs are never served on that session.
	sock, _ := custos.DoorPaths(h.stateDir)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	_, _ = conn.Write([]byte("HELLO not-the-token intruder\n"))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, _ := r.ReadString('\n'); strings.TrimSpace(line) != "ERR bad_token" {
		t.Fatalf("bad token hello = %q", line)
	}
	_, _ = conn.Write([]byte("MCP-ADD x " + lcConfig(t, "x", nil, nil) + "\n"))
	if line, err := r.ReadString('\n'); err == nil && strings.HasPrefix(line, "OK") {
		t.Fatalf("unauthenticated MCP-ADD answered %q", line)
	}
	if h.d.Rail().Connector("x") != nil {
		t.Fatal("unauthenticated add registered a connector")
	}

	c := h.dial()
	pidFile := filepath.Join(t.TempDir(), "pid")
	good := lcConfig(t, "gram", map[string]string{"LC_PIDFILE": pidFile}, nil)
	cases := []struct{ line, want string }{
		{"MCP-ADD", "ERR bad_request"},
		{"MCP-ADD gram", "ERR bad_request"},
		{"MCP-ADD gram notjson", "ERR bad_request"},
		{"MCP-ADD gram []", "ERR bad_request"},
		{"MCP-PROBE", "ERR bad_request"},
		{"MCP-REMOVE", "ERR bad_request"},
		{"MCP-REMOVE a b", "ERR bad_request"},
		{"MCP-STATE", "ERR bad_request"},
		{"MCP-STATE gram", "ERR bad_request"},
		{"MCP-STATE gram sideways", "ERR bad_request"},
		{"MCP-STATE ghost enabled", "ERR not_found"},
		{"MCP-STATE ghost disabled", "ERR not_found"},
		// strictness identical to the YAML lane
		{"MCP-ADD gram " + lcConfig(t, "gram", map[string]string{"LC_PIDFILE": pidFile}, map[string]any{"bogus": 1}), "ERR unknown_key"},
		{"MCP-ADD gram {\"name\":\"gram\",\"transport\":\"stdio\",\"command\":5}", "ERR unknown_key"},
		{"MCP-ADD Bad_Name " + lcConfig(t, "Bad_Name", nil, nil), "ERR bad_name"},
		{"MCP-ADD gram " + lcConfig(t, "other", nil, nil), "ERR bad_name"},
		{"MCP-ADD gmail " + lcConfig(t, "gmail", nil, nil), "ERR reserved_name"},
		{"MCP-ADD custos " + lcConfig(t, "custos", nil, nil), "ERR reserved_name"},
		{"MCP-ADD gram " + lcConfig(t, "gram", nil, map[string]any{"transport": "carrier-pigeon"}), "ERR bad_transport"},
		{"MCP-ADD gram " + lcConfig(t, "gram", nil, map[string]any{"command": "relative/bin"}), "ERR bad_command"},
		{"MCP-ADD gram " + lcConfig(t, "gram", nil, map[string]any{"max_concurrent_calls": 99}), "ERR bad_max_concurrent"},
		{"MCP-ADD gram {\"name\":\"gram\",\"transport\":\"http\",\"url\":\"http://example.com/mcp\"}", "ERR bad_url"},
		{"MCP-PROBE " + lcConfig(t, "gram", map[string]string{"LC_PIDFILE": pidFile}, map[string]any{"bogus": 1}), "ERR unknown_key"},
		{"MCP-PROBE " + lcConfig(t, "Bad", nil, nil), "ERR bad_name"},
	}
	for _, tc := range cases {
		if got := c.send(tc.line); got != tc.want {
			t.Errorf("%.60q => %q, want %q", tc.line, got, tc.want)
		}
	}
	if _, err := os.Stat(pidFile); err == nil {
		t.Fatal("rejected config spawned a child")
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, "connectors.json")); err == nil {
		t.Fatal("rejected ADD wrote the store")
	}
	if n := len(h.d.Rail().Connectors()); n != 0 {
		t.Fatalf("rejected ADD registered %d connectors", n)
	}
	// every frame we ever got is exactly one line (one-frame-per-line law)
	for _, l := range c.all {
		if strings.Count(l, "\n") != 1 || !strings.HasSuffix(l, "\n") {
			t.Fatalf("frame %q violates one-frame-per-line", l)
		}
	}
	_ = good
}

func TestLC_AddReplaceRemove(t *testing.T) {
	dir := shortGuardDir(t)
	h := startLC(t, dir, nil)
	c := h.dial()
	pidFile := filepath.Join(t.TempDir(), "pid")

	if got := c.send("MCP-ADD alpha-conn " + lcConfig(t, "alpha-conn", map[string]string{"LC_PIDFILE": pidFile}, nil)); got != "OK" {
		t.Fatalf("add = %q", got)
	}
	conn := h.d.Rail().Connector("alpha-conn")
	if conn == nil {
		t.Fatal("not registered")
	}
	st := waitState(t, conn)
	if st.Tools != 2 {
		t.Fatalf("tools = %d", st.Tools)
	}
	pid1 := readPid(t, pidFile)
	names, tombs := storeNames(t, h.stateDir)
	if len(names) != 1 || names[0] != "alpha-conn" || len(tombs) != 0 {
		t.Fatalf("store = %v / %v", names, tombs)
	}
	fi, err := os.Stat(filepath.Join(h.stateDir, "connectors.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("store mode: %v %v", fi, err)
	}

	// Replace: same name, new shape (deny-list beta). Old child must be gone
	// before the replacement runs.
	cfg2 := lcConfig(t, "alpha-conn", map[string]string{"LC_PIDFILE": pidFile}, map[string]any{"tool_denylist": []string{"beta"}})
	if got := c.send("MCP-ADD alpha-conn " + cfg2); got != "OK" {
		t.Fatalf("replace = %q", got)
	}
	requireGone(t, pid1)
	conn2 := h.d.Rail().Connector("alpha-conn")
	if conn2 == conn {
		t.Fatal("replace kept the old connector")
	}
	if st := waitState(t, conn2); st.Tools != 1 {
		t.Fatalf("replaced tools = %d", st.Tools)
	}
	if pid2 := readPid(t, pidFile); pid2 == pid1 {
		t.Fatal("replacement did not spawn a new child")
	}
	if n := len(h.d.Rail().Connectors()); n != 1 {
		t.Fatalf("registry has %d connectors after replace", n)
	}

	// Remove: gone from the registry, tombstoned, idempotent.
	pid2 := readPid(t, pidFile)
	if got := c.send("MCP-REMOVE alpha-conn"); got != "OK" {
		t.Fatalf("remove = %q", got)
	}
	requireGone(t, pid2)
	if h.d.Rail().Connector("alpha-conn") != nil {
		t.Fatal("still registered after remove")
	}
	names, tombs = storeNames(t, h.stateDir)
	if len(names) != 0 || len(tombs) != 1 || tombs[0] != "alpha-conn" {
		t.Fatalf("store after remove = %v / %v", names, tombs)
	}
	before, _ := os.ReadFile(filepath.Join(h.stateDir, "connectors.json"))
	for _, n := range []string{"alpha-conn", "never-existed"} {
		if got := c.send("MCP-REMOVE " + n); got != "OK" {
			t.Fatalf("idempotent remove(%s) = %q", n, got)
		}
	}
	after, _ := os.ReadFile(filepath.Join(h.stateDir, "connectors.json"))
	if string(before) != string(after) {
		t.Fatalf("idempotent remove changed the store:\n%s\n%s", before, after)
	}

	// Re-adding a tombstoned name clears the tombstone.
	if got := c.send("MCP-ADD alpha-conn " + cfg2); got != "OK" {
		t.Fatalf("re-add = %q", got)
	}
	_, tombs = storeNames(t, h.stateDir)
	if len(tombs) != 0 {
		t.Fatalf("tombstone survived re-add: %v", tombs)
	}
}

func TestLC_AuditKinds(t *testing.T) {
	dir := shortGuardDir(t)
	h := startLC(t, dir, nil)
	c := h.dial()
	cfg := lcConfig(t, "aud", map[string]string{"LC_SECRET": "super-secret-audit-value"}, nil)
	if got := c.send("MCP-ADD aud " + cfg); got != "OK" {
		t.Fatal(got)
	}
	waitState(t, h.d.Rail().Connector("aud"))
	if got := c.send("MCP-STATE aud disabled"); got != "OK" {
		t.Fatal(got)
	}
	if got := c.send("MCP-REMOVE aud"); got != "OK" {
		t.Fatal(got)
	}
	var kinds []string
	var added custos.AuditRecord
	err := filepath.Walk(filepath.Join(h.stateDir, "audit"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p) //nolint:gosec // G122: sweep of a test-owned tmpdir; no symlink race outside the fixture
		for _, ln := range strings.Split(string(b), "\n") {
			var rec custos.AuditRecord
			if json.Unmarshal([]byte(ln), &rec) != nil || rec.Cred != "aud" {
				continue
			}
			kinds = append(kinds, rec.Kind)
			if rec.Kind == custos.AuditKindMCPAdded {
				added = rec
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(kinds, ",")
	for _, k := range []string{custos.AuditKindMCPAdded, custos.AuditKindMCPToggled, custos.AuditKindMCPRemoved} {
		if !strings.Contains(joined, k) {
			t.Errorf("audit kinds %s missing %s", joined, k)
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(added.Fingerprint) {
		t.Errorf("mcp_added fingerprint = %q", added.Fingerprint)
	}
	if !strings.Contains(strings.Join(added.Names, ","), "LC_SECRET") {
		t.Errorf("mcp_added names = %v, want env NAMES", added.Names)
	}
	for _, k := range []string{custos.AuditKindMCPEdited, custos.AuditKindMCPAddDenied} {
		if k == "" {
			t.Fatal("kind constant empty")
		}
	}
}

func TestLC_ProbeSuccessLeavesNoTrace(t *testing.T) {
	dir := shortGuardDir(t)
	h := startLC(t, dir, nil)
	c := h.dial()
	pidFile := filepath.Join(t.TempDir(), "pid")
	cfg := lcConfig(t, "probed", map[string]string{"LC_PIDFILE": pidFile}, nil)

	if _, err := os.Stat(filepath.Join(h.stateDir, "connectors.json")); err == nil {
		t.Fatal("store exists before probe")
	}
	reply := c.send("MCP-PROBE " + cfg)
	m := regexp.MustCompile(`^OK (\d+) ([0-9a-f]{64})$`).FindStringSubmatch(reply)
	if m == nil || m[1] != "2" {
		t.Fatalf("probe reply = %q", reply)
	}
	frame := c.readLine()
	if !strings.HasPrefix(frame, "MCP-TOOLS _probe ") {
		t.Fatalf("probe frame = %q", frame)
	}
	var tools []struct {
		Name  string `json:"name"`
		Flags string `json:"flags"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "MCP-TOOLS _probe ")), &tools); err != nil || len(tools) != 2 {
		t.Fatalf("probe tools = %q (%v)", frame, err)
	}
	flags := map[string]string{}
	for _, tl := range tools {
		flags[tl.Name] = tl.Flags
	}
	if flags["alpha"] != "read_only" || flags["beta"] != "none" {
		t.Fatalf("flags = %v", flags)
	}

	requireGone(t, readPid(t, pidFile)) // child killed + reaped
	if h.d.Rail().Connector("probed") != nil || len(h.d.Rail().Connectors()) != 0 {
		t.Fatal("probe registered a connector")
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, "connectors.json")); err == nil {
		t.Fatal("probe wrote the store")
	}

	// Probe fingerprint equals the fingerprint of the same config once added.
	if got := c.send("MCP-ADD probed " + cfg); got != "OK" {
		t.Fatal(got)
	}
	conn := h.d.Rail().Connector("probed")
	waitState(t, conn)
	if conn.Fingerprint() != m[2] {
		t.Fatalf("fingerprint probe=%s add=%s", m[2], conn.Fingerprint())
	}
}

func TestLC_ProbeBootFailureReapsChild(t *testing.T) {
	dir := shortGuardDir(t)
	h := startLC(t, dir, nil)
	c := h.dial()

	pidDie := filepath.Join(t.TempDir(), "pid-die")
	got := c.send("MCP-PROBE " + lcConfig(t, "dies", map[string]string{"LC_PIDFILE": pidDie, "LC_MODE": "die"}, nil))
	if !strings.HasPrefix(got, "ERR ") || strings.Contains(got, "\n") {
		t.Fatalf("die probe = %q", got)
	}
	requireGone(t, readPid(t, pidDie))

	pidHang := filepath.Join(t.TempDir(), "pid-hang")
	start := time.Now()
	got = c.send("MCP-PROBE " + lcConfig(t, "hangs", map[string]string{"LC_PIDFILE": pidHang, "LC_MODE": "hang"},
		map[string]any{"list_timeout": "1s"}))
	if got != "ERR list_timeout" {
		t.Fatalf("hang probe = %q", got)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("hang probe took %v", time.Since(start))
	}
	requireGone(t, readPid(t, pidHang))

	if len(h.d.Rail().Connectors()) != 0 {
		t.Fatal("failed probe registered a connector")
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, "connectors.json")); err == nil {
		t.Fatal("failed probe wrote the store")
	}
}

func TestLC_PromotionNeverLeaksLiteral(t *testing.T) {
	const secret = "sk-promote-LITERAL-9d41c7e2f0" //nolint:gosec // G101: fixture canary secret
	const bearerSecret = "bearer-LITERAL-55aa77cc33ee"
	t.Setenv("LC_PROMO_TEST_REF", "env-ref-value")
	dir := shortGuardDir(t)
	h := startLC(t, dir, nil)
	c := h.dial()
	outFile := filepath.Join(t.TempDir(), "child-secret")
	pidFile := filepath.Join(t.TempDir(), "pid")
	cfg := lcConfig(t, "promo", map[string]string{
		"LC_SECRET":  secret,
		"LC_OUTFILE": outFile,
		"LC_PIDFILE": pidFile,
		"FROM_ENV":   "${env:LC_PROMO_TEST_REF}",
	}, nil)

	if got := c.send("MCP-ADD promo " + cfg); got != "OK" {
		t.Fatalf("add = %q", got)
	}
	conn := h.d.Rail().Connector("promo")
	waitState(t, conn)

	// Composed child sees the resolved value, identically to a hand-written ref.
	if b, _ := os.ReadFile(outFile); string(b) != secret {
		t.Fatalf("child saw %q, want resolved secret", b)
	}

	// Store holds a ${vault:} ref; the vault holds the value.
	m := readStore(t, h.stateDir)
	rec, ok := m["connectors"].([]any)[0].(map[string]any)
	if !ok {
		t.Fatalf("connector[0] is not an object: %v", m["connectors"])
	}
	env, ok := rec["env"].(map[string]any)
	if !ok {
		t.Fatalf("connector env is not an object: %v", rec["env"])
	}
	ref, _ := env["LC_SECRET"].(string)
	mm := regexp.MustCompile(`^\$\{vault:([a-z0-9][a-z0-9_-]{0,63})\}$`).FindStringSubmatch(ref)
	if mm == nil {
		t.Fatalf("stored LC_SECRET = %q, want ${vault:name}", ref)
	}
	cred, ok := h.d.Vault().GetCredential(mm[1])
	if !ok || cred.Secret != secret {
		t.Fatalf("vault credential %q missing or wrong", mm[1])
	}
	if env["FROM_ENV"] != "${env:LC_PROMO_TEST_REF}" {
		t.Fatalf("env ref was rewritten: %v", env["FROM_ENV"])
	}

	// Idempotent: same literal, same derived name, byte-identical store, no growth.
	storeBefore, _ := os.ReadFile(filepath.Join(h.stateDir, "connectors.json"))
	credCount := len(h.d.Vault().LoadedCredentials())
	if got := c.send("MCP-ADD promo " + cfg); got != "OK" {
		t.Fatalf("re-add = %q", got)
	}
	storeAfter, _ := os.ReadFile(filepath.Join(h.stateDir, "connectors.json"))
	if string(storeBefore) != string(storeAfter) {
		t.Fatalf("re-ADD changed store:\n%s\n%s", storeBefore, storeAfter)
	}
	if n := len(h.d.Vault().LoadedCredentials()); n != credCount {
		t.Fatalf("re-ADD grew vault %d -> %d", credCount, n)
	}

	// http bearer literal is promoted too.
	httpCfg := fmt.Sprintf(`{"name":"web","transport":"http","url":"http://127.0.0.1:1/mcp","bearer":%q,"list_timeout":"1s"}`, bearerSecret)
	if got := c.send("MCP-ADD web " + httpCfg); got != "OK" {
		t.Fatalf("http add = %q", got)
	}
	m = readStore(t, h.stateDir)
	var webRec map[string]any
	connList, listOK := m["connectors"].([]any)
	if !listOK {
		t.Fatalf("connectors key is %T, want []any", m["connectors"])
	}
	for _, e := range connList {
		if rec, ok := e.(map[string]any); ok && rec["name"] == "web" {
			webRec = rec
		}
	}
	bref, _ := webRec["bearer"].(string)
	bm := regexp.MustCompile(`^\$\{vault:([a-z0-9][a-z0-9_-]{0,63})\}$`).FindStringSubmatch(bref)
	if bm == nil {
		t.Fatalf("stored bearer = %q", bref)
	}
	if bc, ok := h.d.Vault().GetCredential(bm[1]); !ok || bc.Secret != bearerSecret {
		t.Fatal("bearer not in vault")
	}
	// probe of a config with literals does not promote anything.
	nCreds := len(h.d.Vault().LoadedCredentials())
	_ = c.send("MCP-PROBE " + lcConfig(t, "probe-lit", map[string]string{"LC_SECRET": "probe-only-LITERAL-1234567"}, nil))
	_ = c.readLine()
	if n := len(h.d.Vault().LoadedCredentials()); n != nCreds {
		t.Fatalf("probe touched the vault (%d -> %d)", nCreds, n)
	}
	h.d.Rail().Connector("web").Shutdown(false)

	// Nothing under the hearth dir (store, audit, vault, snapshots, stderr
	// captures) and no reply frame carries a literal, raw or base64.
	for _, lit := range []string{secret, bearerSecret, "probe-only-LITERAL-1234567"} {
		needles := []string{
			lit, base64.StdEncoding.EncodeToString([]byte(lit)), base64.RawURLEncoding.EncodeToString([]byte(lit)),
			base64.URLEncoding.EncodeToString([]byte(lit)),
		}
		for off := 1; off < 3; off++ { // base64 of the literal at other alignments
			enc := base64.RawStdEncoding.EncodeToString([]byte(strings.Repeat("x", off) + lit))
			needles = append(needles, enc[4:len(enc)-4])
		}
		err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || info.Mode()&os.ModeSocket != 0 {
				return nil //nolint:nilerr // sweep ignores unreadable entries by design
			}
			b, rerr := os.ReadFile(p) //nolint:gosec // G122: sweep of a test-owned tmpdir; no symlink race outside the fixture
			if rerr != nil {
				return nil //nolint:nilerr // sweep ignores unreadable entries by design
			}
			for _, n := range needles {
				if strings.Contains(string(b), n) {
					t.Errorf("%s contains literal secret (%.8s…)", p, n)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range c.all {
			for _, n := range needles {
				if strings.Contains(l, n) {
					t.Errorf("reply frame leaks literal: %.80q", l)
				}
			}
		}
	}
}

func TestLC_AddRefusedWhileLockedAndNoPartialState(t *testing.T) {
	dir := shortGuardDir(t)
	h := startLC(t, dir, nil)
	c := h.dial()
	if err := h.d.Vault().Lock(); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	if got := c.send("MCP-ADD locked-one " + lcConfig(t, "locked-one", map[string]string{"LC_PIDFILE": pidFile, "LC_SECRET": "locked-LITERAL-0099aabb"}, nil)); got != "ERR locked" {
		t.Fatalf("locked add = %q", got)
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, "connectors.json")); err == nil {
		t.Fatal("locked add wrote the store")
	}
	if _, err := os.Stat(pidFile); err == nil {
		t.Fatal("locked add spawned a child")
	}
	if len(h.d.Rail().Connectors()) != 0 {
		t.Fatal("locked add registered a connector")
	}
}

func TestLC_StateTogglePersistsAcrossRestart(t *testing.T) {
	dir := shortGuardDir(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	h := startLC(t, dir, nil)
	c := h.dial()
	if got := c.send("MCP-ADD tog " + lcConfig(t, "tog", map[string]string{"LC_PIDFILE": pidFile}, nil)); got != "OK" {
		t.Fatal(got)
	}
	conn := h.d.Rail().Connector("tog")
	waitState(t, conn)
	pid := readPid(t, pidFile)

	if got := c.send("MCP-STATE tog disabled"); got != "OK" {
		t.Fatal(got)
	}
	st := conn.Status()
	if st.State != custos.MCPStateDisabled || st.Reason != custos.MCPReasonUserDisabled {
		t.Fatalf("status after disable = %+v", st)
	}
	requireGone(t, pid)
	if err := conn.ManualRefresh(context.Background()); err == nil {
		t.Fatal("ManualRefresh restarted a user-disabled connector")
	}
	if got := c.send("MCP-STATE tog disabled"); got != "OK" {
		t.Fatalf("disable is not idempotent: %q", got)
	}

	// Simulated restart over the same state dir: stays disabled, never started.
	h.stop()
	_ = os.Remove(pidFile)
	h2 := startLC(t, dir, nil)
	conn2 := h2.d.Rail().Connector("tog")
	if conn2 == nil {
		t.Fatal("stored connector missing after restart")
	}
	time.Sleep(300 * time.Millisecond) // first-unlock hook runs async
	if st := conn2.Status(); st.State != custos.MCPStateDisabled || st.Reason != custos.MCPReasonUserDisabled {
		t.Fatalf("after restart = %+v", st)
	}
	if _, err := os.Stat(pidFile); err == nil {
		t.Fatal("disabled connector was started at boot")
	}

	c2 := h2.dial()
	if got := c2.send("MCP-STATE tog enabled"); got != "OK" {
		t.Fatal(got)
	}
	waitState(t, conn2)

	// And enabled persists across another restart.
	h2.stop()
	h3 := startLC(t, dir, nil)
	waitState(t, h3.d.Rail().Connector("tog"))
}

func TestLC_TombstoneBeatsYAMLSeedAcrossRestart(t *testing.T) {
	dir := shortGuardDir(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	seed := []custos.MCPServerConfig{{
		Name:      "seeded",
		Transport: "stdio",
		Command:   os.Args[0],
		Args:      []string{"-test.run=TestLifecycleHelperProcess", "--"},
		Env:       map[string]string{"GO_WANT_LC_HELPER": "1", "LC_PIDFILE": pidFile},
	}}
	h := startLC(t, dir, seed)
	waitState(t, h.d.Rail().Connector("seeded"))
	c := h.dial()

	// YAML seed can be toggled without copying its config into the store.
	if got := c.send("MCP-STATE seeded disabled"); got != "OK" {
		t.Fatal(got)
	}
	raw, _ := os.ReadFile(filepath.Join(h.stateDir, "connectors.json"))
	if strings.Contains(string(raw), "command") {
		t.Fatalf("state overlay copied the seed config into the store: %s", raw)
	}
	if got := c.send("MCP-REMOVE seeded"); got != "OK" {
		t.Fatal(got)
	}
	h.stop()

	h2 := startLC(t, dir, seed)
	if h2.d.Rail().Connector("seeded") != nil {
		t.Fatal("tombstoned YAML seed came back")
	}
}

func TestLC_StoreBootMergeMatrix(t *testing.T) {
	seed := func(names ...string) []custos.MCPServerConfig {
		var out []custos.MCPServerConfig
		for _, n := range names {
			out = append(out, custos.MCPServerConfig{Name: n, Transport: "stdio", Command: "/bin/true", Env: map[string]string{"FROM": "yaml"}})
		}
		return out
	}
	open := func(content string) *custos.ConnectorStore {
		d := t.TempDir()
		if content != "" {
			if err := os.WriteFile(filepath.Join(d, "connectors.json"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		st, err := custos.OpenConnectorStore(d)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return st
	}
	names := func(m []custos.MergedConnector) string {
		var s []string
		for _, e := range m {
			s = append(s, e.Config.Name)
		}
		return strings.Join(s, ",")
	}

	// YAML only (no store file)
	if got := open("").Merge(seed("a", "b")); names(got) != "a,b" || !got[0].Enabled {
		t.Fatalf("yaml only = %v", got)
	}

	// store overrides YAML by name, carries enabled + last_used; store-only appended
	st := open(`{"connectors":[` +
		`{"name":"a","transport":"stdio","command":"/bin/false","env":{"FROM":"store"},"enabled":false,"last_used":"2026-10-31T12:00:00Z"},` +
		`{"name":"c","transport":"http","url":"http://127.0.0.1:9/","enabled":true,"last_used":null}` +
		`],"tombstones":[]}`)
	got := st.Merge(seed("a", "b"))
	if names(got) != "a,b,c" {
		t.Fatalf("override merge = %s", names(got))
	}
	if got[0].Config.Command != "/bin/false" || got[0].Config.Env["FROM"] != "store" || got[0].Enabled {
		t.Fatalf("store did not override a: %+v", got[0])
	}
	if got[0].LastUsed == nil || got[0].LastUsed.UTC().Format(time.RFC3339) != "2026-10-31T12:00:00Z" {
		t.Fatalf("last_used not carried: %v", got[0].LastUsed)
	}
	if got[1].Config.Env["FROM"] != "yaml" || !got[1].Enabled {
		t.Fatalf("b should stay seed: %+v", got[1])
	}

	// tombstone drops a YAML seed (and a store entry of the same name)
	st = open(`{"connectors":[{"name":"c","transport":"http","url":"http://127.0.0.1:9/","enabled":true,"last_used":null}],"tombstones":["a","c"]}`)
	if got := st.Merge(seed("a", "b")); names(got) != "b" {
		t.Fatalf("tombstone merge = %s", names(got))
	}

	// corrupt store refuses (open error + Daemon.Start refusal)
	for name, content := range map[string]string{
		"garbage":       "{not json",
		"unknown field": `{"connectors":[],"tombstones":[],"extra":1}`,
		"bad name":      `{"connectors":[{"name":"Bad Name","enabled":true,"last_used":null}],"tombstones":[]}`,
		"bad transport": `{"connectors":[{"name":"x","transport":"pigeon","enabled":true,"last_used":null}],"tombstones":[]}`,
		"bad tombstone": `{"connectors":[],"tombstones":["BAD NAME"]}`,
		"bad last_used": `{"connectors":[{"name":"x","transport":"stdio","enabled":true,"last_used":"yesterday"}],"tombstones":[]}`,
	} {
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, "connectors.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := custos.OpenConnectorStore(d); !errors.Is(err, custos.ErrConnectorStoreCorrupt) {
			t.Errorf("%s: open err = %v, want ErrConnectorStoreCorrupt", name, err)
		}
	}
	dir := shortGuardDir(t)
	stateDir := filepath.Join(dir, "custos")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("{ tombstones: [oops")
	if err := os.WriteFile(filepath.Join(stateDir, "connectors.json"), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	d := custos.NewDaemon(stateDir, &custos.Config{})
	err := d.Start()
	if err == nil || !strings.Contains(err.Error(), "connector store") {
		t.Fatalf("corrupt store Start err = %v, want refusal", err)
	}
	if got, _ := os.ReadFile(filepath.Join(stateDir, "connectors.json")); string(got) != string(corrupt) {
		t.Fatal("corrupt store was overwritten/discarded")
	}
}

func TestLC_StoreDeterministicAndSeedYAMLUntouched(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "lararium.yaml")
	yamlBody := []byte("custos:\n  connectors:\n    mcp: []\n")
	if err := os.WriteFile(yamlPath, yamlBody, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := custos.OpenConnectorStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"zeta", "alpha", "mid"} {
		st.Put(custos.RecordFromConfig(custos.MCPServerConfig{Name: n, Transport: "stdio", Command: "/bin/true", Env: map[string]string{"Z": "${env:Z}", "A": "${env:A}"}}))
	}
	st.Remove("old")
	st.Remove("ancient")
	if err := st.Save(nil, nil); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(dir, "connectors.json"))
	if err := st.Save(nil, nil); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(dir, "connectors.json"))
	if string(first) != string(second) {
		t.Fatal("store marshal not deterministic")
	}
	if !strings.Contains(string(first), `"tombstones":["ancient","old"]`) {
		t.Fatalf("tombstones not sorted: %s", first)
	}
	if strings.Index(string(first), `"name":"alpha"`) > strings.Index(string(first), `"name":"mid"`) ||
		strings.Index(string(first), `"name":"mid"`) > strings.Index(string(first), `"name":"zeta"`) {
		t.Fatalf("connectors not sorted: %s", first)
	}
	if strings.Index(string(first), `"A":`) > strings.Index(string(first), `"Z":`) {
		t.Fatalf("env keys not stable: %s", first)
	}
	fi, _ := os.Stat(filepath.Join(dir, "connectors.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode())
	}
	if got, _ := os.ReadFile(yamlPath); string(got) != string(yamlBody) {
		t.Fatal("lararium.yaml was modified")
	}
	if ents, _ := filepath.Glob(filepath.Join(dir, "connectors-*.tmp")); len(ents) != 0 {
		t.Fatalf("temp files left behind: %v", ents)
	}
}

func TestLC_LastUsedAdditiveAndPersisted(t *testing.T) {
	dir := shortGuardDir(t)
	h := startLC(t, dir, nil)
	c := h.dial()
	if got := c.send("MCP-ADD used " + lcConfig(t, "used", nil, nil)); got != "OK" {
		t.Fatal(got)
	}
	conn := h.d.Rail().Connector("used")
	waitState(t, conn)

	raw, rerr := json.Marshal(conn.Status())
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(string(raw), "last_used") {
		t.Fatalf("unused connector status carries last_used: %s", raw)
	}
	if _, err := conn.Execute(context.Background(), "alpha", json.RawMessage(`{}`), custos.Credential{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	raw, mErr := json.Marshal(conn.Status())
	if mErr != nil {
		t.Fatal(mErr)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	lu, _ := row["last_used"].(string)
	ts, err := time.Parse(time.RFC3339, lu)
	if err != nil || time.Since(ts) > time.Minute {
		t.Fatalf("last_used = %q (%v)", lu, err)
	}
	for _, k := range []string{"name", "state", "tools", "restarts", "reason"} { // existing keys unchanged
		if _, ok := row[k]; !ok {
			t.Fatalf("status row lost key %s: %s", k, raw)
		}
	}

	// Persisted on the next store write; restored at boot.
	if got := c.send("MCP-STATE used disabled"); got != "OK" {
		t.Fatal(got)
	}
	if got := c.send("MCP-STATE used enabled"); got != "OK" {
		t.Fatal(got)
	}
	h.stop()
	h2 := startLC(t, dir, nil)
	conn2 := h2.d.Rail().Connector("used")
	if conn2 == nil || conn2.Status().LastUsed == nil || *conn2.Status().LastUsed != lu {
		t.Fatalf("last_used not restored: %+v", conn2.Status())
	}
}

func TestLC_StatusFrameBroadcastOnRemove(t *testing.T) {
	dir := shortGuardDir(t)
	h := startLC(t, dir, nil)
	c := h.dial()
	if got := c.send("MCP-ADD gone " + lcConfig(t, "gone", nil, nil)); got != "OK" {
		t.Fatal(got)
	}
	waitState(t, h.d.Rail().Connector("gone"))
	watcher := h.dial()
	if got := c.send("MCP-REMOVE gone"); got != "OK" {
		t.Fatal(got)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = watcher.conn.SetReadDeadline(deadline)
		line, err := watcher.r.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "MCP-STATUS ") && !strings.Contains(line, `"gone"`) {
			return
		}
	}
	t.Fatal("no MCP-STATUS broadcast without the removed connector")
}
