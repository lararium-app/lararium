package custos

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupTestVaultL9(t *testing.T) (*Vault, string, string) {
	t.Helper()
	SetTestAgeWorkFactor(10)
	base, err := os.MkdirTemp("/tmp", "lrv-l9")
	if err != nil {
		t.Fatalf("short tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	dir := filepath.Join(base, "h")
	stateDir := filepath.Join(dir, "custos")
	passphrase := "test-secret-passphrase-l9"
	v := NewVault(stateDir, 5*time.Second)
	return v, stateDir, passphrase
}

// TestL9_UnlockFailureReasonClosedSet asserts that failed unlock audit records
// contain only mapped reason tokens from the closed set, never arbitrary error strings (L-9).
func TestL9_UnlockFailureReasonClosedSet(t *testing.T) {
	v, stateDir, pass := setupTestVaultL9(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	allowedReasons := map[string]bool{
		AuditReasonBadCredential: true,
		AuditReasonVaultLocked:   true,
		AuditReasonStoreError:    true,
	}

	// 1. Failed unlock with bad credentials -> bad-credential
	err := v.Unlock("wrong-passphrase-attempt", false)
	if err == nil {
		t.Fatal("expected unlock with wrong passphrase to fail")
	}

	// 2. Failed unlock with tampered envelope -> store-error
	vaultAgePath := filepath.Join(stateDir, "vault.age")
	b, err := os.ReadFile(vaultAgePath)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(vaultAgePath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	err = v.Unlock(pass, false)
	if err == nil {
		t.Fatal("expected unlock with tampered envelope to fail")
	}

	// Read all audit records and verify lock_failed reasons
	files, err := v.Audit().ListLogFiles()
	if err != nil {
		t.Fatal(err)
	}

	lockFailedCount := 0
	foundBadCredential := false
	foundStoreError := false

	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(content), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var rec AuditRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				continue
			}
			if rec.Kind == AuditKindLockFailed {
				lockFailedCount++
				if !allowedReasons[rec.Reason] {
					t.Fatalf("lock_failed audit reason %q is not in closed set %v", rec.Reason, allowedReasons)
				}
				if rec.Reason == AuditReasonBadCredential {
					foundBadCredential = true
				}
				if rec.Reason == AuditReasonStoreError {
					foundStoreError = true
				}
			}
		}
	}

	if lockFailedCount != 2 {
		t.Fatalf("expected 2 lock_failed records, got %d", lockFailedCount)
	}
	if !foundBadCredential {
		t.Fatalf("expected lock_failed with reason %q", AuditReasonBadCredential)
	}
	if !foundStoreError {
		t.Fatalf("expected lock_failed with reason %q", AuditReasonStoreError)
	}
}

// TestL9_MapLockFailureReason verifies the error classification mapping (L-9).
func TestL9_MapLockFailureReason(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{ErrIncorrectPassphrase, AuditReasonBadCredential},
		{ErrEmpty, AuditReasonBadCredential},
		{ErrCustosLocked, AuditReasonVaultLocked},
		{ErrLockTimeout, AuditReasonVaultLocked},
		{ErrVaultEnvelopeCorrupt, AuditReasonStoreError},
		{ErrSnapshotCorrupt, AuditReasonStoreError},
		{ErrUnexpectedFile, AuditReasonStoreError},
		{os.ErrNotExist, AuditReasonStoreError},
		{os.ErrPermission, AuditReasonStoreError},
		{fmt.Errorf("arbitrary internal filesystem driver error"), AuditReasonStoreError},
		{nil, ""},
	}

	for _, tc := range tests {
		got := MapLockFailureReason(tc.err)
		if got != tc.want {
			t.Errorf("MapLockFailureReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
