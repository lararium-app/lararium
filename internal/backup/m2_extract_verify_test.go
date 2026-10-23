package backup

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestM2_ExtractConfigVerifiesFirst: a bundle whose config member was
// swapped (content differs from the manifest digest) must be refused
// by extract-config, and nothing may land at the destination.
func TestM2_ExtractConfigVerifiesFirst(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)
	out := filepath.Join(t.TempDir(), "base.lararium-backup")
	if err := CreateOffline(root, cfg, out, false, nil); err != nil {
		t.Fatal(err)
	}

	tampered := filepath.Join(t.TempDir(), "swapped.lararium-backup")
	rezipSwapConfig(t, out, tampered)

	// Sanity: the swap is a detectable tamper.
	findings, err := Verify(tampered, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	sawTamper := false
	for _, f := range findings {
		if f.Class == "tamper" && f.Path == "config/lararium.yaml" {
			sawTamper = true
		}
	}
	if !sawTamper {
		t.Fatalf("expected config tamper finding, got %+v", findings)
	}

	dest := filepath.Join(t.TempDir(), "evil.yaml")
	err = ExtractConfig(tampered, dest)
	if err == nil {
		t.Fatal("extract-config accepted a tampered bundle")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("destination written despite tamper: %v", statErr)
	}

	// The clean bundle still extracts.
	good := filepath.Join(t.TempDir(), "good.yaml")
	if err := ExtractConfig(out, good); err != nil {
		t.Fatalf("clean bundle extract-config: %v", err)
	}
	data, _ := os.ReadFile(good)
	if len(data) == 0 || strings.Contains(string(data), "attacker") {
		t.Fatalf("unexpected extracted config: %q", data)
	}
}

// rezipSwapConfig copies every entry of src to dst, replacing the body
// of config/lararium.yaml with attacker bytes (manifest left as-is).
func rezipSwapConfig(t *testing.T, src, dst string) {
	t.Helper()
	r, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	w, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	zw := zip.NewWriter(w)

	for _, f := range r.File {
		if f.Name == "config/lararium.yaml" {
			hdr := f.FileHeader // same name/mode; body swapped
			fw, err := zw.CreateHeader(&hdr)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(fw, "provider: https://attacker.example\n")
			continue
		}
		hdr := f.FileHeader
		fw, err := zw.CreateHeader(&hdr)
		if err != nil {
			t.Fatal(err)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(fw, rc) //nolint:gosec // G110: fixture copy of our own zip entries, no untrusted input
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}
