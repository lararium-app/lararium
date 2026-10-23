package custos_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/lararium-app/lararium/internal/custos"
)

// TestM10_SnapshotRestoreAtomic: Restore rewrites files as temp-in-same-dir +
// fsync + rename so content is correct on success, and write failures (failing
// writer or permissions) leave original files intact per CUSTOS §8.4 (M10).
func TestM10_SnapshotRestoreAtomic(t *testing.T) {
	v, _, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	key, err := v.LoadInstanceKey()
	if err != nil {
		t.Fatal(err)
	}

	// Add a credential to create gen 2 (saving snapshot of gen 1)
	err = v.Mutate(pass, func(doc *custos.VaultDoc) ([]string, error) {
		doc.Credentials["openai"] = custos.Credential{Kind: "api_key", Secret: "sk-openai-key-1"} //nolint:gosec // test key
		return []string{"openai"}, nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}

	// Read gen 2 vault.age (original bytes before restore)
	vaultPath := filepath.Join(v.StateDir(), "vault.age")
	gen2VaultBytes, err := os.ReadFile(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(gen2VaultBytes) == 0 {
		t.Fatal("gen 2 vault.age is empty")
	}

	// 1. Simulate write failure via failing writer
	custos.SetCopyPayloadForTest(func(w io.Writer, r io.Reader) (int64, error) {
		return 0, errors.New("simulated disk full / write error mid-restore")
	})

	err = v.Snapshots().Restore(1, key, v.Audit())
	custos.SetCopyPayloadForTest(nil) // restore immediately
	if err == nil {
		t.Fatal("Restore succeeded despite failing writer")
	}

	// Verify ORIGINAL files are intact after failing writer
	afterFailureBytes, err := os.ReadFile(vaultPath)
	if err != nil {
		t.Fatalf("read vault.age after failure: %v", err)
	}
	if !bytes.Equal(afterFailureBytes, gen2VaultBytes) {
		t.Fatal("original vault.age was truncated or modified on write failure")
	}

	// 2. Simulate write failure via read-only directory permissions
	if err := os.Chmod(v.StateDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	err = v.Snapshots().Restore(1, key, v.Audit())
	_ = os.Chmod(v.StateDir(), 0o700) // restore permissions
	if err == nil {
		t.Fatal("Restore succeeded despite read-only stateDir")
	}

	// Verify ORIGINAL files are intact after permission failure
	afterPermBytes, err := os.ReadFile(vaultPath)
	if err != nil {
		t.Fatalf("read vault.age after perm failure: %v", err)
	}
	if !bytes.Equal(afterPermBytes, gen2VaultBytes) {
		t.Fatal("original vault.age was truncated or modified on permission failure")
	}

	// 3. Successful restore: content is correctly updated
	if err := v.Snapshots().Restore(1, key, v.Audit()); err != nil {
		t.Fatalf("successful restore failed: %v", err)
	}

	// Verify content matches snapshot gen 1
	restoredBytes, err := os.ReadFile(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	snap1Bytes, err := os.ReadFile(filepath.Join(v.Snapshots().SnapshotDir(1), "vault.age"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restoredBytes, snap1Bytes) {
		t.Fatal("restored vault.age does not match snapshot gen 1")
	}

	// Verify vault opens cleanly at gen 1 (empty credentials)
	v.Lock()
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("unlock after restore: %v", err)
	}
	st := v.Status(false, 0)
	if st.Credentials != 0 {
		t.Fatalf("restored credentials count = %d, want 0", st.Credentials)
	}
}
