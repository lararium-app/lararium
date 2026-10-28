package main_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func buildCustosd(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "custosd")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build custosd: %v\n%s", err, out)
	}
	return bin
}

func TestCustosdDaemon(t *testing.T) {
	bin := buildCustosd(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	_ = os.MkdirAll(home, 0o700)

	cfgPath := filepath.Join(dir, "lararium.yaml")
	cfgContent := "hearth:\n  home: " + home + "\n"
	_ = os.WriteFile(cfgPath, []byte(cfgContent), 0o600)

	stateDir := filepath.Join(home, "custos")
	pass := "daemon-passphrase-1234"

	// Initialize vault first
	v := custos.NewVault(stateDir, 5*time.Second)
	if err := v.Init(pass); err != nil {
		t.Fatalf("init vault: %v", err)
	}

	// Start custosd in background
	cmd := exec.Command(bin, "--config", cfgPath, "serve")
	var stdout, stderr safeBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start custosd: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	// Wait for ctl.sock to appear
	sockPath := filepath.Join(stateDir, "ctl.sock")
	for range 60 {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	doorsPath := filepath.Join(stateDir, "doors.sock")
	wantBanner := "custosd serve: doors on " + doorsPath + " \u2014 ctl on " + sockPath + " \u2014 vault: locked"
	if !strings.Contains(stderr.String(), wantBanner) {
		t.Errorf("stderr = %q, want banner containing %q", stderr.String(), wantBanner)
	}

	// 1. custosd status (should be locked)
	statusCmd := exec.Command(bin, "--config", cfgPath, "status")
	var statusOut bytes.Buffer
	statusCmd.Stdout = &statusOut
	if err := statusCmd.Run(); err != nil {
		t.Fatalf("custosd status: %v (daemon err: %s)", err, stderr.String())
	}
	if !strings.Contains(statusOut.String(), "locked") {
		t.Errorf("status = %q, want 'locked'", statusOut.String())
	}

	// 2. custosd shutdown
	shutdownCmd := exec.Command(bin, "--config", cfgPath, "shutdown")
	if err := shutdownCmd.Run(); err != nil {
		t.Fatalf("custosd shutdown: %v", err)
	}

	// Wait for daemon process to terminate
	err := cmd.Wait()
	if err != nil && cmd.ProcessState.ExitCode() != 0 {
		t.Logf("daemon exited: %v", err)
	}
}

func TestCustosdDaemon_KeyfileMode(t *testing.T) {
	bin := buildCustosd(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	_ = os.MkdirAll(home, 0o700)

	keyfilePath := filepath.Join(dir, "unlock.key")
	pass := "daemon-passphrase-5678"
	_ = os.WriteFile(keyfilePath, []byte(pass+"\n"), 0o600)

	cfgPath := filepath.Join(dir, "lararium.yaml")
	cfgContent := "hearth:\n  home: " + home + "\ncustos:\n  unlock_keyfile: " + keyfilePath + "\n"
	_ = os.WriteFile(cfgPath, []byte(cfgContent), 0o600)

	stateDir := filepath.Join(home, "custos")

	// Initialize vault first
	v := custos.NewVault(stateDir, 5*time.Second)
	if err := v.Init(pass); err != nil {
		t.Fatalf("init vault: %v", err)
	}

	// Start custosd in background
	cmd := exec.Command(bin, "--config", cfgPath, "serve")
	var stdout, stderr safeBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start custosd: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	// Wait for ctl.sock to appear
	sockPath := filepath.Join(stateDir, "ctl.sock")
	for range 60 {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	doorsPath := filepath.Join(stateDir, "doors.sock")
	wantBanner := "custosd serve: doors on " + doorsPath + " \u2014 ctl on " + sockPath + " \u2014 vault: unlocked (keyfile)"
	if !strings.Contains(stderr.String(), wantBanner) {
		t.Errorf("stderr = %q, want banner containing %q", stderr.String(), wantBanner)
	}

	// 1. custosd status (should be unlocked)
	statusCmd := exec.Command(bin, "--config", cfgPath, "status")
	var statusOut bytes.Buffer
	statusCmd.Stdout = &statusOut
	if err := statusCmd.Run(); err != nil {
		t.Fatalf("custosd status: %v (daemon err: %s)", err, stderr.String())
	}
	if !strings.Contains(statusOut.String(), "unlocked") {
		t.Errorf("status = %q, want 'unlocked'", statusOut.String())
	}

	// 2. custosd shutdown
	shutdownCmd := exec.Command(bin, "--config", cfgPath, "shutdown")
	if err := shutdownCmd.Run(); err != nil {
		t.Fatalf("custosd shutdown: %v", err)
	}

	// Wait for daemon process to terminate
	if waitErr := cmd.Wait(); waitErr != nil && cmd.ProcessState.ExitCode() != 0 {
		t.Logf("daemon exited: %v", waitErr)
	}
}

func TestCustosd_BrokenYAML_FailsBoot(t *testing.T) {
	bin := buildCustosd(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "broken.yaml")
	brokenYAML := `custos:
  connectors:
    mcp: [
`
	if err := os.WriteFile(cfgPath, []byte(brokenYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "--config", cfgPath, "serve")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatalf("expected custosd -config broken.yaml serve to fail, but exited 0")
	}

	errStr := stderr.String()
	if !strings.Contains(errStr, cfgPath) {
		t.Errorf("stderr %q must mention file path %q", errStr, cfgPath)
	}
	if !strings.Contains(errStr, "line") {
		t.Errorf("stderr %q must mention 'line'", errStr)
	}
	if !strings.Contains(errStr, "column") {
		t.Errorf("stderr %q must mention 'column'", errStr)
	}
}

func TestCustosd_UnknownKey_SinglePrefix(t *testing.T) {
	bin := buildCustosd(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "unknown.yaml")
	unknownYAML := `custos:
  state_dir: /foo
`
	if err := os.WriteFile(cfgPath, []byte(unknownYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "--config", cfgPath, "status")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatalf("expected custosd -config unknown.yaml status to fail, but exited 0")
	}

	errStr := stderr.String()
	wantSingle := `config error: unknown_key: unknown key "state_dir" in custos config`
	if !strings.Contains(errStr, wantSingle) {
		t.Errorf("stderr %q does not contain expected single-prefix message %q", errStr, wantSingle)
	}
	if strings.Contains(errStr, "unknown_key: unknown_key:") { //nolint:dupword // asserts the doubled prefix is absent
		t.Errorf("stderr %q contains duplicated prefix", errStr)
	}
}

// A named config path (-config) that does not exist must be fatal: a typo'd
// path must not silently boot with empty defaults (vault in the wrong place).
// Only the never-created DEFAULT config in the cwd stays lenient.
func TestCustosd_ExplicitMissingConfigFatal(t *testing.T) {
	bin := buildCustosd(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope", "lararium.yaml")

	cmd := exec.Command(bin, "-config", missing, "status")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("expected non-zero exit for missing explicit config; stderr: %s", stderr.String())
	}
	if out := stderr.String(); !strings.Contains(out, "config error") {
		t.Fatalf("expected 'config error' in stderr, got: %s", out)
	}

	// Default config missing in cwd: still lenient (first-run flows).
	cmd = exec.Command(bin, "status")
	cmd.Dir = dir
	stderr.Reset()
	cmd.Stderr = &stderr
	_ = cmd.Run() // may fail for other reasons (no socket); must NOT be a config error
	if out := stderr.String(); strings.Contains(out, "config error") {
		t.Fatalf("missing DEFAULT config must stay lenient, got: %s", out)
	}
}
