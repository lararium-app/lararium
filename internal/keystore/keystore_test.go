package keystore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSetReadRemoveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	got, err := s.Read()
	if err != nil || len(got) != 0 {
		t.Fatalf("empty store: got %v, %v; want empty, nil", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keys.json")); !os.IsNotExist(err) {
		t.Fatalf("Read must not create keys.json")
	}

	if err := s.Set("alpha", "  sk-test-1\n"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err = s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got["alpha"] != "sk-test-1" {
		t.Fatalf("trimmed storage: got %q, want %q", got["alpha"], "sk-test-1")
	}

	ok, err := s.Remove("alpha")
	if err != nil || !ok {
		t.Fatalf("Remove present: got (%v, %v); want (true, nil)", ok, err)
	}

	// Absent removal: (false, nil), file untouched.
	before, _ := os.ReadFile(filepath.Join(dir, "keys.json"))
	ok, err = s.Remove("alpha")
	if err != nil || ok {
		t.Fatalf("Remove absent: got (%v, %v); want (false, nil)", ok, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "keys.json"))
	if string(before) != string(after) {
		t.Fatalf("Remove absent modified keys.json")
	}
}

func TestValidName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"openrouter", true},
		{"a-b_c9", true},
		{"a-b_C9", false}, // uppercase rejected by the frozen lowercase-only gate
		{"a", true},
		{"0lead", true},
		{strings.Repeat("x", 64), true},
		{"", false},
		{"Up", false},
		{"__proto__", false},
		{"..", false},
		{"a/b", false},
		{"a b", false},
		{"-lead", false},
		{"_lead", false},
		{strings.Repeat("x", 65), false},
	}
	for _, c := range cases {
		if got := ValidName(c.name); got != c.want {
			t.Errorf("ValidName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSetValidation(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Set("__proto__", "v"); !errors.Is(err, ErrBadName) {
		t.Errorf("bad name: got %v, want ErrBadName", err)
	}
	if err := s.Set("ok", "   "); !errors.Is(err, ErrEmpty) {
		t.Errorf("whitespace key: got %v, want ErrEmpty", err)
	}
	if err := s.Set("ok", ""); !errors.Is(err, ErrEmpty) {
		t.Errorf("empty key: got %v, want ErrEmpty", err)
	}
}

func TestCap(t *testing.T) {
	s := New(t.TempDir())
	for i := range MaxKeys {
		name := fmt.Sprintf("k%02d", i)
		if err := s.Set(name, "v"); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	if err := s.Set("overflow", "v"); !errors.Is(err, ErrFull) {
		t.Fatalf("65th new name: got %v, want ErrFull", err)
	}
	// Overwrite at cap is always allowed.
	if err := s.Set("k00", "v2"); err != nil {
		t.Fatalf("overwrite at cap: got %v, want nil", err)
	}
}

func TestModes(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	dir := t.TempDir()
	s := New(filepath.Join(dir, "hearth"))
	if err := s.Set("alpha", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	for path, want := range map[string]os.FileMode{
		s.path:     0o600,
		s.lockPath: 0o600,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s mode = %o, want %o", filepath.Base(path), fi.Mode().Perm(), want)
		}
	}
	di, err := os.Stat(filepath.Join(dir, "hearth"))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o, want 0700", di.Mode().Perm())
	}
}

func TestConcurrentSets(t *testing.T) {
	s := New(t.TempDir())
	done := make(chan error, 8)
	for i := range 8 {
		go func(i int) {
			done <- s.Set(fmt.Sprintf("c%d", i), "v")
		}(i)
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Fatalf("concurrent Set: %v", err)
		}
	}
	got, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 8 {
		t.Fatalf("lost update: %d entries, want 8", len(got))
	}
}

func TestAtomicWriteNoTempLeftovers(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.Set("alpha", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "keys-") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp leftover: %s", e.Name())
		}
	}
}
