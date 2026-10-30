package surface

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	revoked, err := store.RevokeLabel("test-label")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked != 1 {
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

	revoked, err := store.RevokeLabel("nonexistent")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked != 0 {
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

	revoked, err := store.RevokeLabel("temp-label")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked != 1 {
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

func TestTokenStoreExpiryRoundtrip(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens.json")

	store, err := OpenTokenStore(path)
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}

	// Create with --expires-in path at the store level:
	// CreateExpiring(label, time.Hour) sets a parseable future Expires.
	tokFuture, err := store.CreateExpiring("future", time.Hour)
	if err != nil {
		t.Fatalf("CreateExpiring future: %v", err)
	}

	// CreateExpiring(label, 0) leaves Expires empty.
	tokNever, err := store.CreateExpiring("never", 0)
	if err != nil {
		t.Fatalf("CreateExpiring never: %v", err)
	}

	entries, err := store.readEntries()
	if err != nil {
		t.Fatalf("readEntries: %v", err)
	}
	var entryFuture, entryNever *tokenEntry
	for i := range entries {
		if entries[i].Label == "future" {
			entryFuture = &entries[i]
		}
		if entries[i].Label == "never" {
			entryNever = &entries[i]
		}
	}
	if entryFuture == nil || entryNever == nil {
		t.Fatalf("missing created entries")
	}

	if entryNever.Expires != "" {
		t.Errorf("entryNever.Expires = %q, want empty", entryNever.Expires)
	}
	parsedExp, err := time.Parse(time.RFC3339, entryFuture.Expires)
	if err != nil {
		t.Fatalf("entryFuture.Expires %q failed to parse: %v", entryFuture.Expires, err)
	}
	if !parsedExp.After(time.Now().UTC()) {
		t.Errorf("entryFuture.Expires %v is not in future", parsedExp)
	}

	// Create + Verify roundtrip still passes: future and never both verify
	if !store.Verify(tokFuture) {
		t.Errorf("Verify(tokFuture) = false, want true")
	}
	if !store.Verify(tokNever) {
		t.Errorf("Verify(tokNever) = false, want true")
	}

	// An entry whose Expires is in the past fails Verify
	pastTime := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	entryPast := tokenEntry{
		Hash:    entryFuture.Hash,
		Label:   "past",
		Created: time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339),
		Expires: pastTime,
	}
	data, err := json.Marshal([]tokenEntry{entryPast})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if store.Verify(tokFuture) {
		t.Errorf("Verify(past token) = true, want false (entry in past must fail Verify)")
	}
}

func TestTokenStoreVerifyFailsClosedOnUnparseable(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens.json")

	store, err := OpenTokenStore(path)
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}

	tok, err := store.Create("unparseable-test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	entries, err := store.readEntries()
	if err != nil {
		t.Fatalf("readEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	entries[0].Expires = "not-a-valid-rfc3339-timestamp"
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if store.Verify(tok) {
		t.Errorf("Verify(unparseable expiry) = true, want false (fail closed)")
	}
}

func TestTokenStoreCreateExpiringPruning(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens.json")

	store, err := OpenTokenStore(path)
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}

	// Write entries directly: one expired, one unparseable, one never-expiring, one future-expiring
	now := time.Now().UTC()
	past := now.Add(-time.Hour).Format(time.RFC3339)
	future := now.Add(time.Hour).Format(time.RFC3339)

	initial := []tokenEntry{
		{Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Label: "expired", Created: past, Expires: past},
		{Hash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Label: "unparseable", Created: past, Expires: "invalid-time"},
		{Hash: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Label: "never", Created: past, Expires: ""},
		{Hash: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", Label: "future", Created: past, Expires: future},
	}
	data, err := json.Marshal(initial)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// CreateExpiring prunes already-expired entry and preserves never, future, and unparseable
	_, err = store.CreateExpiring("new-token", 30*time.Minute)
	if err != nil {
		t.Fatalf("CreateExpiring: %v", err)
	}

	entries, err := store.readEntries()
	if err != nil {
		t.Fatalf("readEntries: %v", err)
	}

	labels := make(map[string]bool)
	for _, e := range entries {
		labels[e.Label] = true
	}

	if labels["expired"] {
		t.Errorf("expired entry was not pruned by CreateExpiring")
	}
	if !labels["never"] {
		t.Errorf("never-expiring entry was unexpectedly pruned")
	}
	if !labels["future"] {
		t.Errorf("future-expiring entry was unexpectedly pruned")
	}
	if !labels["unparseable"] {
		t.Errorf("unparseable entry was pruned (spec: unparseable is Verify concern, not dropped by CreateExpiring)")
	}
	if !labels["new-token"] {
		t.Errorf("new-token was not written")
	}
}

func TestTokenStoreRevokeLabel(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens.json")

	store, err := OpenTokenStore(path)
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}

	// Create 3 entries with the same label
	_, err = store.Create("dupe")
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	_, err = store.Create("dupe")
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	_, err = store.Create("dupe")
	if err != nil {
		t.Fatalf("Create 3: %v", err)
	}
	_, err = store.Create("other")
	if err != nil {
		t.Fatalf("Create other: %v", err)
	}

	// RevokeLabel removes N duplicates and returns the count
	removed, err := store.RevokeLabel("dupe")
	if err != nil {
		t.Fatalf("RevokeLabel: %v", err)
	}
	if removed != 3 {
		t.Errorf("RevokeLabel returned %d, want 3", removed)
	}

	entries, err := store.readEntries()
	if err != nil {
		t.Fatalf("readEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Label != "other" {
		t.Errorf("remaining entries = %+v, want only 'other'", entries)
	}
}

func TestTokenStoreRevokeHashPrefix(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens.json")

	store, err := OpenTokenStore(path)
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}

	// Create two entries with the same label
	tok1, err := store.Create("shared-label")
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	tok2, err := store.Create("shared-label")
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}

	infos, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("expected 2 tokens, got %d", len(infos))
	}

	prefix1 := infos[0].Prefix
	prefix2 := infos[1].Prefix
	if prefix1 == prefix2 {
		t.Fatalf("prefixes must differ: %s vs %s", prefix1, prefix2)
	}

	// Short prefix errors
	_, err = store.RevokeHashPrefix("1234567") // 7 chars
	if err == nil || !strings.Contains(err.Error(), "token id prefix must be at least 8 hex characters (use the prefix from token list)") {
		t.Errorf("short prefix error = %v, want at least 8 hex characters error", err)
	}

	// Unknown prefix returns 0, nil
	removed, err := store.RevokeHashPrefix("ffffffffffff")
	if err != nil {
		t.Fatalf("unknown prefix: %v", err)
	}
	if removed != 0 {
		t.Errorf("unknown prefix removed = %d, want 0", removed)
	}

	// Single match removes exactly that row and sibling with same label survives
	removed, err = store.RevokeHashPrefix(prefix1)
	if err != nil {
		t.Fatalf("RevokeHashPrefix(prefix1): %v", err)
	}
	if removed != 1 {
		t.Errorf("RevokeHashPrefix removed = %d, want 1", removed)
	}

	surviving, err := store.List()
	if err != nil {
		t.Fatalf("List after revoke: %v", err)
	}
	if len(surviving) != 1 || surviving[0].Prefix != prefix2 {
		t.Fatalf("surviving entries = %+v, want prefix %s", surviving, prefix2)
	}

	// Verify sibling token still verifies, while revoked token fails
	if store.Verify(tok1) && !store.Verify(tok2) {
		t.Errorf("tok1 should have been revoked")
	}
	if !store.Verify(tok1) && !store.Verify(tok2) {
		t.Errorf("tok2 should still verify")
	}

	// Ambiguous (two entries sharing a forged 8-hex prefix — write tokens.json by hand) errors
	forgedEntries := []tokenEntry{
		{Hash: "1122334455667788aaaa", Label: "ambig1", Created: time.Now().UTC().Format(time.RFC3339)},
		{Hash: "1122334455667788bbbb", Label: "ambig2", Created: time.Now().UTC().Format(time.RFC3339)},
	}
	forgedData, err := json.Marshal(forgedEntries)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, forgedData, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	removed, err = store.RevokeHashPrefix("1122334455667788")
	if err == nil {
		t.Fatalf("expected error on ambiguous prefix, got nil (removed=%d)", removed)
	}
	if removed != 0 {
		t.Errorf("ambiguous prefix returned removed = %d, want 0", removed)
	}
	wantAmbigErr := "prefix matches 2 tokens; extend the prefix"
	if err.Error() != wantAmbigErr {
		t.Errorf("ambiguous error = %q, want %q", err.Error(), wantAmbigErr)
	}
}

func TestTokenStoreListExpiry(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens.json")

	store, err := OpenTokenStore(path)
	if err != nil {
		t.Fatalf("OpenTokenStore: %v", err)
	}

	now := time.Now().UTC()
	past := now.Add(-time.Hour).Format(time.RFC3339)
	future := now.Add(time.Hour).Format(time.RFC3339)

	entries := []tokenEntry{
		{Hash: "1111111111111111111111111111111111111111111111111111111111111111", Label: "a_future", Created: now.Format(time.RFC3339), Expires: future},
		{Hash: "2222222222222222222222222222222222222222222222222222222222222222", Label: "b_never", Created: now.Format(time.RFC3339), Expires: ""},
		{Hash: "3333333333333333333333333333333333333333333333333333333333333333", Label: "c_past", Created: now.Format(time.RFC3339), Expires: past},
		{Hash: "4444444444444444444444444444444444444444444444444444444444444444", Label: "d_unparseable", Created: now.Format(time.RFC3339), Expires: "garbage"},
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	infos, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 4 {
		t.Fatalf("List returned %d items, want 4", len(infos))
	}

	// Check future
	if infos[0].Label != "a_future" || infos[0].Expires != future || infos[0].Expired {
		t.Errorf("infos[0] future = %+v, want Expires=%s Expired=false", infos[0], future)
	}
	// Check never
	if infos[1].Label != "b_never" || infos[1].Expires != "" || infos[1].Expired {
		t.Errorf("infos[1] never = %+v, want Expires=\"\" Expired=false", infos[1])
	}
	// Check past
	if infos[2].Label != "c_past" || infos[2].Expires != past || !infos[2].Expired {
		t.Errorf("infos[2] past = %+v, want Expires=%s Expired=true", infos[2], past)
	}
	// Check unparseable
	if infos[3].Label != "d_unparseable" || infos[3].Expires != "garbage" || !infos[3].Expired {
		t.Errorf("infos[3] unparseable = %+v, want Expires=garbage Expired=true", infos[3])
	}
}
