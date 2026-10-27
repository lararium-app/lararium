package custos_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lararium-app/lararium/internal/custos"
)

func runLoaderSubprocessWithPath(t *testing.T, yamlContent string) (string, string, int) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "lararium.yaml")
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestMCPConfigLoaderHelper", "--") //nolint:gosec // G702: re-executes this test binary with static args
	cmd.Env = append(os.Environ(),
		"GO_WANT_HELPER_PROCESS=1",
		"TEST_CONFIG_PATH="+cfgPath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("run subprocess: %v", err)
		}
	}
	return cfgPath, stderr.String(), exitCode
}

func TestB2_BrokenYAMLInsideConnectors_MentionsFileLineColumn(t *testing.T) {
	brokenYAML := `custos:
  connectors:
    mcp: [
`
	cfgPath, stderr, exitCode := runLoaderSubprocessWithPath(t, brokenYAML)
	if exitCode == 0 {
		t.Fatalf("expected non-zero exit code on broken YAML, got 0")
	}

	if !strings.Contains(stderr, cfgPath) {
		t.Errorf("stderr %q must mention file path %q", stderr, cfgPath)
	}
	if !strings.Contains(stderr, "line") {
		t.Errorf("stderr %q must mention 'line'", stderr)
	}
	if !strings.Contains(stderr, "column") {
		t.Errorf("stderr %q must mention 'column'", stderr)
	}
}

func TestB2_UnknownKeySinglePrefixShape(t *testing.T) {
	unknownKeyYAML := `custos:
  state_dir: /foo
`
	_, stderr, exitCode := runLoaderSubprocessWithPath(t, unknownKeyYAML)
	if exitCode == 0 {
		t.Fatalf("expected non-zero exit code on unknown key, got 0")
	}

	wantSingle := `config error: unknown_key: unknown key "state_dir" in custos config`
	if !strings.Contains(stderr, wantSingle) {
		t.Errorf("stderr %q does not contain expected single-prefix message %q", stderr, wantSingle)
	}

	if strings.Contains(stderr, "unknown_key: unknown_key:") { //nolint:dupword // asserts the doubled prefix is absent
		t.Errorf("stderr %q contains duplicated prefix 'unknown_key: unknown_key:'", stderr)
	}
}

func TestB2_ValidConfigBootsRegression(t *testing.T) {
	fakeExec := createFakeExecutable(t)
	validYAML := `custos:
  connectors:
    mcp:
      - name: test-server
        transport: stdio
        command: ` + fakeExec + `
`
	_, stderr, exitCode := runLoaderSubprocessWithPath(t, validYAML)
	if exitCode != 0 {
		t.Fatalf("expected exit code 0 on valid config, got %d (stderr: %q)", exitCode, stderr)
	}

	// In-process check: LoadConfig directly loads valid config without error
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "lararium.yaml")
	if err := os.WriteFile(cfgPath, []byte(validYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := custos.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig failed on valid config: %v", err)
	}
	if len(cfg.Connectors.MCP) != 1 || cfg.Connectors.MCP[0].Name != "test-server" {
		t.Fatalf("unexpected connectors in loaded config: %+v", cfg.Connectors.MCP)
	}
}
