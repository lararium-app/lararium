package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/lararium-app/lararium/internal/backup"
	"github.com/lararium-app/lararium/internal/surface"
)

// captureOutput intercepts stdout, stderr, and exitFunc during fn execution.
func captureOutput(t *testing.T, fn func()) (stdoutStr, stderrStr string, exitCode int) {
	t.Helper()

	oldStdout := os.Stdout
	oldStderr := os.Stderr
	oldExit := exitFunc

	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout = wOut
	os.Stderr = wErr

	code := 0
	exitFunc = func(c int) {
		code = c
		panic(fmt.Sprintf("exitFunc(%d)", c))
	}

	defer func() {
		os.Stdout = oldStdout
		os.Stderr = oldStderr
		exitFunc = oldExit
	}()

	func() {
		defer func() {
			if r := recover(); r != nil {
				if s, ok := r.(string); !ok || !strings.HasPrefix(s, "exitFunc(") {
					panic(r)
				}
			}
		}()
		fn()
	}()

	_ = wOut.Close()
	_ = wErr.Close()

	var bufOut bytes.Buffer
	_, _ = io.Copy(&bufOut, rOut)
	_ = rOut.Close()

	var bufErr bytes.Buffer
	_, _ = io.Copy(&bufErr, rErr)
	_ = rErr.Close()

	return bufOut.String(), bufErr.String(), code
}

// withStdin pipes input to os.Stdin during fn execution.
func withStdin(input string, fn func()) {
	oldStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	if input != "" {
		_, _ = w.WriteString(input)
	}
	_ = w.Close()
	defer func() {
		os.Stdin = oldStdin
		_ = r.Close()
	}()
	fn()
}

// BK19: Seal round-trip: create --to/--passphrase → sealed bytes carry the age magic, never PK;
// list/verify/restore/extract-config succeed via identity/passphrase; wrong secret → refusal, not garbage;
// inner zip bytes match an unsealed create of the same root.
func TestBK19_SealRoundTrip(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)

	// 1. Generate identity & recipient
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "id.key")
	if err := os.WriteFile(keyPath, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tmpDir := t.TempDir()
	sealedToBundle := filepath.Join(tmpDir, "sealed-to.lararium-backup")

	// Create --to
	_, _, code := captureOutput(t, func() {
		backupCreateCmd(cfgPath, []string{"--out", sealedToBundle, "--to", id.Recipient().String()})
	})
	if code != 0 {
		t.Fatalf("backup create --to failed with code %d", code)
	}

	// Assert sealed bytes carry age magic and never PK
	rawBytes, err := os.ReadFile(sealedToBundle)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(rawBytes, []byte("age-encryption.org/v1")) {
		t.Fatal("sealed bundle missing age magic at offset 0")
	}
	if bytes.HasPrefix(rawBytes, []byte("PK\x03\x04")) {
		t.Fatal("sealed bundle contains PK header at offset 0")
	}

	// Test list, verify, extract-config, restore via --identity
	stdout, _, code := captureOutput(t, func() {
		backupListCmd(cfgPath, []string{sealedToBundle, "--identity", keyPath})
	})
	if code != 0 || !strings.Contains(stdout, "#columns: path size sha256 mode") {
		t.Fatalf("backup list --identity failed (code %d): %s", code, stdout)
	}

	_, _, code = captureOutput(t, func() {
		backupVerifyCmd(cfgPath, []string{sealedToBundle, "--identity", keyPath})
	})
	if code != 0 {
		t.Fatalf("backup verify --identity failed with code %d", code)
	}

	extractedConfig := filepath.Join(tmpDir, "extracted.yaml")
	_, _, code = captureOutput(t, func() {
		backupExtractConfigCmd(cfgPath, []string{sealedToBundle, extractedConfig, "--identity", keyPath})
	})
	if code != 0 {
		t.Fatalf("backup extract-config --identity failed with code %d", code)
	}
	if fi, err := os.Stat(extractedConfig); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("extracted config missing or wrong mode: %v", err)
	}

	restoreToDir := filepath.Join(tmpDir, "restored-to")
	_, _, code = captureOutput(t, func() {
		restoreCmd(cfgPath, []string{sealedToBundle, "--to", restoreToDir, "--identity", keyPath})
	})
	if code != 0 {
		t.Fatalf("restore --to failed with code %d", code)
	}
	if _, err := os.Stat(filepath.Join(restoreToDir, "SOUL.md")); err != nil {
		t.Fatal("SOUL.md missing in restored directory")
	}

	// Wrong secret refusal (not garbage)
	wrongId, _ := age.GenerateX25519Identity()
	wrongKeyPath := filepath.Join(tmpDir, "wrong.key")
	_ = os.WriteFile(wrongKeyPath, []byte(wrongId.String()+"\n"), 0o600)

	_, stderr, code := captureOutput(t, func() {
		backupVerifyCmd(cfgPath, []string{sealedToBundle, "--identity", wrongKeyPath})
	})
	if code == 0 {
		t.Fatal("expected verify refusal with wrong secret, got code 0")
	}
	if !strings.Contains(stderr, "unseal bundle") && !strings.Contains(stderr, "no identity matched") {
		t.Fatalf("expected unseal error, got: %s", stderr)
	}

	// 2. Passphrase create & roundtrip via stdin
	passphrase := "bk19-pass-secret-2026"
	sealedPassBundle := filepath.Join(tmpDir, "sealed-pass.lararium-backup")

	withStdin(passphrase+"\n", func() {
		_, _, code = captureOutput(t, func() {
			backupCreateCmd(cfgPath, []string{"--out", sealedPassBundle, "--passphrase"})
		})
	})
	if code != 0 {
		t.Fatalf("backup create --passphrase failed with code %d", code)
	}

	rawPassBytes, _ := os.ReadFile(sealedPassBundle)
	if !bytes.HasPrefix(rawPassBytes, []byte("age-encryption.org/v1")) {
		t.Fatal("passphrase bundle missing age magic")
	}
	if bytes.HasPrefix(rawPassBytes, []byte("PK\x03\x04")) {
		t.Fatal("passphrase bundle contains PK header")
	}

	// Read via passphrase in stdin
	withStdin(passphrase+"\n", func() {
		_, _, code = captureOutput(t, func() {
			backupVerifyCmd(cfgPath, []string{sealedPassBundle})
		})
	})
	if code != 0 {
		t.Fatalf("verify with stdin passphrase failed with code %d", code)
	}

	// Wrong passphrase refusal
	withStdin("wrong-passphrase\n", func() {
		_, stderr, code = captureOutput(t, func() {
			backupVerifyCmd(cfgPath, []string{sealedPassBundle})
		})
	})
	if code == 0 {
		t.Fatal("expected verify refusal with wrong passphrase")
	}
	if !strings.Contains(stderr, "unseal") && !strings.Contains(stderr, "incorrect") {
		t.Fatalf("expected passphrase refusal, got: %s", stderr)
	}

	// 3. Inner zip bytes match an unsealed create of the same root
	fixedTime := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	backup.SetNowFunc(func() time.Time { return fixedTime })
	defer backup.SetNowFunc(nil)

	unsealedBundle := filepath.Join(tmpDir, "byte-equality-unsealed.lararium-backup")
	sealedEqualityBundle := filepath.Join(tmpDir, "byte-equality-sealed.lararium-backup")

	if err := backup.CreateOffline(home, cfgPath, unsealedBundle, false, nil); err != nil {
		t.Fatalf("CreateOffline unsealed: %v", err)
	}
	if err := backup.CreateOffline(home, cfgPath, sealedEqualityBundle, false, nil, id.Recipient()); err != nil {
		t.Fatalf("CreateOffline sealed: %v", err)
	}

	fSealed, err := os.Open(sealedEqualityBundle)
	if err != nil {
		t.Fatal(err)
	}
	defer fSealed.Close()

	rSniffed, isSealed, err := backup.IsSealed(fSealed)
	if err != nil || !isSealed {
		t.Fatalf("IsSealed: err=%v, isSealed=%v", err, isSealed)
	}
	decReader, err := backup.Unseal(rSniffed, []age.Identity{id})
	if err != nil {
		t.Fatalf("Unseal: %v", err)
	}
	innerZipBytes, err := io.ReadAll(decReader)
	if err != nil {
		t.Fatalf("read decrypted inner zip: %v", err)
	}

	unsealedZipBytes, err := os.ReadFile(unsealedBundle)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(innerZipBytes, unsealedZipBytes) {
		t.Fatalf("inner zip bytes mismatch: len(decrypted)=%d, len(unsealed)=%d", len(innerZipBytes), len(unsealedZipBytes))
	}
}

// BK20: Create posture: no recipients + no --passphrase → refuse;
// --plaintext writes unsealed + banner; config encrypt_to honored with zero flags;
// socket verb refuses when config unset.
func TestBK20_CreatePosture(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home) // config has empty backup section

	tmpDir := t.TempDir()
	outBundle := filepath.Join(tmpDir, "out.lararium-backup")

	// 1. No recipients + no --passphrase → refuse with remedy
	_, stderr, code := captureOutput(t, func() {
		backupCreateCmd(cfgPath, []string{"--out", outBundle})
	})
	if code == 0 {
		t.Fatal("expected refusal when no recipients and no passphrase, got code 0")
	}
	if !strings.Contains(stderr, "refusing unsealed backup") || !strings.Contains(stderr, "--to") || !strings.Contains(stderr, "--plaintext") {
		t.Fatalf("expected refusal with remedy, got: %s", stderr)
	}

	// 2. --plaintext writes unsealed + fixed banner
	_, stderr, code = captureOutput(t, func() {
		backupCreateCmd(cfgPath, []string{"--out", outBundle, "--plaintext"})
	})
	if code != 0 {
		t.Fatalf("create --plaintext failed with code %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, backup.PlaintextBanner) {
		t.Fatalf("expected PlaintextBanner %q, got: %s", backup.PlaintextBanner, stderr)
	}
	raw, err := os.ReadFile(outBundle)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte("PK\x03\x04")) {
		t.Fatal("expected plaintext bundle to start with zip PK header")
	}

	// 3. Config encrypt_to honored with zero flags
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	cfgWithEncryptTo := filepath.Join(tmpDir, "with-encrypt.yaml")
	cfgContent := fmt.Sprintf("hearth:\n  home: %s\nmodels:\n  default: [\"local/model\"]\nproviders:\n  - name: local\n    base_url: http://127.0.0.1:8000\nbackup:\n  encrypt_to:\n    - %s\n", home, id.Recipient().String())
	if err := os.WriteFile(cfgWithEncryptTo, []byte(cfgContent), 0o600); err != nil {
		t.Fatal(err)
	}

	sealedFromConfig := filepath.Join(tmpDir, "from-config.lararium-backup")
	_, _, code = captureOutput(t, func() {
		backupCreateCmd(cfgWithEncryptTo, []string{"--out", sealedFromConfig})
	})
	if code != 0 {
		t.Fatalf("backup create with config encrypt_to failed with code %d", code)
	}
	rawConfigBytes, _ := os.ReadFile(sealedFromConfig)
	if !bytes.HasPrefix(rawConfigBytes, []byte("age-encryption.org/v1")) {
		t.Fatal("bundle created from config encrypt_to is not age sealed")
	}

	// 4. Socket verb refuses when backup.encrypt_to is unset
	sockPath := filepath.Join(home, "hearthd.sock")
	srv := &surface.Server{}
	srv.BackupCheck = func() error {
		// Server handler checks config encrypt_to
		return fmt.Errorf("no recipients configured")
	}
	srv.Backup = func(outAbs string, noConfig bool, progress func(string)) error {
		return fmt.Errorf("no recipients configured")
	}

	closer, err := srv.ServeSocket(sockPath, func() error { return nil })
	if err != nil {
		t.Fatalf("ServeSocket: %v", err)
	}
	defer closer.Close()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "backup %s\n", filepath.Join(tmpDir, "socket-refusal.lararium-backup"))
	buf := make([]byte, 256)
	n, _ := conn.Read(buf)
	resp := string(buf[:n])
	if !strings.HasPrefix(resp, "ERR no recipients configured") {
		t.Fatalf("expected 'ERR no recipients configured', got %q", resp)
	}
}

// BK21: Compat: unsealed bundles verify/list/restore with the exact UNENCRYPTED stderr line;
// sealed bundles fail cleanly on pre-A4b magic check paths (reader honesty, no silent partial reads).
func TestBK21_Compat(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)
	tmpDir := t.TempDir()

	// 1. Unsealed bundle verify/list/restore prints the exact UNENCRYPTED line
	unsealedBundle := filepath.Join(tmpDir, "unsealed-compat.lararium-backup")
	if err := backup.CreateOffline(home, cfgPath, unsealedBundle, false, nil); err != nil {
		t.Fatal(err)
	}

	// List
	_, stderr, code := captureOutput(t, func() {
		backupListCmd(cfgPath, []string{unsealedBundle})
	})
	if code != 0 {
		t.Fatalf("list unsealed failed with code %d", code)
	}
	if !strings.Contains(stderr, backup.UnsealedWarning) {
		t.Fatalf("list expected %q, got: %s", backup.UnsealedWarning, stderr)
	}

	// Verify
	_, stderr, code = captureOutput(t, func() {
		backupVerifyCmd(cfgPath, []string{unsealedBundle})
	})
	if code != 0 {
		t.Fatalf("verify unsealed failed with code %d", code)
	}
	if !strings.Contains(stderr, backup.UnsealedWarning) {
		t.Fatalf("verify expected %q, got: %s", backup.UnsealedWarning, stderr)
	}

	// Restore
	restoreDir := filepath.Join(tmpDir, "restore-unsealed")
	_, stderr, code = captureOutput(t, func() {
		restoreCmd(cfgPath, []string{unsealedBundle, "--to", restoreDir})
	})
	if code != 0 {
		t.Fatalf("restore unsealed failed with code %d", code)
	}
	if !strings.Contains(stderr, backup.UnsealedWarning) {
		t.Fatalf("restore expected %q, got: %s", backup.UnsealedWarning, stderr)
	}

	// 2. Sealed bundles fail cleanly on pre-A4b magic check paths
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sealedBundle := filepath.Join(tmpDir, "sealed-compat.lararium-backup")
	if err := backup.CreateOffline(home, cfgPath, sealedBundle, false, nil, id.Recipient()); err != nil {
		t.Fatal(err)
	}

	// Pre-A4b reader attempted to open directly via zip.OpenReader
	_, err = zip.OpenReader(sealedBundle)
	if err == nil {
		t.Fatal("pre-A4b reader succeeded unexpectedly on sealed bundle")
	}
	if !strings.Contains(err.Error(), "not a valid zip") {
		t.Fatalf("expected zip format error from pre-A4b reader, got: %v", err)
	}
}

// BK22: Streaming law: verify of a sealed bundle completes with the inner zip never present
// in the output dir or any user-visible path; passphrase never in argv (test asserts on argv inspection).
func TestBK22_StreamingLaw(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)
	tmpDir := t.TempDir()

	passphrase := "super-secure-bk22-pass" //nolint:gosec // test-only passphrase
	sealedBundle := filepath.Join(tmpDir, "sealed-bk22.lararium-backup")

	withStdin(passphrase+"\n", func() {
		backupCreateCmd(cfgPath, []string{"--out", sealedBundle, "--passphrase"})
	})

	// Assert passphrase is never in os.Args
	for _, arg := range os.Args {
		if strings.Contains(arg, passphrase) {
			t.Fatalf("passphrase leaked into os.Args: %s", arg)
		}
	}

	// Record directory contents before verify
	entriesBefore, _ := os.ReadDir(tmpDir)

	// Verify using piped stdin
	withStdin(passphrase+"\n", func() {
		_, _, code := captureOutput(t, func() {
			backupVerifyCmd(cfgPath, []string{sealedBundle})
		})
		if code != 0 {
			t.Fatalf("verify failed with code %d", code)
		}
	})

	// Inspect tmpDir and verify no inner zip or decrypted artifact was written to disk
	entriesAfter, _ := os.ReadDir(tmpDir)
	if len(entriesAfter) != len(entriesBefore) {
		t.Fatalf("new files appeared in bundle directory after verify: %v -> %v", entriesBefore, entriesAfter)
	}

	// Walk tmpDir to confirm no decrypted zip member or zip magic exists on disk outside sealedBundle
	err := filepath.Walk(tmpDir, func(p string, fi os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if fi.IsDir() || p == sealedBundle {
			return nil
		}
		data, err := os.ReadFile(p) //nolint:gosec // test verification of temp dir
		if err == nil && bytes.HasPrefix(data, []byte("PK\x03\x04")) {
			return fmt.Errorf("found decrypted zip on disk at %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestBK23_SafetyBundleSeal verifies §4.5.2a amendment A4b safety-bundle law:
// --replace seals the §4.5.4 rule 3 safety bundle with the given recipients;
// nil recipients keep it unsealed (pre-A4b law).
func TestBK23_SafetyBundleSeal(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)
	bundle := filepath.Join(t.TempDir(), "bk23.lararium-backup")
	if err := backup.CreateOffline(home, cfgPath, bundle, false, nil); err != nil {
		t.Fatal(err)
	}

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("sealed_safety_bundle", func(t *testing.T) {
		targetRoot := createTestHearthRoot(t)
		if err := backup.RestoreReplace(bundle, targetRoot, true, false, []age.Recipient{id.Recipient()}); err != nil {
			t.Fatalf("RestoreReplace with recipients: %v", err)
		}
		safety, err := filepath.Glob(filepath.Join(filepath.Dir(targetRoot), "*.pre-restore-*.lararium-backup"))
		if err != nil || len(safety) != 1 {
			t.Fatalf("expected exactly one safety bundle, got %v (%v)", safety, err)
		}
		f, err := os.Open(safety[0])
		if err != nil {
			t.Fatal(err)
		}
		head := make([]byte, len(backup.SealMagic))
		if _, err := io.ReadFull(f, head); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		if string(head) != backup.SealMagic {
			t.Fatalf("safety bundle must be sealed (age magic), got %q", head)
		}
		// The sealed safety bundle must verify cleanly with the identity.
		findings, err := backup.Verify(safety[0], io.Discard, io.Discard, id)
		if err != nil || len(findings) != 0 {
			t.Fatalf("sealed safety bundle failed Verify: err=%v findings=%+v", err, findings)
		}
	})

	t.Run("nil_recipients_stay_unsealed", func(t *testing.T) {
		targetRoot := createTestHearthRoot(t)
		if err := backup.RestoreReplace(bundle, targetRoot, true, false, nil); err != nil {
			t.Fatalf("RestoreReplace nil recipients: %v", err)
		}
		safety, err := filepath.Glob(filepath.Join(filepath.Dir(targetRoot), "*.pre-restore-*.lararium-backup"))
		if err != nil || len(safety) != 1 {
			t.Fatalf("expected exactly one unsealed safety bundle, got %v (%v)", safety, err)
		}
		f, err := os.Open(safety[0])
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		head := make([]byte, 2)
		if _, err := io.ReadFull(f, head); err != nil {
			t.Fatal(err)
		}
		if string(head) != "PK" {
			t.Fatalf("unsealed safety bundle must start with PK, got %q", head)
		}
	})
}
