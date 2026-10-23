package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestM6_SymlinkLockRefused verifies that symlinked lock files are refused
// with a clear error via O_NOFOLLOW per PENATUS-SPEC §4.5.3.3 (M6).
func TestM6_SymlinkLockRefused(t *testing.T) {
	root := t.TempDir()

	// 1. Symlink backup.lock to an external target
	target := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(target, []byte("innocent"), 0o600); err != nil {
		t.Fatal(err)
	}

	backupLock := filepath.Join(root, "backup.lock")
	if err := os.Symlink(target, backupLock); err != nil {
		t.Fatal(err)
	}

	locks, err := AcquireLocks(root)
	if err == nil {
		locks.Release()
		t.Fatal("AcquireLocks succeeded despite symlinked backup.lock")
	}
	if !strings.Contains(err.Error(), "backup.lock") {
		t.Fatalf("expected error mentioning backup.lock, got: %v", err)
	}

	// Remove symlink, verify AcquireLocks succeeds now
	if err := os.Remove(backupLock); err != nil {
		t.Fatal(err)
	}
	locks, err = AcquireLocks(root)
	if err != nil {
		t.Fatalf("AcquireLocks failed on clean root: %v", err)
	}
	locks.Release()

	// 2. Symlink custos.lock in subdirectory
	custosDir := filepath.Join(root, "custos")
	if err := os.MkdirAll(custosDir, 0o700); err != nil {
		t.Fatal(err)
	}
	custosLock := filepath.Join(custosDir, "custos.lock")
	_ = os.Remove(custosLock)
	if err := os.Symlink(target, custosLock); err != nil {
		t.Fatal(err)
	}

	locks, err = AcquireLocks(root)
	if err == nil {
		locks.Release()
		t.Fatal("AcquireLocks succeeded despite symlinked custos.lock")
	}
	if !strings.Contains(err.Error(), "custos.lock") {
		t.Fatalf("expected error mentioning custos.lock, got: %v", err)
	}
}
