package main_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/surface"
)

func buildCustosCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "custos")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build custos: %v\n%s", err, out)
	}
	return bin
}

func TestCustosCLI(t *testing.T) {
	bin := buildCustosCLI(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	_ = os.MkdirAll(home, 0o700)

	cfgPath := filepath.Join(dir, "lararium.yaml")
	cfgContent := "hearth:\n  home: " + home + "\n"
	_ = os.WriteFile(cfgPath, []byte(cfgContent), 0o600)

	pass := "cli-passphrase-1234"

	// 1. custos init
	cmd := exec.Command(bin, "--config", cfgPath, "init")
	cmd.Stdin = strings.NewReader(pass + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("init failed: %v, stderr: %s", err, stderr.String())
	}

	// Second init must fail with "vault exists"
	cmd = exec.Command(bin, "--config", cfgPath, "init")
	cmd.Stdin = strings.NewReader(pass + "\n")
	stderr.Reset()
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("second init expected error, got nil")
	}
	if !strings.Contains(stderr.String(), "vault exists") {
		t.Errorf("stderr = %q, want 'vault exists'", stderr.String())
	}

	// 2. custos status (locked)
	cmd = exec.Command(bin, "--config", cfgPath, "status")
	stdout.Reset()
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("status failed: %v", err)
	}
	var st custos.Status
	if err := json.Unmarshal(stdout.Bytes(), &st); err != nil {
		t.Fatalf("parse status JSON: %v (raw: %s)", err, stdout.String())
	}
	if st.State != custos.StateLocked {
		t.Errorf("state = %s, want locked", st.State)
	}

	// 3. custos audit verify
	cmd = exec.Command(bin, "--config", cfgPath, "audit", "verify")
	stdout.Reset()
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("audit verify failed: %v", err)
	}
	if !strings.Contains(stdout.String(), "chain ok") {
		t.Errorf("audit verify output = %q, want prefix 'chain ok'", stdout.String())
	}

	// 4. custos unlock
	cmd = exec.Command(bin, "--config", cfgPath, "unlock")
	cmd.Stdin = strings.NewReader(pass + "\n")
	stdout.Reset()
	stderr.Reset()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("unlock failed: %v (stderr: %s)", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "unlocked") {
		t.Errorf("unlock output = %q, want 'unlocked'", stdout.String())
	}

	// 5. custos lock
	cmd = exec.Command(bin, "--config", cfgPath, "lock")
	stdout.Reset()
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("lock failed: %v", err)
	}

	// 6. custos snapshots list
	cmd = exec.Command(bin, "--config", cfgPath, "snapshots", "list")
	stdout.Reset()
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("snapshots list failed: %v", err)
	}
	if !strings.Contains(stdout.String(), "generation") {
		t.Errorf("snapshots list output = %q, want header 'generation'", stdout.String())
	}

	// 7. custos change-passphrase
	newPass := "new-cli-pass-5678" //nolint:gosec // test passphrase
	cmd = exec.Command(bin, "--config", cfgPath, "change-passphrase")
	cmd.Stdin = strings.NewReader(pass + "\n" + newPass + "\n")
	stdout.Reset()
	stderr.Reset()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("change-passphrase failed: %v (stderr: %s)", err, stderr.String())
	}

	// Verify old passphrase fails and new passphrase succeeds
	cmd = exec.Command(bin, "--config", cfgPath, "unlock")
	cmd.Stdin = strings.NewReader(pass + "\n")
	if err := cmd.Run(); err == nil {
		t.Fatal("unlock with old passphrase should fail")
	}

	cmd = exec.Command(bin, "--config", cfgPath, "unlock")
	cmd.Stdin = strings.NewReader(newPass + "\n")
	if err := cmd.Run(); err != nil {
		t.Fatalf("unlock with new passphrase failed: %v", err)
	}
}

func runCmd(t *testing.T, bin string, stdin string, args ...string) (stdout string, stderr string, exitCode int) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	cmd := exec.Command(bin, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exitCode = 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("run command %v: %v", args, err)
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

func testV14PolicyOps(t *testing.T, bin, cfgPath string) {
	t.Helper()
	// 3. Policy add tests & Domination note (§6.1, §10 V14, V24)
	// Add cdn.tracker.com/* auto
	_, stderr, code := runCmd(t, bin, "", "--config", cfgPath, "policy", "add", "egress", "cdn.tracker.com/*", "auto")
	if code != 0 {
		t.Fatalf("policy add egress cdn.tracker.com/* failed: %s", stderr)
	}
	if stderr != "" {
		t.Errorf("policy add unexpected stderr: %q", stderr)
	}

	// Add tracker.com/* deny -> domination warning to stderr, exit 0
	_, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "policy", "add", "egress", "tracker.com/*", "deny")
	if code != 0 {
		t.Fatalf("policy add egress tracker.com/* failed with code %d", code)
	}
	expectedNote := "note: overridden by cdn.tracker.com/* (more-specific match wins)"
	if !strings.Contains(stderr, expectedNote) {
		t.Errorf("domination warning stderr = %q, want %q", stderr, expectedNote)
	}

	// Add credential rules
	_, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "policy", "add", "credential", "gmail", "ask")
	if code != 0 {
		t.Fatalf("policy add credential gmail failed: %s", stderr)
	}
	_, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "policy", "add", "credential", "gmail/send", "deny")
	if code != 0 {
		t.Fatalf("policy add credential gmail/send failed: %s", stderr)
	}

	// Illegal pattern refusal
	_, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "policy", "add", "egress", "API.EXAMPLE.COM", "auto")
	if code != 1 {
		t.Errorf("illegal pattern exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "invalid pattern") {
		t.Errorf("illegal pattern stderr = %q, want 'invalid pattern'", stderr)
	}

	// 4. Policy list frozen header & round-trip parse (§11)
	stdout, stderr, code := runCmd(t, bin, "", "--config", cfgPath, "policy", "list")
	if code != 0 {
		t.Fatalf("policy list failed: %s", stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) < 2 {
		t.Fatalf("policy list expected at least header + rows, got: %q", stdout)
	}
	if lines[0] != "lane\tpattern\tverdict\talways\tsource" {
		t.Errorf("policy list header = %q, want 'lane\\tpattern\\tverdict\\talways\\tsource'", lines[0])
	}

	// 5. Policy rm
	_, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "policy", "rm", "egress", "tracker.com/*")
	if code != 0 {
		t.Fatalf("policy rm failed: %s", stderr)
	}
	// rm non-existent rule -> exit 1
	_, _, code = runCmd(t, bin, "", "--config", cfgPath, "policy", "rm", "egress", "non.existent.com")
	if code != 1 {
		t.Errorf("policy rm non-existent code = %d, want 1", code)
	}

	// 6. Policy reset (§6.5: "always-rules cleared")
	stdout, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "policy", "reset")
	if code != 0 {
		t.Fatalf("policy reset failed: %s", stderr)
	}
	if strings.TrimSpace(stdout) != "always-rules cleared" {
		t.Errorf("policy reset stdout = %q, want 'always-rules cleared'", stdout)
	}

	// 7. Egress strict on / off (§12 Q4, §11)
	stdout, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "egress", "strict", "on")
	if code != 0 {
		t.Fatalf("egress strict on failed: %s", stderr)
	}
	if strings.TrimSpace(stdout) != "egress strict: on" {
		t.Errorf("egress strict on stdout = %q, want 'egress strict: on'", stdout)
	}
	stdout, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "egress", "strict", "off")
	if code != 0 {
		t.Fatalf("egress strict off failed: %s", stderr)
	}
	if strings.TrimSpace(stdout) != "egress strict: off" {
		t.Errorf("egress strict off stdout = %q, want 'egress strict: off'", stdout)
	}
}

func testV14SurrogateOps(t *testing.T, bin, cfgPath string) {
	t.Helper()
	// 8. Surrogate add (§5.3, §6.5, §11)
	// Refuse unsatisfiable binding per §6.5
	_, stderr, code := runCmd(t, bin, "", "--config", cfgPath, "surrogate", "add", "openai", "--host", "127.0.0.1")
	if code != 1 {
		t.Errorf("surrogate add floor IP code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "unsatisfiable binding") {
		t.Errorf("surrogate add floor IP stderr = %q, want 'unsatisfiable binding'", stderr)
	}

	_, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "surrogate", "add", "openai", "--host", "localhost")
	if code != 1 {
		t.Errorf("surrogate add localhost code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "unsatisfiable binding") {
		t.Errorf("surrogate add localhost stderr = %q, want 'unsatisfiable binding'", stderr)
	}

	// Refuse non-canonical host
	_, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "surrogate", "add", "openai", "--host", "API.EXAMPLE.COM")
	if code != 1 {
		t.Errorf("surrogate add uppercase host code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "invalid pattern") {
		t.Errorf("surrogate add uppercase host stderr = %q, want 'invalid pattern'", stderr)
	}

	// Refuse port 0
	_, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "surrogate", "add", "openai", "--host", "api.example.com", "--port", "0")
	if code != 1 {
		t.Errorf("surrogate add port 0 code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "invalid port") && !strings.Contains(stderr, "invalid pattern") {
		t.Errorf("surrogate add port 0 stderr = %q, want invalid port/pattern", stderr)
	}

	// Valid surrogate add (TTY prints token once per §5.3)
	stdout, stderr, code := runCmd(t, bin, "", "--config", cfgPath, "surrogate", "add", "openai", "--host", "api.example.com", "--port", "8080", "--path", "/v1/")
	if code != 0 {
		t.Fatalf("surrogate add failed: %s", stderr)
	}
	token := strings.TrimSpace(stdout)
	if !strings.HasPrefix(token, "sur_") {
		t.Errorf("surrogate token = %q, want sur_ prefix", token)
	}
	id8 := custos.SHA256Hex8(token)

	// 9. Surrogate list frozen header & content (§11)
	stdout, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "surrogate", "list")
	if code != 0 {
		t.Fatalf("surrogate list failed: %s", stderr)
	}
	sLines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(sLines) < 2 {
		t.Fatalf("surrogate list expected header + row, got: %q", stdout)
	}
	if sLines[0] != "name\tfingerprint\tbinding" {
		t.Errorf("surrogate list header = %q, want 'name\\tfingerprint\\tbinding'", sLines[0])
	}
	// Assert token NEVER appears in list (§5.3, §11)
	if strings.Contains(stdout, token) {
		t.Errorf("surrogate list leaked cleartext token: %s", stdout)
	}
	if !strings.Contains(stdout, id8) {
		t.Errorf("surrogate list missing id8 %s in: %s", id8, stdout)
	}
	if !strings.Contains(stdout, "openai\t"+id8+"\tapi.example.com:8080/v1/") {
		t.Errorf("surrogate list row missing expected format, got: %s", stdout)
	}

	// 10. Surrogate revoke (§5.3, §11)
	_, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "surrogate", "revoke", id8)
	if code != 0 {
		t.Fatalf("surrogate revoke failed: %s", stderr)
	}
	stdout, _, _ = runCmd(t, bin, "", "--config", cfgPath, "surrogate", "list")
	if strings.Contains(stdout, id8) {
		t.Errorf("revoked surrogate still listed: %s", stdout)
	}

	// 11. Atomic revoke credential removes surrogates (§4.4, §5.3)
	// Add another surrogate for openai
	stdout, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "surrogate", "add", "openai", "--host", "api.openai.com")
	if code != 0 {
		t.Fatalf("second surrogate add failed: %s", stderr)
	}
	token2 := strings.TrimSpace(stdout)
	id8_2 := custos.SHA256Hex8(token2)

	// Revoke credential
	_, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "revoke", "openai")
	if code != 0 {
		t.Fatalf("revoke openai failed: %s", stderr)
	}

	// Assert surrogates list is now empty of openai surrogates
	stdout, _, _ = runCmd(t, bin, "", "--config", cfgPath, "surrogate", "list")
	if strings.Contains(stdout, id8_2) {
		t.Errorf("surrogate %s still in list after credential revoke: %s", id8_2, stdout)
	}

	// Audit verify clean
	stdout, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "audit", "verify")
	if code != 0 || !strings.Contains(stdout, "chain ok") {
		t.Fatalf("audit verify failed after mutations: code=%d, out=%s, err=%s", code, stdout, stderr)
	}
}

// V14: CLI frozen strings byte-exact: audit verify, policy add|list|rm|reset,
// snapshots list, surrogate list|add|revoke, egress strict; exit codes per §11 conventions
// (policy add prints the domination note: line to stderr, exit 0).
func TestV14_CLIFrozenStringsAndExitCodes(t *testing.T) {
	bin := buildCustosCLI(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	_ = os.MkdirAll(home, 0o700)

	cfgPath := filepath.Join(dir, "lararium.yaml")
	keyfilePath := filepath.Join(dir, "custos.key")
	passphrase := strings.Join([]string{"cli", "v14", "passphrase", "strong"}, "-")
	_ = os.WriteFile(keyfilePath, []byte(passphrase+"\n"), 0o600)

	cfgContent := "hearth:\n  home: " + home + "\n"
	_ = os.WriteFile(cfgPath, []byte(cfgContent), 0o600)

	stateDir := filepath.Join(home, "custos")

	// 1. Initialize vault
	_, stderr, code := runCmd(t, bin, passphrase+"\n", "--config", cfgPath, "init")
	if code != 0 {
		t.Fatalf("init failed: %s", stderr)
	}

	// 2. Refuse locked vault on surrogate add (without keyfile, no stdin)
	_, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "surrogate", "add", "openai", "--host", "api.example.com")
	if code != 1 {
		t.Errorf("locked surrogate add code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "custos locked — run: custos unlock") {
		t.Errorf("locked surrogate add stderr = %q, want 'custos locked — run: custos unlock'", stderr)
	}

	// Configure keyfile for automatic identity
	cfgWithKeyfile := fmt.Sprintf("hearth:\n  home: %s\ncustos:\n  unlock_keyfile: %s\n", home, keyfilePath)
	_ = os.WriteFile(cfgPath, []byte(cfgWithKeyfile), 0o600)

	// Add credential to vault via Vault API
	v := custos.NewVault(stateDir, 5*time.Second)
	if err := v.Unlock(passphrase, true); err != nil {
		t.Fatal(err)
	}
	err := v.Mutate(passphrase, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["openai"] = custos.Credential{
			Kind:   "api_key",
			Secret: "sk-test-secret-12345",
		}
		return []string{"openai"}, nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	// Run policy and surrogate operations
	testV14PolicyOps(t, bin, cfgPath)
	testV14SurrogateOps(t, bin, cfgPath)
}

func TestCustosApprovalsCLI(t *testing.T) {
	bin := buildCustosCLI(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	_ = os.MkdirAll(home, 0o700)

	cfgPath := filepath.Join(dir, "lararium.yaml")
	cfgContent := "hearth:\n  home: " + home + "\n"
	_ = os.WriteFile(cfgPath, []byte(cfgContent), 0o600)

	pass := "cli-passphrase-approvals"

	// 1. custos init
	_, _, code := runCmd(t, bin, pass+"\n", "--config", cfgPath, "init")
	if code != 0 {
		t.Fatalf("init failed")
	}

	stateDir := filepath.Join(home, "custos")
	v := custos.NewVault(stateDir, 5*time.Second)
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	hub := surface.NewApprovalHub(5 * time.Second)
	v.SetApprovalHub(hub)

	ws := custos.NewWorkerServer(v, hub, &custos.Config{})

	// Start daemon ctlServer with hub and workers
	ctlServer, err := custos.StartCtlServer(stateDir, v, nil)
	if err != nil {
		t.Fatalf("start ctl server: %v", err)
	}
	defer ctlServer.Close()
	ctlServer.SetHub(hub)
	ctlServer.SetWorkers(ws)

	// 2. custos approvals (empty list)
	stdout, _, code := runCmd(t, bin, "", "--config", cfgPath, "approvals")
	if code != 0 {
		t.Fatalf("approvals empty exit code = %d, want 0", code)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("expected empty output, got: %q", stdout)
	}

	// 3. Phantom hub card (MAJOR 6): card in hub but no park manager must not appear
	phantomID, _ := hub.RegisterCustos("cell-1", "gmail", "gmail.googleapis.com:send", "review-phantom", 5*time.Second, nil)
	stdout, _, code = runCmd(t, bin, "", "--config", cfgPath, "approvals", "list")
	if code != 0 {
		t.Fatalf("approvals list exit code = %d, want 0", code)
	}
	if strings.Contains(stdout, phantomID) {
		t.Fatalf("phantom hub card appeared in approvals list: %q", stdout)
	}

	// 4. Properly parked card: appears in approvals list
	cardID, _ := hub.RegisterCustos("cell-1", "gmail", "gmail.googleapis.com:send", "review-summary", 5*time.Second, nil)
	ws.RegisterParkForTest(cardID, "cell-1", "gmail", "send", "review-summary")

	stdout, _, code = runCmd(t, bin, "", "--config", cfgPath, "approvals", "list")
	if code != 0 {
		t.Fatalf("approvals list exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, cardID) {
		t.Fatalf("approvals list output missing cardID: %q", stdout)
	}

	// 5. Nonexistent card error string (BLOCKING 3: "no such card")
	_, stderr, code := runCmd(t, bin, "", "--config", cfgPath, "approvals", "approve", "nonexistent-card-id")
	if code != 1 {
		t.Fatalf("approve nonexistent exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no such card") {
		t.Fatalf("approve nonexistent stderr = %q, want 'no such card'", stderr)
	}

	_, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "approvals", "deny", "nonexistent-card-id")
	if code != 1 {
		t.Fatalf("deny nonexistent exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no such card") {
		t.Fatalf("deny nonexistent stderr = %q, want 'no such card'", stderr)
	}

	// 6. custos approvals approve <id>
	stdout, _, code = runCmd(t, bin, "", "--config", cfgPath, "approvals", "approve", cardID)
	if code != 0 {
		t.Fatalf("approvals approve exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "approved") {
		t.Fatalf("approvals approve output = %q, want 'approved'", stdout)
	}

	// 7. second approve -> already answered
	_, stderr, code = runCmd(t, bin, "", "--config", cfgPath, "approvals", "approve", cardID)
	if code != 1 {
		t.Fatalf("second approve code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "already answered") {
		t.Fatalf("second approve stderr = %q, want 'already answered'", stderr)
	}

	// 8. Register another card and deny
	cardID2, _ := hub.RegisterCustos("cell-1", "gmail", "gmail.googleapis.com:send", "review-summary-2", 5*time.Second, nil)
	ws.RegisterParkForTest(cardID2, "cell-1", "gmail", "send", "review-summary-2")
	stdout, _, code = runCmd(t, bin, "", "--config", cfgPath, "approvals", "deny", cardID2)
	if code != 0 {
		t.Fatalf("approvals deny exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "denied") {
		t.Fatalf("approvals deny output = %q, want 'denied'", stdout)
	}

	// 9. Interactive watch with piped stdin (BLOCKING 5)
	stdinR, stdinW := io.Pipe()
	cmdWatch := exec.Command(bin, "--config", cfgPath, "approvals", "watch")
	cmdWatch.Stdin = stdinR
	var watchOut, watchErr bytes.Buffer
	cmdWatch.Stdout = &watchOut
	cmdWatch.Stderr = &watchErr

	if err := cmdWatch.Start(); err != nil {
		t.Fatalf("start watch: %v", err)
	}

	// Park card while watch is running
	cardID3, _ := hub.RegisterCustos("cell-1", "gmail", "gmail.googleapis.com:send", "review-watch-test", 5*time.Second, nil)
	ws.RegisterParkForTest(cardID3, "cell-1", "gmail", "send", "review-watch-test")

	// Watch must reprint added card within its poll interval (1s)
	deadline := time.Now().Add(3 * time.Second)
	foundCard := false
	for time.Now().Before(deadline) {
		if strings.Contains(watchOut.String(), cardID3) {
			foundCard = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !foundCard {
		t.Fatalf("watch did not reprint added card within poll interval; stdout=%q, stderr=%q", watchOut.String(), watchErr.String())
	}

	// Send interactive approve command: "a <id>\n"
	_, _ = stdinW.Write([]byte(fmt.Sprintf("a %s\n", cardID3)))

	// Wait for approved output
	deadline = time.Now().Add(3 * time.Second)
	foundApproved := false
	for time.Now().Before(deadline) {
		if strings.Contains(watchOut.String(), "approved") {
			foundApproved = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !foundApproved {
		t.Fatalf("watch did not output 'approved'; stdout=%q, stderr=%q", watchOut.String(), watchErr.String())
	}

	// Send 'q\n' to exit
	_, _ = stdinW.Write([]byte("q\n"))
	_ = stdinW.Close()

	if err := cmdWatch.Wait(); err != nil {
		t.Fatalf("watch command exited with error: %v, stderr=%s", err, watchErr.String())
	}
}
