package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/nuntius"
)

func TestPairCreateDeviceCLI(t *testing.T) {
	bin := buildCLI(t)
	cfgPath, home := hearthHome(t)

	// Pre-pair an owner to prove `pair create --device` has NO owner-exists gate.
	ownerStore, err := nuntius.NewPairStore(filepath.Join(home, "nuntius"))
	if err != nil {
		t.Fatalf("NewPairStore: %v", err)
	}
	now := time.Now()
	ownerCode, err := ownerStore.Mint(now, 15*time.Minute)
	if err != nil {
		t.Fatalf("Mint owner: %v", err)
	}
	if res, err := ownerStore.Redeem("owner-1", ownerCode, now); err != nil || res != nuntius.RedeemPaired {
		t.Fatalf("Redeem owner: res=%v err=%v", res, err)
	}

	// 1. pair create --device prints dev1_ code on stdout + 24h TTL and POST /v1/pair hint on stderr.
	stdout, stderr, code := runPairCLI(t, bin, cfgPath, "create", "--device")
	if code != 0 {
		t.Fatalf("pair create --device failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	codeStr := strings.TrimRight(stdout, "\r\n")
	re := regexp.MustCompile(`^dev1_[0-9A-Za-z]{20}$`)
	if !re.MatchString(codeStr) {
		t.Fatalf("device code %q does not match %s", codeStr, re.String())
	}
	if !strings.Contains(stderr, codeStr) || !strings.Contains(stderr, "24h") || !strings.Contains(stderr, "POST /v1/pair") {
		t.Fatalf("stderr = %q, want code + 24h expiry + POST /v1/pair", stderr)
	}

	// 2. pair create --device --stdout prints ONLY the code.
	stdout2, stderr2, code2 := runPairCLI(t, bin, cfgPath, "create", "--device", "--stdout")
	if code2 != 0 {
		t.Fatalf("pair create --device --stdout failed: code=%d stderr=%q", code2, stderr2)
	}
	codeStr2 := strings.TrimRight(stdout2, "\r\n")
	if !re.MatchString(codeStr2) {
		t.Fatalf("stdout2 %q does not match %s", codeStr2, re.String())
	}
	if stderr2 != "" {
		t.Fatalf("pair create --device --stdout stderr = %q, want empty", stderr2)
	}

	// 3. pair status shows pending device enrollment count & expiries, never hashes or codes.
	stOut, stErr, stCode := runPairCLI(t, bin, cfgPath, "status")
	if stCode != 0 || stErr != "" {
		t.Fatalf("pair status failed: code=%d stderr=%q", stCode, stErr)
	}
	if !strings.Contains(stOut, "device enrollment codes: 2") {
		t.Fatalf("pair status stdout = %q, want device enrollment codes: 2", stOut)
	}
	if strings.Contains(stOut, codeStr) || strings.Contains(stOut, codeStr2) || strings.Contains(stOut, "dev1_") {
		t.Fatalf("pair status leaked code material: %q", stOut)
	}

	// 4. pair revoke --all clears BOTH stores and prints both counts.
	// Also seed 1 pending owner code directly so both counts are non-zero.
	if _, err := ownerStore.Mint(now, 15*time.Minute); err != nil {
		t.Fatalf("Mint extra owner code: %v", err)
	}
	revOut, revErr, revCode := runPairCLI(t, bin, cfgPath, "revoke", "--all")
	if revCode != 0 || revErr != "" {
		t.Fatalf("pair revoke --all failed: code=%d stderr=%q", revCode, revErr)
	}
	if !strings.Contains(revOut, "owner: 1") || !strings.Contains(revOut, "device: 2") {
		t.Fatalf("pair revoke --all stdout = %q, want owner: 1 and device: 2", revOut)
	}

	// Confirm enroll.json exists with 0600 mode.
	fi, err := os.Stat(filepath.Join(home, "custos", "enroll.json"))
	if err != nil {
		t.Fatalf("stat enroll.json: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("enroll.json mode = %04o, want 0600", fi.Mode().Perm())
	}
}
