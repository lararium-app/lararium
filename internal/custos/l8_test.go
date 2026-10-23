package custos

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

func setupTestVaultInternal(t *testing.T) (*Vault, string) {
	t.Helper()
	SetTestAgeWorkFactor(10)
	base := t.TempDir()
	dir := filepath.Join(base, "h")
	stateDir := filepath.Join(dir, "custos")
	passphrase := "test-secret-passphrase-1234"
	v := NewVault(stateDir, 5*time.Second)
	return v, passphrase
}

// TestL8_ZeroBytes verifies that zeroBytes clears all bytes of a slice (L-8).
func TestL8_ZeroBytes(t *testing.T) {
	b := []byte("secret-passphrase-material-here")
	zeroBytes(b)
	for i, v := range b {
		if v != 0 {
			t.Fatalf("byte at %d was %d, want 0", i, v)
		}
	}
}

// TestL8_LockZeroesDerivedKey verifies that Lock zeroes and clears v.derivedKey (L-8).
func TestL8_LockZeroesDerivedKey(t *testing.T) {
	v, pass := setupTestVaultInternal(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("unlock failed: %v", err)
	}

	v.mu.RLock()
	dkLen := len(v.derivedKey)
	v.mu.RUnlock()
	if dkLen == 0 {
		t.Fatal("expected derivedKey to be populated while unlocked")
	}

	if err := v.Lock(); err != nil {
		t.Fatalf("lock failed: %v", err)
	}

	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.derivedKey != nil {
		t.Fatalf("expected derivedKey to be nil after Lock, got len=%d", len(v.derivedKey))
	}
	if v.isUnlocked {
		t.Fatal("expected isUnlocked to be false after Lock")
	}
}

// TestL8_UnlockFailureNoDerivedKey verifies that a failed unlock does not retain derivedKey (L-8).
func TestL8_UnlockFailureNoDerivedKey(t *testing.T) {
	v, pass := setupTestVaultInternal(t)
	if err := v.Init(pass); err != nil {
		t.Fatal(err)
	}

	err := v.Unlock("definitely-wrong-passphrase", false)
	if err == nil {
		t.Fatal("expected unlock with wrong passphrase to fail")
	}

	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.derivedKey != nil {
		t.Fatalf("expected derivedKey to be nil after failed unlock, got len=%d", len(v.derivedKey))
	}
}

// TestL8_EncryptDecryptAgeWithKey verifies that EncryptAgeWithKey and DecryptAgeWithKey
// encrypt and decrypt correctly across the library boundary without leaking raw string copies (L-8).
func TestL8_EncryptDecryptAgeWithKey(t *testing.T) {
	SetTestAgeWorkFactor(10)
	key := []byte("test-passphrase-derived-key-1234")
	data := []byte("confidential-vault-document-content")

	ct, err := EncryptAgeWithKey(data, key)
	if err != nil {
		t.Fatalf("EncryptAgeWithKey failed: %v", err)
	}

	pt, err := DecryptAgeWithKey(ct, key)
	if err != nil {
		t.Fatalf("DecryptAgeWithKey failed: %v", err)
	}

	if !bytes.Equal(pt, data) {
		t.Fatalf("decrypted = %q, want %q", pt, data)
	}
}
