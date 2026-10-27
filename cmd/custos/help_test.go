package main_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func makeThrowawayCustos(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "lararium.yaml")
	cfg := "hearth:\n  home: " + home + "\ncustos:\n  header_extras: [x-trace]\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, home
}

func runCustosArgs(t *testing.T, bin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run %v: %v", args, err)
		}
	}
	return stdout.String(), stderr.String(), code
}

func TestCustosHelpNeverMutates(t *testing.T) {
	bin := buildCustosCLI(t)

	tests := []struct {
		name string
		args []string
	}{
		{"top-level help", []string{"--help"}},
		{"top-level -h", []string{"-h"}},
		{"init", []string{"init", "--help"}},
		{"unlock", []string{"unlock", "--help"}},
		{"lock", []string{"lock", "--help"}},
		{"status", []string{"status", "--help"}},
		{"audit", []string{"audit", "--help"}},
		{"audit verify", []string{"audit", "verify", "--help"}},
		{"snapshots", []string{"snapshots", "--help"}},
		{"restore", []string{"restore", "--help"}},
		{"change-passphrase", []string{"change-passphrase", "--help"}},
		{"surrogate", []string{"surrogate", "--help"}},
		{"surrogate add", []string{"surrogate", "add", "--help"}},
		{"surrogate list", []string{"surrogate", "list", "--help"}},
		{"surrogate revoke", []string{"surrogate", "revoke", "--help"}},
		{"policy", []string{"policy", "--help"}},
		{"policy add", []string{"policy", "add", "--help"}},
		{"policy rm", []string{"policy", "rm", "--help"}},
		{"policy list", []string{"policy", "list", "--help"}},
		{"policy reset", []string{"policy", "reset", "--help"}},
		{"egress", []string{"egress", "--help"}},
		{"revoke", []string{"revoke", "--help"}},
		{"login", []string{"login", "--help"}},
		{"approvals", []string{"approvals", "--help"}},
		{"approvals watch", []string{"approvals", "watch", "--help"}},
		{"approvals approve", []string{"approvals", "approve", "--help"}},
		{"approvals deny", []string{"approvals", "deny", "--help"}},
		{"mcp", []string{"mcp", "--help"}},
		{"mcp status", []string{"mcp", "status", "--help"}},
		{"mcp tools", []string{"mcp", "tools", "--help"}},
		{"mcp refresh", []string{"mcp", "refresh", "--help"}},
		{"mcp call", []string{"mcp", "call", "--help"}},
		{"mcp trust", []string{"mcp", "trust", "--help"}},
		{"mcp distrust", []string{"mcp", "distrust", "--help"}},
		{"mcp trust reversed", []string{"mcp", "--help", "trust"}},
		{"mcp distrust reversed", []string{"mcp", "--help", "distrust"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, home := makeThrowawayCustos(t)
			fullArgs := append([]string{"-config", cfgPath}, tc.args...)

			stdout, stderr, code := runCustosArgs(t, bin, fullArgs...)
			if code != 0 {
				t.Fatalf("expected exit code 0, got %d (stderr: %q, stdout: %q)", code, stderr, stdout)
			}

			combined := stdout + "\n" + stderr
			if !strings.Contains(strings.ToLower(combined), "usage") {
				t.Fatalf("expected usage in output, got stdout=%q, stderr=%q", stdout, stderr)
			}

			// State mutation assertions:
			// custos state dir should not have unprompted initialized vault or lock files
			stateDir := filepath.Join(home, "custos")
			vaultDoc := filepath.Join(stateDir, "vault.json")
			if _, err := os.Stat(vaultDoc); err == nil {
				t.Fatalf("vault.json was created during help invocation %v", tc.args)
			}
			lockFile := filepath.Join(stateDir, "custos.lock")
			if _, err := os.Stat(lockFile); err == nil {
				t.Fatalf("custos.lock was created during help invocation %v", tc.args)
			}
		})
	}
}

func TestCustosMCPHelpMentionsTrustDistrust(t *testing.T) {
	bin := buildCustosCLI(t)
	cfgPath, _ := makeThrowawayCustos(t)

	stdout, stderr, code := runCustosArgs(t, bin, "-config", cfgPath, "mcp", "--help")
	if code != 0 {
		t.Fatalf("custos mcp --help exited with %d; want 0", code)
	}

	combined := stdout + "\n" + stderr
	if !strings.Contains(combined, "trust") {
		t.Fatalf("expected 'trust' in mcp --help output, got:\n%s", combined)
	}
	if !strings.Contains(combined, "distrust") {
		t.Fatalf("expected 'distrust' in mcp --help output, got:\n%s", combined)
	}
}
