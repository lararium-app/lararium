package main_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func buildHearthdCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "hearthd-test")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build hearthd: %v\n%s", err, out)
	}
	return bin
}

func makeThrowawayHearth(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "lararium.yaml")
	cfg := "hearth:\n  home: " + home + "\nmodels:\n  default: [p/m]\nproviders:\n  - name: p\n    base_url: http://127.0.0.1:9/v1\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, home
}

func runHearthdArgs(t *testing.T, bin string, args ...string) (string, string, int) {
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

func TestHearthdHelpNeverMutates(t *testing.T) {
	bin := buildHearthdCLI(t)

	tests := []struct {
		name string
		args []string
	}{
		{"top-level help", []string{"--help"}},
		{"top-level -h", []string{"-h"}},
		{"token", []string{"token", "--help"}},
		{"token create", []string{"token", "create", "--help"}},
		{"token create reversed", []string{"token", "--help", "create"}},
		{"token revoke", []string{"token", "revoke", "--help"}},
		{"token list", []string{"token", "list", "--help"}},
		{"keys", []string{"keys", "--help"}},
		{"keys set", []string{"keys", "set", "--help"}},
		{"keys set reversed", []string{"keys", "--help", "set"}},
		{"keys rm", []string{"keys", "rm", "--help"}},
		{"keys list", []string{"keys", "list", "--help"}},
		{"pair", []string{"pair", "--help"}},
		{"pair create", []string{"pair", "create", "--help"}},
		{"pair revoke", []string{"pair", "revoke", "--help"}},
		{"pair status", []string{"pair", "status", "--help"}},
		{"pair unpair", []string{"pair", "unpair", "--help"}},
		{"backup", []string{"backup", "--help"}},
		{"backup create", []string{"backup", "create", "--help"}},
		{"backup create reversed", []string{"backup", "--help", "create"}},
		{"backup list", []string{"backup", "list", "--help"}},
		{"backup verify", []string{"backup", "verify", "--help"}},
		{"backup extract-config", []string{"backup", "extract-config", "--help"}},
		{"backup keygen", []string{"backup", "keygen", "--help"}},
		{"restore", []string{"restore", "--help"}},
		{"setup", []string{"setup", "--help"}},
		{"serve", []string{"serve", "--help"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, home := makeThrowawayHearth(t)
			fullArgs := append([]string{"-config", cfgPath}, tc.args...)

			stdout, stderr, code := runHearthdArgs(t, bin, fullArgs...)
			if code != 0 {
				t.Fatalf("expected exit code 0, got %d (stderr: %q, stdout: %q)", code, stderr, stdout)
			}

			combined := stdout + "\n" + stderr
			if !strings.Contains(strings.ToLower(combined), "usage") {
				t.Fatalf("expected usage in output, got stdout=%q, stderr=%q", stdout, stderr)
			}

			// State mutation assertions:
			// 1. tokens.json must NOT exist (token create must not have minted a token)
			tokensPath := filepath.Join(home, "tokens.json")
			if _, err := os.Stat(tokensPath); err == nil {
				t.Fatalf("tokens.json was created during help invocation %v", tc.args)
			}

			// 2. keys.json must NOT exist
			keysPath := filepath.Join(home, "keys.json")
			if _, err := os.Stat(keysPath); err == nil {
				t.Fatalf("keys.json was created during help invocation %v", tc.args)
			}

			// 3. nuntius owners must NOT exist
			ownersPath := filepath.Join(home, "nuntius", "owners.json")
			if _, err := os.Stat(ownersPath); err == nil {
				t.Fatalf("owners.json was created during help invocation %v", tc.args)
			}
		})
	}
}

func TestServe_StubWebAssetsWarning(t *testing.T) {
	bin := buildHearthdCLI(t)
	cfgPath, home := makeThrowawayHearth(t)
	for _, name := range []string{"SOUL.md", "IDENTITY.md", "USER.md"} {
		_ = os.WriteFile(filepath.Join(home, name), []byte("---\nkind: persona\n---\n# "+name+"\n"), 0o600)
	}

	cmd := exec.Command(bin, "-config", cfgPath, "serve")
	var stdout, stderr safeBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hearthd serve: %v", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	wantWarn := "warning: web assets not built \u2014 run (cd web && npm ci && npm run build) and rebuild, or use the official Docker image; the chat UI will not load"
	found := false
	for range 30 {
		if strings.Contains(stderr.String(), wantWarn) {
			found = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !found {
		t.Errorf("expected warning %q on stderr, got %q", wantWarn, stderr.String())
	}
}
