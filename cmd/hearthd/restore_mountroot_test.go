package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/lararium-app/lararium/internal/backup"
)

// #119: rename(2) EBUSY on a mount-root target must fall back to the
// in-place copy swap and still produce a fully restored root.

func TestRestoreReplace_MountRoot_EBUSY_CopyFallback(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)
	bundle := filepath.Join(t.TempDir(), "mrr.lararium-backup")
	if err := backup.CreateOffline(home, cfgPath, bundle, false, nil); err != nil {
		t.Fatal(err)
	}

	targetRoot := createTestHearthRoot(t)

	prevRename := backup.RenameFunc
	backup.RenameFunc = func(src, dst string) error {
		return &os.PathError{Op: "rename", Path: src, Err: syscall.EBUSY}
	}
	defer func() { backup.RenameFunc = prevRename }()

	if err := backup.RestoreReplace(bundle, targetRoot, true, false); err != nil {
		t.Fatalf("EBUSY fallback restore failed: %v", err)
	}

	// Restored content must match the source home file-for-file.
	want, err := os.ReadFile(filepath.Join(home, "SOUL.md"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(targetRoot, "SOUL.md"))
	if err != nil {
		t.Fatalf("SOUL.md missing after copy fallback: %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Fatal("restored SOUL.md differs from source")
	}

	tp := filepath.Dir(targetRoot)
	// No residue: no marker, no staging, no pre-swap dir.
	if m, _ := filepath.Glob(filepath.Join(tp, ".restore-*.json")); len(m) != 0 {
		t.Fatalf("marker residue: %v", m)
	}
	if m, _ := filepath.Glob(targetRoot + ".staging-*"); len(m) != 0 {
		t.Fatalf("staging residue: %v", m)
	}
	if m, _ := filepath.Glob(targetRoot + ".pre-swap-*"); len(m) != 0 {
		t.Fatalf("pre-swap residue: %v", m)
	}
	// Safety bundle kept and verifiable.
	safety, _ := filepath.Glob(filepath.Join(tp, "*.pre-restore-*.lararium-backup"))
	if len(safety) == 0 {
		t.Fatal("expected safety bundle from fallback path")
	}
	var out bytes.Buffer
	if findings, err := backup.Verify(safety[0], &out, &out); err != nil || len(findings) != 0 {
		t.Fatalf("safety bundle verify failed: err=%v findings=%+v", err, findings)
	}
}

func TestRecoverSwap_CopyMode_ResumesInterruptedCopy(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)
	bundle := filepath.Join(t.TempDir(), "crx.lararium-backup")
	if err := backup.CreateOffline(home, cfgPath, bundle, false, nil); err != nil {
		t.Fatal(err)
	}

	targetRoot := createTestHearthRoot(t)

	prevRename := backup.RenameFunc
	backup.RenameFunc = func(src, dst string) error {
		return &os.PathError{Op: "rename", Path: src, Err: syscall.EBUSY}
	}
	defer func() { backup.RenameFunc = prevRename }()

	backup.SwapHook = func(step string) {
		if step == "post-clear/pre-copy" {
			panic("crash:mid-copy")
		}
	}
	func() {
		defer func() { _ = recover() }()
		_ = backup.RestoreReplace(bundle, targetRoot, true, false)
	}()
	backup.SwapHook = nil

	// Between clear and copy the root is bare; recovery must replay the copy.
	if err := backup.RecoverSwap(targetRoot); err != nil {
		t.Fatalf("RecoverSwap copy resume failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetRoot, "SOUL.md")); err != nil {
		t.Fatalf("copy not replayed: %v", err)
	}
	tp := filepath.Dir(targetRoot)
	if m, _ := filepath.Glob(filepath.Join(tp, ".restore-*.json")); len(m) != 0 {
		t.Fatalf("marker residue after resume: %v", m)
	}
	if m, _ := filepath.Glob(targetRoot + ".staging-*"); len(m) != 0 {
		t.Fatalf("staging residue after resume: %v", m)
	}
}

func TestRecoverSwap_CopyMode_StagingGoneCompletes(t *testing.T) {
	targetRoot := createTestHearthRoot(t)
	tp := filepath.Dir(targetRoot)
	markerPath := filepath.Join(tp, ".restore-2099.01.01-00:00:00.json")
	marker := map[string]string{
		"staging": targetRoot + ".staging-2099.01.01-00:00:00", // does not exist
		"target":  targetRoot,
		"ts":      "2099.01.01-00:00:00",
		"mode":    "copy",
	}
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backup.RecoverSwap(targetRoot); err != nil {
		t.Fatalf("RecoverSwap copy row-2 failed: %v", err)
	}
	if _, err := os.Lstat(markerPath); err == nil {
		t.Fatal("marker should be deleted after copy row-2")
	}
	// Existing root content untouched.
	if _, err := os.Stat(filepath.Join(targetRoot, "SOUL.md")); err != nil {
		t.Fatal("existing root must remain intact")
	}
}
