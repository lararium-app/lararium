package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/lararium-app/lararium/internal/backup"
)

// TestBK1_RoundTrip tests create → verify → --to → roundtrip bytes (§4.5.6, BK1).
func TestBK1_RoundTrip(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)

	outBundle := filepath.Join(t.TempDir(), "bk1.lararium-backup")
	if err := backup.CreateOffline(home, cfgPath, outBundle, false, nil); err != nil {
		t.Fatalf("CreateOffline failed: %v", err)
	}

	var stdout, stderr bytes.Buffer
	findings, err := backup.Verify(outBundle, &stdout, &stderr)
	if err != nil || len(findings) != 0 {
		t.Fatalf("Verify failed: err=%v, findings=%+v", err, findings)
	}

	restoredDir := filepath.Join(t.TempDir(), "restored-hearth")
	if err := backup.RestoreTo(outBundle, restoredDir); err != nil {
		t.Fatalf("RestoreTo failed: %v", err)
	}

	// Compare every file in restoredDir with home
	err = filepath.Walk(home, func(srcPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if srcPath == home {
			return nil
		}
		rel, err := filepath.Rel(home, srcPath)
		if err != nil {
			return err
		}
		if backup.IsExcluded(rel, info) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		dstPath := filepath.Join(restoredDir, rel)
		dstInfo, err := os.Lstat(dstPath)
		if err != nil {
			return fmt.Errorf("missing restored file %s: %w", rel, err)
		}

		if info.IsDir() {
			if !dstInfo.IsDir() {
				return fmt.Errorf("expected directory at %s", rel)
			}
			return nil
		}

		// Check byte equality
		srcBytes, err := os.ReadFile(srcPath) //nolint:gosec // G122: test round-trip comparator over own temp root
		if err != nil {
			return err
		}
		dstBytes, err := os.ReadFile(dstPath)
		if err != nil {
			return err
		}
		if !bytes.Equal(srcBytes, dstBytes) {
			return fmt.Errorf("content mismatch at %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("roundtrip comparison failed: %v", err)
	}
}

// TestBK6_Refusals tests all refusal cases per §4.5.6 (BK6):
// --to existing; --replace live socket answering ping; --replace w/o --yes;
// corrupt source + --replace refuses until --no-safety prints the retry command.
func TestBK6_Refusals(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)
	bundle := filepath.Join(t.TempDir(), "bk6.lararium-backup")
	if err := backup.CreateOffline(home, cfgPath, bundle, false, nil); err != nil {
		t.Fatal(err)
	}

	// 1. --to existing directory
	t.Run("to_existing_refusal", func(t *testing.T) {
		existingDir := filepath.Join(t.TempDir(), "existing")
		if err := os.MkdirAll(existingDir, 0o700); err != nil {
			t.Fatal(err)
		}
		err := backup.RestoreTo(bundle, existingDir)
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("expected refusal on existing dir, got %v", err)
		}
	})

	// 2. --replace live socket answering ping
	t.Run("replace_live_socket_refusal", func(t *testing.T) {
		// Short path on purpose: t.TempDir() embeds the test name and can
		// exceed the 108-byte unix sun_path limit for hearthd.sock.
		//nolint:usetesting // short temp path required for the unix socket
		tempDir, err := os.MkdirTemp("", "hr-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(tempDir)
		targetRoot := filepath.Join(tempDir, "hearth")
		_ = os.MkdirAll(targetRoot, 0o700)
		_ = os.WriteFile(filepath.Join(targetRoot, "SOUL.md"), []byte("# Soul\n"), 0o644)
		sockPath := filepath.Join(targetRoot, "hearthd.sock")

		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()

		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				buf := make([]byte, 16)
				n, _ := conn.Read(buf)
				if strings.HasPrefix(string(buf[:n]), "PING") {
					_, _ = conn.Write([]byte("OK\n"))
				}
				conn.Close()
			}
		}()

		// ping succeeds -> refusal
		err = backup.RestoreReplace(bundle, targetRoot, true, false)
		if err == nil || !strings.Contains(err.Error(), "running") {
			t.Fatalf("expected refusal for live daemon, got %v", err)
		}

		// Close listener -> dead socket remains -> stale socket refusal
		ln.Close()
		_ = os.WriteFile(sockPath, []byte("stale-socket"), 0o600)
		err = backup.RestoreReplace(bundle, targetRoot, true, false)
		if err == nil || !strings.Contains(err.Error(), OperatorStepStaleSocket) {
			t.Fatalf("expected stale socket refusal with %q, got %v", OperatorStepStaleSocket, err)
		}
	})

	// 3. --replace w/o --yes
	t.Run("replace_without_yes_refusal", func(t *testing.T) {
		targetRoot := createTestHearthRoot(t)
		err := backup.RestoreReplace(bundle, targetRoot, false, false)
		if err == nil || !strings.Contains(err.Error(), "requires --yes") {
			t.Fatalf("expected refusal without --yes, got %v", err)
		}
	})

	// 4. corrupt source + --replace refuses until --no-safety prints the retry command
	t.Run("corrupt_source_safety_failure_and_retry", func(t *testing.T) {
		targetRoot := createTestHearthRoot(t)

		// Introduce corruption: symlink violates tree-shape law (§4.5.2)
		symlinkPath := filepath.Join(targetRoot, "illegal-symlink")
		if err := os.Symlink(targetRoot, symlinkPath); err != nil {
			t.Fatal(err)
		}

		// Capture stderr during RestoreReplace
		oldStderr := os.Stderr
		r, w, _ := os.Pipe()
		os.Stderr = w

		err := backup.RestoreReplace(bundle, targetRoot, true, false)
		w.Close()
		os.Stderr = oldStderr

		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		r.Close()
		stderrStr := buf.String()

		if err == nil {
			t.Fatal("expected failure on corrupt source safety bundle")
		}

		// Rule 22: the --no-safety retry line is printed EXACTLY as
		// `hearthd restore <file> --replace <dir> --yes --no-safety` form with real paths substituted.
		wantRetry := fmt.Sprintf("hearthd restore %s --replace %s --yes --no-safety", bundle, targetRoot)
		if !strings.Contains(stderrStr, wantRetry) {
			t.Fatalf("stderr did not contain exact retry command: %q\nstderr was:\n%s", wantRetry, stderrStr)
		}

		// Now run with noSafety = true: must succeed!
		if err := backup.RestoreReplace(bundle, targetRoot, true, true); err != nil {
			t.Fatalf("RestoreReplace with noSafety failed: %v", err)
		}

		// Verify targetRoot now has the restored bundle contents (and illegal symlink is gone)
		if _, err := os.Lstat(filepath.Join(targetRoot, "illegal-symlink")); err == nil {
			t.Fatal("illegal symlink should not exist in restored root")
		}
	})
}

// TestBK8_SwapKillMatrix tests swap kill matrix via swapHook at every point listed (§4.5.6, BK8)
// and exercises every recovery table row.
func TestBK8_SwapKillMatrix(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)
	bundle := filepath.Join(t.TempDir(), "bk8.lararium-backup")
	if err := backup.CreateOffline(home, cfgPath, bundle, false, nil); err != nil {
		t.Fatal(err)
	}

	// 1. Crash point: staging-complete
	t.Run("crash_staging_complete", func(t *testing.T) {
		defer func() { backup.SwapHook = nil }()
		targetRoot := createTestHearthRoot(t)

		backup.SwapHook = func(step string) {
			if step == "staging-complete" {
				panic("crash:staging-complete")
			}
		}

		func() {
			defer func() { _ = recover() }()
			_ = backup.RestoreReplace(bundle, targetRoot, true, false)
		}()

		if err := backup.RecoverSwap(targetRoot); err != nil {
			t.Fatalf("RecoverSwap failed: %v", err)
		}

		// Exactly ONE complete root (old root remains)
		if _, err := os.Stat(filepath.Join(targetRoot, "SOUL.md")); err != nil {
			t.Fatal("old root incomplete after recovery")
		}

		// Bootable safety bundle exists
		parent := filepath.Dir(targetRoot)
		safetyMatches, _ := filepath.Glob(filepath.Join(parent, "*.pre-restore-*.lararium-backup"))
		if len(safetyMatches) == 0 {
			t.Fatal("expected safety bundle to exist")
		}
		var out bytes.Buffer
		findings, err := backup.Verify(safetyMatches[0], &out, &out)
		if err != nil || len(findings) != 0 {
			t.Fatalf("safety bundle verify failed: err=%v, findings=%+v", err, findings)
		}
	})

	// 2. Crash point: post-marker/pre-rename-1
	t.Run("crash_post_marker_pre_rename_1", func(t *testing.T) {
		defer func() { backup.SwapHook = nil }()
		targetRoot := createTestHearthRoot(t)

		backup.SwapHook = func(step string) {
			if step == "post-marker/pre-rename-1" {
				panic("crash:post-marker")
			}
		}

		func() {
			defer func() { _ = recover() }()
			_ = backup.RestoreReplace(bundle, targetRoot, true, false)
		}()

		if err := backup.RecoverSwap(targetRoot); err != nil {
			t.Fatalf("RecoverSwap failed: %v", err)
		}

		// Row 2: marker, pre_swap absent → delete staging + marker, proceed normally.
		// Exactly ONE complete root (old root intact).
		if _, err := os.Stat(filepath.Join(targetRoot, "SOUL.md")); err != nil {
			t.Fatal("old root incomplete after row-2 recovery")
		}

		// Marker must be deleted
		parent := filepath.Dir(targetRoot)
		markers, _ := filepath.Glob(filepath.Join(parent, ".restore-*.json"))
		if len(markers) != 0 {
			t.Fatalf("expected marker to be deleted, found: %v", markers)
		}
	})

	// 3. Crash point: post-rename-1/pre-rename-2
	t.Run("crash_post_rename_1_pre_rename_2", func(t *testing.T) {
		defer func() { backup.SwapHook = nil }()
		targetRoot := createTestHearthRoot(t)

		backup.SwapHook = func(step string) {
			if step == "post-rename-1/pre-rename-2" {
				panic("crash:post-rename-1")
			}
		}

		func() {
			defer func() { _ = recover() }()
			_ = backup.RestoreReplace(bundle, targetRoot, true, false)
		}()

		// Target dir was renamed away, so it does not exist currently
		if _, err := os.Stat(targetRoot); err == nil {
			t.Fatal("targetRoot should not exist between rename-1 and rename-2")
		}

		if err := backup.RecoverSwap(targetRoot); err != nil {
			t.Fatalf("RecoverSwap failed: %v", err)
		}

		// Row 3: marker, dir absent, staging + pre_swap present → rename staging→dir, delete marker.
		// Exactly ONE complete root (new root is now at targetRoot).
		if _, err := os.Stat(filepath.Join(targetRoot, "SOUL.md")); err != nil {
			t.Fatal("new root missing after row-3 recovery")
		}

		// pre_swap sibling remains as pre-restore root
		parent := filepath.Dir(targetRoot)
		preSwaps, _ := filepath.Glob(filepath.Join(parent, "*.pre-swap-*"))
		if len(preSwaps) == 0 {
			t.Fatal("pre-swap sibling should remain")
		}
	})

	// 4. Crash point: post-rename-2/pre-marker-delete
	t.Run("crash_post_rename_2_pre_marker_delete", func(t *testing.T) {
		defer func() { backup.SwapHook = nil }()
		targetRoot := createTestHearthRoot(t)

		backup.SwapHook = func(step string) {
			if step == "post-rename-2/pre-marker-delete" {
				panic("crash:post-rename-2")
			}
		}

		func() {
			defer func() { _ = recover() }()
			_ = backup.RestoreReplace(bundle, targetRoot, true, false)
		}()

		if err := backup.RecoverSwap(targetRoot); err != nil {
			t.Fatalf("RecoverSwap failed: %v", err)
		}

		// Row 4: marker, dir present, staging absent → delete marker, done.
		parent := filepath.Dir(targetRoot)
		markers, _ := filepath.Glob(filepath.Join(parent, ".restore-*.json"))
		if len(markers) != 0 {
			t.Fatalf("expected marker to be deleted, found: %v", markers)
		}
		if _, err := os.Stat(filepath.Join(targetRoot, "SOUL.md")); err != nil {
			t.Fatal("new root missing after row-4 recovery")
		}
	})

	// 5. Table Row 5: dir + staging + pre_swap all present → refuse with rm escape
	t.Run("row5_impossible_state_refusal", func(t *testing.T) {
		targetRoot := filepath.Join(t.TempDir(), "hearth")
		_ = os.MkdirAll(targetRoot, 0o700)
		parent := filepath.Dir(targetRoot)

		stagingDir := filepath.Join(parent, "hearth.staging-fake")
		_ = os.MkdirAll(stagingDir, 0o700)
		preSwapDir := filepath.Join(parent, "hearth.pre-swap-fake")
		_ = os.MkdirAll(preSwapDir, 0o700)

		markerPath := filepath.Join(parent, ".restore-fake-row5.json")
		markerContent := fmt.Sprintf(`{"staging":%q,"pre_swap":%q,"target":%q,"ts":"fake"}`, stagingDir, preSwapDir, targetRoot)
		_ = os.WriteFile(markerPath, []byte(markerContent), 0o600)

		oldStderr := os.Stderr
		r, w, _ := os.Pipe()
		os.Stderr = w

		err := backup.RecoverSwap(targetRoot)
		w.Close()
		os.Stderr = oldStderr

		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		r.Close()
		out := buf.String()

		if err == nil {
			t.Fatal("expected refusal on row 5 impossible state")
		}
		if !strings.Contains(out, fmt.Sprintf("rm %s", markerPath)) {
			t.Fatalf("expected rm escape hatch in stderr, got: %s", out)
		}
	})

	// 6. Table Row 6: dir absent + staging absent → refuse with rm escape
	t.Run("row6_staging_deleted_refusal", func(t *testing.T) {
		targetRoot := filepath.Join(t.TempDir(), "hearth")
		// targetRoot absent
		parent := filepath.Dir(targetRoot)

		stagingDir := filepath.Join(parent, "hearth.staging-fake-missing")
		preSwapDir := filepath.Join(parent, "hearth.pre-swap-fake")
		_ = os.MkdirAll(preSwapDir, 0o700)

		markerPath := filepath.Join(parent, ".restore-fake-row6.json")
		markerContent := fmt.Sprintf(`{"staging":%q,"pre_swap":%q,"target":%q,"ts":"fake"}`, stagingDir, preSwapDir, targetRoot)
		_ = os.WriteFile(markerPath, []byte(markerContent), 0o600)

		oldStderr := os.Stderr
		r, w, _ := os.Pipe()
		os.Stderr = w

		err := backup.RecoverSwap(targetRoot)
		w.Close()
		os.Stderr = oldStderr

		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		r.Close()
		out := buf.String()

		if err == nil {
			t.Fatal("expected refusal on row 6 unrecoverable state")
		}
		if !strings.Contains(out, fmt.Sprintf("rm %s", markerPath)) {
			t.Fatalf("expected rm escape hatch in stderr, got: %s", out)
		}
	})

	// 7. Marker JSON unreadable/corrupt → refuse, print contents + rm <marker> escape hatch
	t.Run("corrupt_marker_refusal", func(t *testing.T) {
		targetRoot := filepath.Join(t.TempDir(), "hearth")
		_ = os.MkdirAll(targetRoot, 0o700)
		parent := filepath.Dir(targetRoot)

		markerPath := filepath.Join(parent, ".restore-corrupt.json")
		_ = os.WriteFile(markerPath, []byte("{not-json"), 0o600)

		oldStderr := os.Stderr
		r, w, _ := os.Pipe()
		os.Stderr = w

		err := backup.RecoverSwap(targetRoot)
		w.Close()
		os.Stderr = oldStderr

		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		r.Close()
		out := buf.String()

		if err == nil {
			t.Fatal("expected refusal on corrupt marker")
		}
		if !strings.Contains(out, fmt.Sprintf("rm %s", markerPath)) {
			t.Fatalf("expected rm escape hatch in stderr, got: %s", out)
		}
	})
}

// TestBK13_ModesAndShapes tests restore-mode round-trips:
// 04755→0755, empty dirs byte-identical, root 0700, keys.json 0600 (§4.5.6, BK13).
func TestBK13_ModesAndShapes(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)

	// Add empty directory with mode 0755
	emptyDir := filepath.Join(home, "empty-dir")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Add file with mode 04755 (setuid)
	setuidFile := filepath.Join(home, "setuid-tool")
	if err := os.WriteFile(setuidFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Chmod(setuidFile, 0o4755); err != nil {
		t.Fatal(err)
	}

	outBundle := filepath.Join(t.TempDir(), "bk13.lararium-backup")
	if err := backup.CreateOffline(home, cfgPath, outBundle, false, nil); err != nil {
		t.Fatal(err)
	}

	restoredDir := filepath.Join(t.TempDir(), "restored-bk13")
	if err := backup.RestoreTo(outBundle, restoredDir); err != nil {
		t.Fatalf("RestoreTo failed: %v", err)
	}

	// Root dir ends 0700
	rfi, err := os.Stat(restoredDir)
	if err != nil {
		t.Fatal(err)
	}
	if rfi.Mode().Perm() != 0o700 {
		t.Fatalf("restored root mode = %o, want 0700", rfi.Mode().Perm())
	}

	// keys.json mode 0600
	kfi, err := os.Stat(filepath.Join(restoredDir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if kfi.Mode().Perm() != 0o600 {
		t.Fatalf("keys.json mode = %o, want 0600", kfi.Mode().Perm())
	}

	// empty-dir byte-identical mode 0755
	efi, err := os.Stat(filepath.Join(restoredDir, "empty-dir"))
	if err != nil {
		t.Fatal(err)
	}
	if !efi.IsDir() || efi.Mode().Perm() != 0o755 {
		t.Fatalf("empty-dir mode = %o, want 0755", efi.Mode().Perm())
	}

	// source 04755 restores 0755 (setuid stripped per §4.5.2, BK13)
	sfi, err := os.Stat(filepath.Join(restoredDir, "setuid-tool"))
	if err != nil {
		t.Fatal(err)
	}
	if sfi.Mode().Perm() != 0o755 {
		t.Fatalf("setuid-tool perm = %o, want 0755", sfi.Mode().Perm())
	}
	if sfi.Mode()&os.ModeSetuid != 0 {
		t.Fatal("setuid bit was not stripped on restore")
	}
}

// TestBK16_FreshRootStdoutExtractConfig tests that fresh-root (--to) success stdout
// contains the exact hearthd backup extract-config <file> <path> command text (§4.5.6, BK16).
func TestBK16_FreshRootStdoutExtractConfig(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)
	bundle := filepath.Join(t.TempDir(), "bk16.lararium-backup")
	if err := backup.CreateOffline(home, cfgPath, bundle, false, nil); err != nil {
		t.Fatal(err)
	}

	targetDir := filepath.Join(t.TempDir(), "fresh-root")

	// Capture stdout during restoreCmd
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	restoreCmd([]string{bundle, "--to", targetDir})

	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	r.Close()
	stdoutStr := buf.String()

	wantConfigPath := filepath.Join(targetDir, "lararium.yaml")
	wantCmd := fmt.Sprintf("hearthd backup extract-config %s %s", bundle, wantConfigPath)
	if !strings.Contains(stdoutStr, wantCmd) {
		t.Fatalf("stdout did not contain exact extract-config command: %q\nstdout was:\n%s", wantCmd, stdoutStr)
	}

	// Test extracting config with ExtractConfig
	destConfig := filepath.Join(t.TempDir(), "extracted-lararium.yaml")
	if err := backup.ExtractConfig(bundle, destConfig); err != nil {
		t.Fatalf("ExtractConfig failed: %v", err)
	}
	cfi, err := os.Stat(destConfig)
	if err != nil {
		t.Fatal(err)
	}
	if cfi.Mode().Perm() != 0o600 {
		t.Fatalf("extracted config mode = %o, want 0600", cfi.Mode().Perm())
	}
}

// TestBK18_CraftedBundles tests crafted bundles:
// ../x, /x, a/../../x, drive-style, unlisted entry → restore refuses AND target subtree byte-unchanged;
// assert nothing was written by pre-validating then comparing a fingerprint of the parent dir (§4.5.6, BK18).
func TestBK18_CraftedBundles(t *testing.T) {
	evilEntries := []string{
		"../evil.txt",
		"/abs_evil.txt",
		"tree/../../escape.txt",
		"C:drive_evil.txt",
		"tree/unlisted.txt",
	}

	for _, evil := range evilEntries {
		t.Run("crafted_"+evil, func(t *testing.T) {
			parentDir := t.TempDir()
			targetDir := filepath.Join(parentDir, "target-hearth")

			// Write a dummy file in parentDir to include in fingerprint
			_ = os.WriteFile(filepath.Join(parentDir, "sentinel.txt"), []byte("sentinel"), 0o644)

			// Record fingerprint before
			fpBefore := fingerprintDir(t, parentDir)

			craftedBundle := createCraftedZip(t, evil)

			err := backup.RestoreTo(craftedBundle, targetDir)
			if err == nil {
				t.Fatalf("expected refusal for crafted member %q, got nil error", evil)
			}

			// Fingerprint of parent dir must be byte-unchanged
			fpAfter := fingerprintDir(t, parentDir)
			if fpBefore != fpAfter {
				t.Fatalf("parent dir changed after crafted bundle refusal: before=%s, after=%s", fpBefore, fpAfter)
			}

			if _, err := os.Stat(targetDir); err == nil {
				t.Fatalf("target dir was created despite crafted bundle refusal: %s", targetDir)
			}
		})
	}
}

func fingerprintDir(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(h, "%s:%o:%d\n", rel, fi.Mode(), fi.Size())
		if !fi.IsDir() {
			data, _ := os.ReadFile(p) //nolint:gosec // G122: test fingerprint walk over own temp root
			h.Write(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func createCraftedZip(t *testing.T, evilPath string) string {
	t.Helper()
	bundlePath := filepath.Join(t.TempDir(), "crafted.lararium-backup")
	f, err := os.OpenFile(bundlePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)

	// Manifest with a valid file
	validPath := "tree/SOUL.md"
	entries := []backup.ManifestEntry{
		{
			Path:   validPath,
			Mode:   "0644",
			SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
	}
	// If not the unlisted case, put evil entry into manifest so manifest parses
	if evilPath != "tree/unlisted.txt" {
		entries = append(entries, backup.ManifestEntry{
			Path:   evilPath,
			Mode:   "0644",
			SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		})
	}

	m := backup.NewManifest("/home/test", "localhost", entries, backup.CountsInfo{})
	mData, _ := json.MarshalIndent(m, "", "  ") //nolint:errchkjson // test fixture, no unsafe types

	mw, err := zw.CreateHeader(&zip.FileHeader{Name: "manifest.json", Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = mw.Write(mData)

	vw, err := zw.CreateHeader(&zip.FileHeader{Name: validPath, Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = vw.Write([]byte{})

	ew, err := zw.CreateHeader(&zip.FileHeader{Name: evilPath, Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = ew.Write([]byte("evil"))

	_ = zw.Close()
	return bundlePath
}
