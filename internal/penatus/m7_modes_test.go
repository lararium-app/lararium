package penatus

import (
	"os"
	"path/filepath"
	"testing"
)

// TestM7_SessionFileModes: session dirs 0700, session.json and
// events.jsonl 0600; OpenLog tightens modes left loose by older builds.
func TestM7_SessionFileModes(t *testing.T) {
	root := t.TempDir()

	log, err := CreateSession(root, "m7sess", "main")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "sessions", "m7sess")

	assertMode(t, dir, 0o700)
	assertMode(t, filepath.Join(dir, "session.json"), 0o600)
	assertMode(t, filepath.Join(dir, "events.jsonl"), 0o600)

	// Simulate a legacy session: loose modes on disk.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "events.jsonl"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLog(dir); err != nil {
		t.Fatal(err)
	}
	assertMode(t, dir, 0o700)
	assertMode(t, filepath.Join(dir, "events.jsonl"), 0o600)
	_ = log
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}
