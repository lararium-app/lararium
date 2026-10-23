package keystore

import (
	"os"
	"path/filepath"
	"testing"
)

// L-3: keystore readLocked: a 0-byte keys.json currently fails permanently.
// Treat zero-length as empty keystore (recoverable); corrupt non-empty JSON stays
// an error. Test both.
func TestL3_ZeroLengthKeysJsonRecoverable(t *testing.T) {
	dir := t.TempDir()
	keysFile := filepath.Join(dir, "keys.json")

	// 1. Create a 0-byte keys.json file
	if err := os.WriteFile(keysFile, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}

	s := New(dir)

	// Reading a 0-byte keys.json must succeed and return an empty map
	got, err := s.Read()
	if err != nil {
		t.Fatalf("expected 0-byte keys.json to read as empty map, got err: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map from 0-byte keys.json, got: %v", got)
	}

	// Must be recoverable: subsequent Set must succeed and overwrite cleanly
	if err := s.Set("alpha", "recovered-key-123"); err != nil {
		t.Fatalf("Set on previously 0-byte keystore: %v", err)
	}

	gotAfter, err := s.Read()
	if err != nil {
		t.Fatalf("Read after Set: %v", err)
	}
	if gotAfter["alpha"] != "recovered-key-123" {
		t.Fatalf("got %q, want %q", gotAfter["alpha"], "recovered-key-123")
	}
}

func TestL3_CorruptNonEmptyKeysJsonFails(t *testing.T) {
	dir := t.TempDir()
	keysFile := filepath.Join(dir, "keys.json")

	// Create corrupt non-empty JSON
	if err := os.WriteFile(keysFile, []byte("{not-valid-json"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := New(dir)

	// Reading corrupt non-empty keys.json must fail
	got, err := s.Read()
	if err == nil {
		t.Fatalf("expected error reading corrupt keys.json, got map: %v", got)
	}
}
