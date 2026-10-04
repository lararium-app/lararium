package surface

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	var seenDigit, seenUpper, seenLower bool
	for range 500 {
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
			if c >= '0' && c <= '9' {
				seenDigit = true
			}
			if c >= 'A' && c <= 'Z' {
				seenUpper = true
			}
			if c >= 'a' && c <= 'z' {
				seenLower = true
			}
		}
	}
	if !seenDigit || !seenUpper || !seenLower {
		t.Errorf("token alphabet distribution missing classes: digits=%v, upper=%v, lower=%v", seenDigit, seenUpper, seenLower)
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

func TestTokenStoreSequentialSavesRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens.json")

	store, err := OpenTokenStore(path)
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}

	tok1, err := store.Create("label-1")
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	tok2, err := store.Create("label-2")
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}

	if !store.Verify(tok1) {
		t.Errorf("Verify(tok1) = false, want true")
	}
	if !store.Verify(tok2) {
		t.Errorf("Verify(tok2) = false, want true")
	}

	entries, err := store.readEntries()
	if err != nil {
		t.Fatalf("readEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries count = %d, want 2", len(entries))
	}
	if entries[0].Label != "label-1" || entries[1].Label != "label-2" {
		t.Errorf("labels = [%q, %q], want [label-1, label-2]", entries[0].Label, entries[1].Label)
	}
}

func TestTokenStoreCreateRevokeDirectoryCleanAndMode(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens.json")

	store, err := OpenTokenStore(path)
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}

	tok, err := store.Create("temp-label")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !store.Verify(tok) {
		t.Fatal("Verify(tok) = false, want true")
	}

	revoked, err := store.Revoke("temp-label")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !revoked {
		t.Fatal("Revoke = false, want true")
	}

	// After Create+Revoke, directory holds ONLY the store file (no .tmp)
	dirEntries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(dirEntries) != 1 || dirEntries[0].Name() != filepath.Base(path) {
		names := make([]string, len(dirEntries))
		for i, e := range dirEntries {
			names[i] = e.Name()
		}
		t.Fatalf("unexpected files in directory: %v, want only %s", names, filepath.Base(path))
	}

	// Mode 0600
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0600", perm)
	}
}

func TestTokenStoreConcurrentCreate(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens.json")

	store1, err := OpenTokenStore(path)
	if err != nil {
		t.Fatalf("OpenTokenStore 1: %v", err)
	}
	store2, err := OpenTokenStore(path)
	if err != nil {
		t.Fatalf("OpenTokenStore 2: %v", err)
	}

	var (
		tok1, tok2 string
		err1, err2 error
		wg         sync.WaitGroup
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		tok1, err1 = store1.Create("worker-1")
	}()
	go func() {
		defer wg.Done()
		tok2, err2 = store2.Create("worker-2")
	}()
	wg.Wait()

	if err1 != nil {
		t.Fatalf("store1.Create: %v", err1)
	}
	if err2 != nil {
		t.Fatalf("store2.Create: %v", err2)
	}

	if tok1 == "" || tok2 == "" {
		t.Fatal("empty token generated")
	}
	if tok1 == tok2 {
		t.Fatal("tokens must be distinct")
	}

	// Both entries survive in the file
	if !store1.Verify(tok1) {
		t.Errorf("store1.Verify(tok1) failed")
	}
	if !store1.Verify(tok2) {
		t.Errorf("store1.Verify(tok2) failed")
	}
	if !store2.Verify(tok1) {
		t.Errorf("store2.Verify(tok1) failed")
	}
	if !store2.Verify(tok2) {
		t.Errorf("store2.Verify(tok2) failed")
	}

	entries, err := store1.readEntries()
	if err != nil {
		t.Fatalf("readEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries count = %d, want 2", len(entries))
	}

	labels := map[string]bool{entries[0].Label: true, entries[1].Label: true}
	if !labels["worker-1"] || !labels["worker-2"] {
		t.Errorf("surviving labels = %v, want worker-1 and worker-2", labels)
	}
}
