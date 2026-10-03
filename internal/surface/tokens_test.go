package surface

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenStore(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens.json")

	store, err := OpenTokenStore(path)
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}

	// Test create
	plaintext, err := store.Create("test-label")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if len(plaintext) != 37 { // "lar1_" + 32 chars
		t.Errorf("token length = %d, want 37", len(plaintext))
	}
	if plaintext[:5] != "lar1_" {
		t.Errorf("token prefix = %q, want lar1_", plaintext[:5])
	}

	// Test verify with correct token
	if !store.Verify(plaintext) {
		t.Errorf("Verify(plaintext) = false, want true")
	}

	// Test verify with wrong token
	if store.Verify("lar1_" + strings.Repeat("A", 32)) {
		t.Errorf("Verify(wrong) = true, want false")
	}

	// Test revoke
	revoked, err := store.Revoke("test-label")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !revoked {
		t.Errorf("Revoke returned false, want true")
	}

	// Verify revoked token fails
	if store.Verify(plaintext) {
		t.Errorf("Verify after revoke = true, want false")
	}

	// Test file mode 0600
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 0600", info.Mode().Perm())
	}

	// Test stored JSON contains no "lar1_" substring
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if bytes.Contains(data, []byte("lar1_")) {
		t.Errorf("stored JSON contains plaintext token")
	}

	// Test JSON structure
	var entries []tokenEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries after revoke = %d, want 0", len(entries))
	}
}

func TestTokenStoreVerifyReReadsFile(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens.json")

	store, err := OpenTokenStore(path)
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}

	plaintext, err := store.Create("label1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if !store.Verify(plaintext) {
		t.Errorf("initial verify failed")
	}

	// Modify file directly to revoke
	entries := []tokenEntry{}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Verify should re-read and fail
	if store.Verify(plaintext) {
		t.Errorf("Verify after file modification = true, want false (revocation is next-request)")
	}
}

func TestNewTokenFormat(t *testing.T) {
	for range 100 {
		tok := NewToken()
		if len(tok) != 37 {
			t.Errorf("token length = %d, want 37", len(tok))
		}
		if tok[:5] != "lar1_" {
			t.Errorf("token prefix = %q, want lar1_", tok[:5])
		}
		for _, c := range tok[5:] {
			if !strings.ContainsRune(tokenAlphabet, c) {
				t.Errorf("invalid char %q in token", c)
			}
		}
	}
}

func TestTokenStoreRevokeNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens.json")

	store, err := OpenTokenStore(path)
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}

	revoked, err := store.Revoke("nonexistent")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked {
		t.Errorf("Revoke(not found) = true, want false")
	}
}
