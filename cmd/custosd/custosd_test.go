package main_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
)

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
	var stdout, stderr bytes.Buffer
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
	for range 20 {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
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
