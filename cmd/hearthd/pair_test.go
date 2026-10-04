package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/nuntius"
)

// NUNTIUS-SPEC §3 pairing CLI coverage: build the real binary and drive
// it to verify stdout/stderr contracts, exit codes, and on-disk state.

func hearthHomeWithNuntius(t *testing.T, nuntiusYAML string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "lararium.yaml")
	cfg := "hearth:\n  home: " + home + "\nmodels:\n  default: [p/m]\nproviders:\n  - name: p\n    base_url: http://127.0.0.1:9/v1\n"
	if nuntiusYAML != "" {
		cfg += "nuntius:\n" + nuntiusYAML
	}
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, home
}

func runPairCLI(t *testing.T, bin, cfgPath string, args ...string) (string, string, int) {
	t.Helper()
	fullArgs := []string{"-config", cfgPath, "pair"}
	fullArgs = append(fullArgs, args...)
	cmd := exec.Command(bin, fullArgs...)
	cmd.Env = os.Environ()
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
			t.Fatalf("run %v: %v", fullArgs, err)
		}
	}
	return stdout.String(), stderr.String(), code
}

func runHearthdCLI(t *testing.T, bin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = os.Environ()
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

func TestPairCreate(t *testing.T) {
	bin := buildCLI(t)
	// Default config without nuntius block: CLI must succeed and use defaults (§3, §4).
	cfgPath, home := hearthHome(t)

	now := time.Now().Unix()
	stdout, stderr, code := runPairCLI(t, bin, cfgPath, "create")
	if code != 0 || stderr != "" {
		t.Fatalf("pair create failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	codeStr := strings.TrimRight(stdout, "\r\n")
	re := regexp.MustCompile(`^pair1_[0-9A-Za-z]{20}$`)
	if !re.MatchString(codeStr) {
		t.Fatalf("pair create code %q does not match %s", codeStr, re.String())
	}

	raw, err := os.ReadFile(filepath.Join(home, "nuntius", "owners.json"))
	if err != nil {
		t.Fatalf("read owners.json: %v", err)
	}
	var o nuntius.Owners
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatalf("unmarshal owners.json: %v", err)
	}
	if len(o.Codes) != 1 {
		t.Fatalf("expected 1 pending code in owners.json, got %d", len(o.Codes))
	}
	// Default TTL is 15m (900s). Check expires ≈ now + 900s.
	wantExpires := now + 15*60
	diff := o.Codes[0].Expires - wantExpires
	if diff < -5 || diff > 5 {
		t.Fatalf("expires %d not within 5s of expected %d", o.Codes[0].Expires, wantExpires)
	}
	if len(o.Codes[0].Hash) != 64 {
		t.Fatalf("code hash length %d, want 64", len(o.Codes[0].Hash))
	}
}

func TestPairCreateHonorsTTL(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, home := hearthHomeWithNuntius(t, "  pair_code_ttl: 42s\n")

	stdout, stderr, code := runPairCLI(t, bin, cfgPath, "create")
	if code != 0 || stderr != "" {
		t.Fatalf("pair create failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	raw, err := os.ReadFile(filepath.Join(home, "nuntius", "owners.json"))
	if err != nil {
		t.Fatalf("read owners.json: %v", err)
	}
	var o nuntius.Owners
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatalf("unmarshal owners.json: %v", err)
	}
	if len(o.Codes) != 1 {
		t.Fatalf("expected 1 code, got %d", len(o.Codes))
	}

	ttlSecs := o.Codes[0].Expires - o.Codes[0].Created
	if ttlSecs != 42 {
		t.Fatalf("expected TTL 42s, got %d (created=%d expires=%d)", ttlSecs, o.Codes[0].Created, o.Codes[0].Expires)
	}
}

func TestPairRevokeAll(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, home := hearthHome(t)

	// Create two codes first.
	if _, _, code := runPairCLI(t, bin, cfgPath, "create"); code != 0 {
		t.Fatalf("create 1 failed: code=%d", code)
	}
	if _, _, code := runPairCLI(t, bin, cfgPath, "create"); code != 0 {
		t.Fatalf("create 2 failed: code=%d", code)
	}

	// Revoke without --all: wrong arg exits 2 with usage.
	_, stderr, code := runPairCLI(t, bin, cfgPath, "revoke")
	if code != 2 || !strings.Contains(stderr, "usage: hearthd pair revoke --all") {
		t.Fatalf("revoke with no args: code=%d stderr=%q", code, stderr)
	}
	_, stderr, code = runPairCLI(t, bin, cfgPath, "revoke", "--bad")
	if code != 2 || !strings.Contains(stderr, "usage: hearthd pair revoke --all") {
		t.Fatalf("revoke with bad arg: code=%d stderr=%q", code, stderr)
	}

	// Revoke all: clears pending codes and prints exact line.
	stdout, stderr, code := runPairCLI(t, bin, cfgPath, "revoke", "--all")
	if code != 0 || stderr != "" {
		t.Fatalf("revoke --all failed: code=%d stderr=%q", code, stderr)
	}
	if stdout != "pending codes cleared\n" {
		t.Fatalf("revoke --all stdout = %q, want %q", stdout, "pending codes cleared\n")
	}

	// Verify file is valid JSON with empty codes.
	raw, err := os.ReadFile(filepath.Join(home, "nuntius", "owners.json"))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read owners.json: %v", err)
		}
	} else {
		var o nuntius.Owners
		if err := json.Unmarshal(raw, &o); err != nil {
			t.Fatalf("owners.json invalid json: %v", err)
		}
		if len(o.Codes) != 0 {
			t.Fatalf("expected 0 pending codes after revoke --all, got %d", len(o.Codes))
		}
	}
}

func TestPairUnpair(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, home := hearthHome(t)

	// Pre-seed an owner using NewPairStore + Mint + Redeem (§3).
	store, err := nuntius.NewPairStore(filepath.Join(home, "nuntius"))
	if err != nil {
		t.Fatalf("NewPairStore: %v", err)
	}
	now := time.Now()
	code, err := store.Mint(now, 15*time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	res, err := store.Redeem("42", code, now)
	if err != nil || res != nuntius.RedeemPaired {
		t.Fatalf("Redeem: res=%v err=%v", res, err)
	}

	o, err := store.Load()
	if err != nil || len(o.OwnerIDs) != 1 || o.OwnerIDs[0] != "42" {
		t.Fatalf("pre-seed verification failed: owners=%v err=%v", o.OwnerIDs, err)
	}

	// Unpair the owner.
	stdout, stderr, exitCode := runPairCLI(t, bin, cfgPath, "unpair", "42")
	if exitCode != 0 || stderr != "" {
		t.Fatalf("unpair failed: code=%d stderr=%q", exitCode, stderr)
	}
	if stdout != "owner 42 removed\n" {
		t.Fatalf("unpair stdout = %q, want %q", stdout, "owner 42 removed\n")
	}

	// Verify owner is removed on disk.
	o, err = store.Load()
	if err != nil {
		t.Fatalf("reload owners.json: %v", err)
	}
	if len(o.OwnerIDs) != 0 {
		t.Fatalf("expected 0 owners after unpair, got %v", o.OwnerIDs)
	}

	// Idempotent unpair for nonexistent owner succeeds (§3).
	stdout, stderr, exitCode = runPairCLI(t, bin, cfgPath, "unpair", "99999")
	if exitCode != 0 || stderr != "" {
		t.Fatalf("idempotent unpair failed: code=%d stderr=%q", exitCode, stderr)
	}
	if stdout != "owner 99999 removed\n" {
		t.Fatalf("idempotent unpair stdout = %q, want %q", stdout, "owner 99999 removed\n")
	}
}

func TestPairSubcommandRegistrationAndUsage(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, _ := hearthHome(t)

	// Unknown pair subcommand prints usage and exits 2.
	_, stderr, code := runPairCLI(t, bin, cfgPath, "unknown")
	if code != 2 {
		t.Fatalf("unknown subcmd code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage: hearthd pair create|revoke --all|unpair <user_id>") {
		t.Fatalf("stderr = %q, want pair usage line", stderr)
	}

	// No pair subcommand prints usage and exits 2.
	_, stderr, code = runHearthdCLI(t, bin, "-config", cfgPath, "pair")
	if code != 2 {
		t.Fatalf("missing subcmd code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage: hearthd pair create|revoke --all|unpair <user_id>") {
		t.Fatalf("stderr = %q, want pair usage line", stderr)
	}

	// create with extra arg exits 2.
	_, stderr, code = runPairCLI(t, bin, cfgPath, "create", "extra")
	if code != 2 || !strings.Contains(stderr, "usage: hearthd pair create") {
		t.Fatalf("create extra arg code=%d stderr=%q", code, stderr)
	}

	// unpair with no user ID exits 2.
	_, stderr, code = runPairCLI(t, bin, cfgPath, "unpair")
	if code != 2 || !strings.Contains(stderr, "usage: hearthd pair unpair <user_id>") {
		t.Fatalf("unpair no args code=%d stderr=%q", code, stderr)
	}
}

func TestPairCorruptState(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, home := hearthHome(t)

	nuntiusDir := filepath.Join(home, "nuntius")
	if err := os.MkdirAll(nuntiusDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nuntiusDir, "owners.json"), []byte("{corrupt json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runPairCLI(t, bin, cfgPath, "create")
	if code != 1 {
		t.Fatalf("corrupt state code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "corrupt nuntius state") {
		t.Fatalf("stderr = %q, want corrupt state error", stderr)
	}
}
