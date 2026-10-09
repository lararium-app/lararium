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
	if code != 0 {
		t.Fatalf("pair create failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "send '/pair <code>'") || !strings.Contains(stderr, "15m0s from now") {
		t.Fatalf("pair create stderr = %q, want human hint with 15m0s", stderr)
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
	if code != 0 {
		t.Fatalf("pair create failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "send '/pair <code>'") || !strings.Contains(stderr, "42s from now") {
		t.Fatalf("pair create stderr = %q, want 42s hint", stderr)
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
	if !strings.Contains(stderr, "usage: hearthd pair create|revoke --all|status|unpair <user_id>") {
		t.Fatalf("stderr = %q, want pair usage line", stderr)
	}

	// Bogus subcommand prints usage and exits 2 (§3).
	_, stderr, code = runPairCLI(t, bin, cfgPath, "bogus")
	if code != 2 {
		t.Fatalf("bogus subcmd code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage: hearthd pair create|revoke --all|status|unpair <user_id>") {
		t.Fatalf("stderr = %q, want pair usage line", stderr)
	}

	// No pair subcommand prints usage and exits 2.
	_, stderr, code = runHearthdCLI(t, bin, "-config", cfgPath, "pair")
	if code != 2 {
		t.Fatalf("missing subcmd code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage: hearthd pair create|revoke --all|status|unpair <user_id>") {
		t.Fatalf("stderr = %q, want pair usage line", stderr)
	}

	// create with extra arg exits 2.
	_, stderr, code = runPairCLI(t, bin, cfgPath, "create", "extra")
	if code != 2 || !strings.Contains(stderr, "usage: hearthd pair create") {
		t.Fatalf("create extra arg code=%d stderr=%q", code, stderr)
	}

	// status with extra arg exits 2.
	_, stderr, code = runPairCLI(t, bin, cfgPath, "status", "extra")
	if code != 2 || !strings.Contains(stderr, "usage: hearthd pair status") {
		t.Fatalf("status extra arg code=%d stderr=%q", code, stderr)
	}

	// unpair with no user ID exits 2.
	_, stderr, code = runPairCLI(t, bin, cfgPath, "unpair")
	if code != 2 || !strings.Contains(stderr, "usage: hearthd pair unpair <user_id>") {
		t.Fatalf("unpair no args code=%d stderr=%q", code, stderr)
	}
}

// TestPairStatusUnpairedFileAbsent verifies that pair status with no owners.json
// reports unpaired state with exit code 0 (NUNTIUS-SPEC §3).
func TestPairStatusUnpairedFileAbsent(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, _ := hearthHome(t)

	stdout, stderr, code := runPairCLI(t, bin, cfgPath, "status")
	if code != 0 || stderr != "" {
		t.Fatalf("status failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	want := "owners: none\npending codes: none\n"
	if stdout != want {
		t.Fatalf("status stdout = %q, want %q", stdout, want)
	}
}

// TestPairStatusWithCodesAndOwners verifies that status lists owner IDs and
// pending code expiry (NUNTIUS-SPEC §3).
func TestPairStatusWithCodesAndOwners(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, home := hearthHome(t)

	store, err := nuntius.NewPairStore(filepath.Join(home, "nuntius"))
	if err != nil {
		t.Fatalf("NewPairStore: %v", err)
	}
	now := time.Now()
	code1, err := store.Mint(now, 15*time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	res, err := store.Redeem("4242", code1, now)
	if err != nil || res != nuntius.RedeemPaired {
		t.Fatalf("Redeem: res=%v err=%v", res, err)
	}

	// Fresh mint
	_, err = store.Mint(now, 15*time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	stdout, stderr, code := runPairCLI(t, bin, cfgPath, "status")
	if code != 0 || stderr != "" {
		t.Fatalf("status failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	if !strings.Contains(stdout, "owners: 1\n  4242\n") {
		t.Fatalf("stdout %q does not contain owner 4242", stdout)
	}
	if !strings.Contains(stdout, "  expires in ") {
		t.Fatalf("stdout %q does not contain expires in line", stdout)
	}
}

// TestPairStatusExpiredCode verifies that already-expired pending codes
// print "  expired" in status (NUNTIUS-SPEC §3).
func TestPairStatusExpiredCode(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, home := hearthHome(t)

	nuntiusDir := filepath.Join(home, "nuntius")
	if err := os.MkdirAll(nuntiusDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	fixture := nuntius.Owners{
		Codes: []nuntius.OwnerCode{
			{
				Hash:    "dummyhash",
				Created: now - 120,
				Expires: now - 60,
			},
		},
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nuntiusDir, "owners.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runPairCLI(t, bin, cfgPath, "status")
	if code != 0 || stderr != "" {
		t.Fatalf("status failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "  expired\n") {
		t.Fatalf("stdout %q does not contain expired line", stdout)
	}
	if !strings.Contains(stdout, "owners: none\n") {
		t.Fatalf("stdout %q does not contain owners: none", stdout)
	}
}

// TestPairStatusOldestFirst verifies that pending codes are listed oldest first (NUNTIUS-SPEC §3).
func TestPairStatusOldestFirst(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, home := hearthHome(t)

	nuntiusDir := filepath.Join(home, "nuntius")
	if err := os.MkdirAll(nuntiusDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	// Out of order in JSON: code2 is newer, but listed first in JSON
	fixture := nuntius.Owners{
		Codes: []nuntius.OwnerCode{
			{
				Hash:    "hash_newer",
				Created: now - 10,
				Expires: now + 500,
			},
			{
				Hash:    "hash_older",
				Created: now - 50,
				Expires: now + 200,
			},
		},
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nuntiusDir, "owners.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runPairCLI(t, bin, cfgPath, "status")
	if code != 0 || stderr != "" {
		t.Fatalf("status failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[1], "expires in") || !strings.Contains(lines[2], "expires in") {
		t.Fatalf("expected code expiry lines, got %v", lines)
	}
	t1, err := time.ParseDuration(strings.TrimPrefix(lines[1], "  expires in "))
	if err != nil {
		t.Fatalf("parse line 1 duration: %v", err)
	}
	t2, err := time.ParseDuration(strings.TrimPrefix(lines[2], "  expires in "))
	if err != nil {
		t.Fatalf("parse line 2 duration: %v", err)
	}
	if t1 >= t2 {
		t.Fatalf("expected older code (shorter remaining) before newer code, got t1=%v t2=%v", t1, t2)
	}
}

// TestPairCreateRefusesWhilePaired verifies that pair create refuses to mint
// when the bot already has an owner (NUNTIUS-SPEC §3).
func TestPairCreateRefusesWhilePaired(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, home := hearthHome(t)

	store, err := nuntius.NewPairStore(filepath.Join(home, "nuntius"))
	if err != nil {
		t.Fatalf("NewPairStore: %v", err)
	}
	now := time.Now()
	code, err := store.Mint(now, 15*time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	res, err := store.Redeem("owner123", code, now)
	if err != nil || res != nuntius.RedeemPaired {
		t.Fatalf("Redeem: res=%v err=%v", res, err)
	}

	before, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	codesBefore := len(before.Codes)

	stdout, stderr, exitCode := runPairCLI(t, bin, cfgPath, "create")
	if exitCode != 1 {
		t.Fatalf("create while paired code=%d, want 1", exitCode)
	}
	if stdout != "" {
		t.Fatalf("create while paired stdout=%q, want empty", stdout)
	}
	wantStderr := "hearthd: already paired — run 'hearthd pair unpair <user_id>' first (owner migration: unpair, create, re-pair)\n"
	if stderr != wantStderr {
		t.Fatalf("stderr = %q, want %q", stderr, wantStderr)
	}

	after, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(after.Codes) != codesBefore {
		t.Fatalf("expected Codes count %d unchanged, got %d", codesBefore, len(after.Codes))
	}
}

// TestPairCreateUnpairedStdoutAndStderr verifies that when unpaired, create
// prints only the code to stdout and the hint to stderr (NUNTIUS-SPEC §3).
func TestPairCreateUnpairedStdoutAndStderr(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, _ := hearthHome(t)

	stdout, stderr, code := runPairCLI(t, bin, cfgPath, "create")
	if code != 0 {
		t.Fatalf("pair create failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	// stdout must match `^pair1_[0-9A-Za-z]{20}$` per line and NOTHING else on stdout
	re := regexp.MustCompile(`^pair1_[0-9A-Za-z]{20}\n$`)
	if !re.MatchString(stdout) {
		t.Fatalf("stdout %q does not match %s", stdout, re.String())
	}

	// stderr contains send '/pair and expires
	if !strings.Contains(stderr, "send '/pair") || !strings.Contains(stderr, "expires") {
		t.Fatalf("stderr %q does not contain expected hint", stderr)
	}
}

// TestPairOwnerMigrationDance verifies the full unpair -> create -> re-pair
// owner migration workflow (NUNTIUS-SPEC §3).
func TestPairOwnerMigrationDance(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, home := hearthHome(t)

	store, err := nuntius.NewPairStore(filepath.Join(home, "nuntius"))
	if err != nil {
		t.Fatalf("NewPairStore: %v", err)
	}

	// 1. Create code via CLI
	stdout, stderr, code := runPairCLI(t, bin, cfgPath, "create")
	if code != 0 {
		t.Fatalf("step 1 create failed: code=%d stderr=%q", code, stderr)
	}
	code1 := strings.TrimRight(stdout, "\r\n")

	// 2. Redeem via store
	now := time.Now()
	res, err := store.Redeem("migrating_owner", code1, now)
	if err != nil || res != nuntius.RedeemPaired {
		t.Fatalf("step 2 redeem failed: res=%v err=%v", res, err)
	}

	// 3. Status shows owner
	stdout, stderr, code = runPairCLI(t, bin, cfgPath, "status")
	if code != 0 || stderr != "" {
		t.Fatalf("step 3 status failed: code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "owners: 1\n  migrating_owner\n") {
		t.Fatalf("step 3 status stdout=%q, want migrating_owner", stdout)
	}

	// 4. Create refuses while paired
	stdout, stderr, code = runPairCLI(t, bin, cfgPath, "create")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "already paired") {
		t.Fatalf("step 4 create should refuse: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	// 5. Unpair
	stdout, stderr, code = runPairCLI(t, bin, cfgPath, "unpair", "migrating_owner")
	if code != 0 || stderr != "" || stdout != "owner migrating_owner removed\n" {
		t.Fatalf("step 5 unpair failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	// 6. Create works again
	stdout, stderr, code = runPairCLI(t, bin, cfgPath, "create")
	if code != 0 || !strings.Contains(stderr, "send '/pair") {
		t.Fatalf("step 6 create after unpair failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code2 := strings.TrimRight(stdout, "\r\n")
	re := regexp.MustCompile(`^pair1_[0-9A-Za-z]{20}$`)
	if !re.MatchString(code2) {
		t.Fatalf("step 6 code %q does not match regex", code2)
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
