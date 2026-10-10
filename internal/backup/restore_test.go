package backup

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRestore_VersionLaw tests that RestoreTo enforces version law per §4.5.4 rule 1 (BK7):
// major unknown → refuse with migration pointer;
// minor newer → proceed, drop unknown fields, report drops to stderr;
// newer product than binary → warn, proceed.
func TestRestore_VersionLaw(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)
	bundle := filepath.Join(t.TempDir(), "base.lararium-backup")
	if err := CreateOffline(root, cfg, bundle, false, nil); err != nil {
		t.Fatal(err)
	}

	// 1. Major unknown: backup/2.0 → refuse with migration pointer
	t.Run("major_unknown_refuses", func(t *testing.T) {
		bundle2 := rewriteManifest(t, bundle, func(m *Manifest, raw map[string]any) {
			m.Format = "backup/2.0"
			raw["format"] = "backup/2.0"
		})
		dest := filepath.Join(t.TempDir(), "target2")
		err := RestoreTo(bundle2, dest)
		if err == nil || !strings.Contains(err.Error(), "migration") {
			t.Fatalf("expected refusal with migration pointer for backup/2.0, got: %v", err)
		}
	})

	// 2. Minor newer: backup/1.9 with unknown fields → proceed, drop unknown fields
	t.Run("minor_newer_proceeds", func(t *testing.T) {
		bundle19 := rewriteManifest(t, bundle, func(m *Manifest, raw map[string]any) {
			m.Format = "backup/1.9"
			raw["format"] = "backup/1.9"
			raw["future_field"] = "tomorrow"
		})
		dest := filepath.Join(t.TempDir(), "target19")

		oldStderr := os.Stderr
		r, w, _ := os.Pipe()
		os.Stderr = w

		err := RestoreTo(bundle19, dest)
		w.Close()
		os.Stderr = oldStderr

		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		r.Close()
		stderrStr := buf.String()

		if err != nil {
			t.Fatalf("RestoreTo should proceed on backup/1.9, got error: %v", err)
		}
		if !strings.Contains(stderrStr, "future_field") {
			t.Fatalf("expected report drop on unknown field in stderr, got: %s", stderrStr)
		}
	})

	// 3. Newer product: hearthd/9.9.9 → warn on stderr, proceed
	t.Run("newer_product_warns_and_proceeds", func(t *testing.T) {
		bundleProd := rewriteManifest(t, bundle, func(m *Manifest, raw map[string]any) {
			m.Product = "hearthd/9.9.9"
			raw["product"] = "hearthd/9.9.9"
		})
		dest := filepath.Join(t.TempDir(), "targetProd")

		oldStderr := os.Stderr
		r, w, _ := os.Pipe()
		os.Stderr = w

		err := RestoreTo(bundleProd, dest)
		w.Close()
		os.Stderr = oldStderr

		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		r.Close()
		stderrStr := buf.String()

		if err != nil {
			t.Fatalf("RestoreTo should proceed on newer product, got error: %v", err)
		}
		if !strings.Contains(stderrStr, "newer than binary") {
			t.Fatalf("expected warning on newer product in stderr, got: %s", stderrStr)
		}
	})
}

// TestRestore_QuarantineVsTamper tests §4.5.4 rule 1 / BK3:
// tamper findings → refuse; quarantine findings → warn on stderr, proceed.
func TestRestore_QuarantineVsTamper(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)

	// Add events.jsonl with a sequence gap (quarantine class)
	sessDir := filepath.Join(root, "sessions", "q_sess")
	_ = os.MkdirAll(sessDir, 0o755)
	_ = os.WriteFile(filepath.Join(sessDir, "session.json"), []byte(`{"id":"q_sess"}`), 0o644)
	gapEvents := `{"seq":1,"t":"msg"}` + "\n" + `{"seq":5,"t":"msg"}` + "\n"
	_ = os.WriteFile(filepath.Join(sessDir, "events.jsonl"), []byte(gapEvents), 0o644)

	bundle := filepath.Join(t.TempDir(), "quarantine.lararium-backup")
	if err := CreateOffline(root, cfg, bundle, false, nil); err != nil {
		t.Fatal(err)
	}

	// Verify reports quarantine finding
	var stdout, stderr bytes.Buffer
	findings, err := Verify(bundle, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	hasQuarantine := false
	for _, f := range findings {
		if f.Class == "quarantine" {
			hasQuarantine = true
		}
		if f.Class == "tamper" {
			t.Fatalf("unexpected tamper finding: %+v", f)
		}
	}
	if !hasQuarantine {
		t.Fatal("expected quarantine finding on seq gap")
	}

	// RestoreTo on quarantine bundle must warn on stderr and proceed
	dest := filepath.Join(t.TempDir(), "restored-quarantine")
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	err = RestoreTo(bundle, dest)
	w.Close()
	os.Stderr = oldStderr

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	r.Close()
	stderrStr := buf.String()

	if err != nil {
		t.Fatalf("RestoreTo failed on quarantine bundle: %v", err)
	}
	if !strings.Contains(stderrStr, "quarantine") {
		t.Fatalf("expected quarantine warning in stderr, got: %s", stderrStr)
	}
}

// TestRestore_FreeSpaceShortRefusal tests preflight free space check before any lock (BK12, §4.5.4 rule 3).
func TestRestore_FreeSpaceShortRefusal(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)
	bundle := filepath.Join(t.TempDir(), "space.lararium-backup")
	if err := CreateOffline(root, cfg, bundle, false, nil); err != nil {
		t.Fatal(err)
	}

	// Mock StatfsFunc to report only 10 bytes available
	defer func() { StatfsFunc = defaultStatfs }()
	StatfsFunc = func(path string) (FSInfo, error) {
		return FSInfo{AvailableBytes: 10, Fsid: 100}, nil
	}

	target := setupTestRoot(t)
	_ = os.Remove(filepath.Join(target, "hearthd.sock"))
	err := RestoreReplace(bundle, target, true, false)
	if err == nil || !strings.Contains(err.Error(), "insufficient free space") {
		t.Fatalf("expected insufficient free space refusal, got: %v", err)
	}
	if !strings.Contains(err.Error(), "required") || !strings.Contains(err.Error(), "available 10") {
		t.Fatalf("expected numbers printed in refusal error, got: %v", err)
	}
}

func rewriteManifest(t *testing.T, srcBundle string, modify func(m *Manifest, raw map[string]any)) string {
	t.Helper()
	zr, err := zip.OpenReader(srcBundle)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	destBundle := filepath.Join(t.TempDir(), "rewritten.lararium-backup")
	out, err := os.OpenFile(destBundle, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	zw := zip.NewWriter(out)

	// Read original manifest
	mf, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	mData, _ := io.ReadAll(mf)
	mf.Close()

	var m Manifest
	_ = json.Unmarshal(mData, &m)
	var raw map[string]any
	_ = json.Unmarshal(mData, &raw)

	modify(&m, raw)

	newMData, _ := json.MarshalIndent(raw, "", "  ") //nolint:errchkjson // test fixture

	// Write manifest as first entry
	mw, err := zw.CreateHeader(&zip.FileHeader{Name: "manifest.json", Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = mw.Write(newMData)

	// Copy remaining entries
	for i := 1; i < len(zr.File); i++ {
		zf := zr.File[i]
		w, err := zw.CreateHeader(&zf.FileHeader)
		if err != nil {
			t.Fatal(err)
		}
		if !zf.FileInfo().IsDir() {
			rc, err := zf.Open()
			if err != nil {
				t.Fatal(err)
			}
			//nolint:gosec // G110: test re-copies its own small crafted bundle members
			_, _ = io.Copy(w, rc)
			rc.Close()
		}
	}

	_ = zw.Close()
	return destBundle
}
