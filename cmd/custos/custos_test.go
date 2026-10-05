package main_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lararium-app/lararium/internal/custos"
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
