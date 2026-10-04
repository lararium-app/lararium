package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// KEYS-SPEC K3/K6 CLI coverage: build the real binary and drive it —
// the frozen output strings and exit codes are the contract.

func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "hearthd-test")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// hearthHome writes a minimal lararium.yaml + home dir and returns
// (configPath, home).
func hearthHome(t *testing.T) (string, string) {
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

func runCLI(t *testing.T, bin, cfgPath string, stdin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"-config", cfgPath, "keys"}, args...)...)
	cmd.Env = os.Environ()
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	code := cmd.ProcessState.ExitCode()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run %v: %v\n%s", args, err, out)
		}
	}
	return string(out), code
}

func TestCLISetListRm(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, home := hearthHome(t)

	out, code := runCLI(t, bin, cfgPath, "sk-secret-abc123\n", "set", "openai")
	if code != 0 || !strings.Contains(out, "saved openai") ||
		!strings.Contains(out, "no daemon running — will apply at next start") {
		t.Fatalf("set: code=%d out=%q", code, out)
	}

	// keys.json exists with the trimmed value at mode 0600.
	raw, err := os.ReadFile(filepath.Join(home, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(home, "keys.json"))
	if m := fi.Mode().Perm(); m != 0o600 {
		t.Errorf("keys.json mode = %o, want 600", m)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil || m["openai"] != "sk-secret-abc123" {
		t.Errorf("keys.json = %s", raw)
	}

	// list: masked display, never the value.
	out, code = runCLI(t, bin, cfgPath, "", "list")
	if code != 0 || !strings.Contains(out, "openai") || !strings.Contains(out, "keys.json") {
		t.Fatalf("list: code=%d out=%q", code, out)
	}
	if strings.Contains(out, "sk-secret-abc123") {
		t.Errorf("list leaked the key: %q", out)
	}
	// Config provider with no key anywhere: missing row.
	if !strings.Contains(out, "p\tmissing") {
		t.Errorf("list missing row: %q", out)
	}

	out, code = runCLI(t, bin, cfgPath, "", "rm", "openai")
	if code != 0 || !strings.Contains(out, "removed openai") {
		t.Fatalf("rm: code=%d out=%q", code, out)
	}
	// Idempotent rm: "not set", exit 0, and NO "will apply" line (V12).
	out, code = runCLI(t, bin, cfgPath, "", "rm", "openai")
	if code != 0 || strings.TrimSpace(out) != "not set" {
		t.Fatalf("rm absent: code=%d out=%q, want exactly 'not set'", code, out)
	}
}

func TestCLIBadName(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, _ := hearthHome(t)
	for _, bad := range []string{"Bad", "a b", "..", "_x"} {
		out, code := runCLI(t, bin, cfgPath, "sk-x\n", "set", bad)
		if code == 0 || !strings.Contains(out, "invalid provider name") {
			t.Errorf("set %q: code=%d out=%q", bad, code, out)
		}
		out, code = runCLI(t, bin, cfgPath, "", "rm", bad)
		if code == 0 || !strings.Contains(out, "invalid provider name") {
			t.Errorf("rm %q: code=%d out=%q", bad, code, out)
		}
	}
}

func TestCLIEmptyKey(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, home := hearthHome(t)
	out, code := runCLI(t, bin, cfgPath, "   \n", "set", "openai")
	if code == 0 || !strings.Contains(out, "empty key") {
		t.Fatalf("empty: code=%d out=%q", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, "keys.json")); err == nil {
		t.Errorf("empty set created keys.json")
	}
}

func TestCLIFullStore(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, home := hearthHome(t)

	// Pre-seed 64 entries (the cap) directly.
	m := map[string]string{}
	for i := range 64 {
		m["k"+fmt.Sprintf("%02d", i)] = "v"
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "keys.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// New name at cap: frozen message, non-zero, BEFORE prompting.
	out, code := runCLI(t, bin, cfgPath, "", "set", "zz9")
	if code == 0 || !strings.Contains(out, "key store full") {
		t.Fatalf("full: code=%d out=%q", code, out)
	}
	// Overwrite at cap: allowed.
	if out, code = runCLI(t, bin, cfgPath, "sk-new\n", "set", "k00"); code != 0 ||
		!strings.Contains(out, "saved k00") {
		t.Fatalf("overwrite at cap: code=%d out=%q", code, out)
	}
}

func TestCLIReloadAckFailed(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, home := hearthHome(t)

	// A listener that accepts and says nothing: the ack read times out.
	sockPath := filepath.Join(home, "hearthd.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn // never replies; CLI gives up after its timeout
		}
	}()
	defer ln.Close()

	start := time.Now()
	out, code := runCLI(t, bin, cfgPath, "sk-x\n", "set", "openai")
	if code != 0 || !strings.Contains(out, "saved openai") ||
		!strings.Contains(out, "saved; reload ack failed — restart the daemon to apply now") {
		t.Fatalf("ack-fail: code=%d out=%q", code, out)
	}
	// The write succeeded regardless of the ack.
	if _, err := os.Stat(filepath.Join(home, "keys.json")); err != nil {
		t.Errorf("keys.json missing after ack failure")
	}
	// Sanity: the CLI bounded its wait (5s timeout + slack).
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("CLI waited %v for the ack", d)
	}
}
