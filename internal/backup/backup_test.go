package backup

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/text/unicode/norm"
)

func setupTestRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "hearth")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	// Persona files
	_ = os.WriteFile(filepath.Join(root, "SOUL.md"), []byte("# Soul\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "IDENTITY.md"), []byte("# Identity\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "USER.md"), []byte("# User\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "MEMORY.md"), []byte("# Memory\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "HEARTBEAT.md"), []byte("# Orders\n"), 0o644)

	// Runtime files
	_ = os.WriteFile(filepath.Join(root, "keys.json"), []byte(`{"openai":"sk-test"}`), 0o600)
	_ = os.WriteFile(filepath.Join(root, "tokens.json"), []byte(`{"t1":"hash"}`), 0o600)
	_ = os.WriteFile(filepath.Join(root, "custom.token"), []byte("tok"), 0o600)

	// Custos files & snapshots
	custosDir := filepath.Join(root, "custos")
	_ = os.MkdirAll(filepath.Join(custosDir, "snapshots", "snap1"), 0o755)
	_ = os.WriteFile(filepath.Join(custosDir, "vault.key"), []byte("key"), 0o600)
	_ = os.WriteFile(filepath.Join(custosDir, "surrogates.age"), []byte("age"), 0o600)
	_ = os.WriteFile(filepath.Join(custosDir, "fingerprints.json"), []byte("{}"), 0o600)
	_ = os.WriteFile(filepath.Join(custosDir, "snapshots", "snap1", "vault.db"), []byte("db"), 0o600)

	// Memory tree
	_ = os.MkdirAll(filepath.Join(root, "memory", "projects"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "memory", "MEMORY.md"), []byte("mem"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "memory", "projects", "notes.md"), []byte("notes"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "memory", "nested.txt"), []byte("not markdown"), 0o644)
	// Excluded index.db
	_ = os.WriteFile(filepath.Join(root, "memory", "index.db"), []byte("sqlite"), 0o644)

	// Sessions tree: 3 sessions
	for _, s := range []string{"s_1", "s_2", "main"} {
		sDir := filepath.Join(root, "sessions", s)
		_ = os.MkdirAll(sDir, 0o755)
		_ = os.WriteFile(filepath.Join(sDir, "session.json"), []byte(`{"id":"`+s+`"}`), 0o644)
		ev1 := `{"seq":1,"t":"msg","ts":"2026-10-09T18:00:00Z"}` + "\n"
		ev2 := `{"seq":2,"t":"msg","ts":"2026-10-09T18:01:00Z"}` + "\n"
		_ = os.WriteFile(filepath.Join(sDir, "events.jsonl"), []byte(ev1+ev2), 0o644)
	}

	// Archive with empty directory (BK13)
	_ = os.MkdirAll(filepath.Join(root, "archive", "empty-dir"), 0o755)

	// Lock files and sock files (exclusions)
	_ = os.WriteFile(filepath.Join(root, "backup.lock"), []byte(""), 0o600)
	_ = os.WriteFile(filepath.Join(root, "custos.lock"), []byte(""), 0o600)
	_ = os.WriteFile(filepath.Join(root, "keys.lock"), []byte(""), 0o600)
	_ = os.WriteFile(filepath.Join(root, "hearthd.sock"), []byte(""), 0o600)

	return root
}

func setupTestConfig(t *testing.T) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "lararium.yaml")
	_ = os.WriteFile(cfg, []byte("hearth:\n  home: /tmp\n"), 0o600)
	return cfg
}

func TestBK15_Counts(t *testing.T) {
	root := setupTestRoot(t)
	counts, err := ComputeCounts(root)
	if err != nil {
		t.Fatalf("ComputeCounts: %v", err)
	}
	if counts.Sessions != 3 {
		t.Errorf("counts.Sessions = %d, want 3", counts.Sessions)
	}
	if counts.MemoryFiles != 2 {
		t.Errorf("counts.MemoryFiles = %d, want 2 (MEMORY.md + projects/notes.md)", counts.MemoryFiles)
	}
}

func TestBK2_InclusionsExclusionsZipEntry0(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)
	out := filepath.Join(t.TempDir(), "test.lararium-backup")

	if err := CreateOffline(root, cfg, out, false, nil); err != nil {
		t.Fatalf("CreateOffline: %v", err)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()

	if len(zr.File) == 0 {
		t.Fatal("zip is empty")
	}
	if zr.File[0].Name != "manifest.json" {
		t.Fatalf("zip entry 0 is %s, want manifest.json", zr.File[0].Name)
	}

	names := make(map[string]bool)
	for _, f := range zr.File {
		names[f.Name] = true
	}

	// Exclusions absent
	for _, bad := range []string{"tree/memory/index.db", "tree/backup.lock", "tree/custos.lock", "tree/keys.lock", "tree/hearthd.sock"} {
		if names[bad] {
			t.Errorf("excluded file %s found in zip", bad)
		}
	}

	// Inclusions present
	for _, good := range []string{"config/lararium.yaml", "tree/SOUL.md", "tree/custos/snapshots/snap1/vault.db", "tree/archive/empty-dir/"} {
		if !names[good] {
			t.Errorf("expected member %s missing from zip", good)
		}
	}

	// Verify secrets display on stderr
	var stdout, stderr bytes.Buffer
	findings, err := Verify(out, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings, got %d: %+v", len(findings), findings)
	}

	errOutput := stderr.String()
	for _, secret := range []string{"tree/keys.json", "tree/tokens.json", "tree/custom.token", "tree/custos/vault.key", "tree/custos/surrogates.age", "tree/custos/fingerprints.json", "tree/custos/snapshots/snap1/vault.db", "config/lararium.yaml"} {
		if !strings.Contains(errOutput, secret) {
			t.Errorf("secret %s not listed in verify stderr display", secret)
		}
	}
	if strings.Contains(errOutput, "tree/SOUL.md") || strings.Contains(errOutput, "tree/memory/MEMORY.md") {
		t.Errorf("non-secret file found in verify secrets display: %s", errOutput)
	}
	if !strings.Contains(errOutput, SecretsHonestyLine) {
		t.Errorf("secrets honesty line missing from stderr: %s", errOutput)
	}

	// Test --no-config drops config
	outNoCfg := filepath.Join(t.TempDir(), "no-config.lararium-backup")
	if err := CreateOffline(root, cfg, outNoCfg, true, nil); err != nil {
		t.Fatalf("CreateOffline --no-config: %v", err)
	}
	zr2, _ := zip.OpenReader(outNoCfg)
	defer zr2.Close()
	for _, f := range zr2.File {
		if f.Name == "config/lararium.yaml" {
			t.Fatal("config/lararium.yaml found in --no-config bundle")
		}
	}
}

func TestBK3_TamperVsQuarantine(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)
	out := filepath.Join(t.TempDir(), "base.lararium-backup")
	if err := CreateOffline(root, cfg, out, false, nil); err != nil {
		t.Fatalf("CreateOffline: %v", err)
	}

	// Case 1: Byte flip in member -> tamper
	{
		corruptPath := filepath.Join(t.TempDir(), "corrupt-member.lararium-backup")
		copyAndMutateZip(t, out, corruptPath, func(name string, data []byte) []byte {
			if name == "tree/SOUL.md" {
				data[0] ^= 0xff // flip byte
			}
			return data
		})
		var stdout, stderr bytes.Buffer
		findings, err := Verify(corruptPath, &stdout, &stderr)
		if err != nil {
			t.Fatalf("Verify err: %v", err)
		}
		if len(findings) == 0 {
			t.Fatal("expected findings for flipped member byte")
		}
		found := false
		for _, f := range findings {
			if f.Class == "tamper" && f.Path == "tree/SOUL.md" {
				found = true
			}
			if f.Class == "quarantine" {
				t.Fatalf("content damage resulted in quarantine: %+v", f)
			}
		}
		if !found {
			t.Fatalf("expected tamper finding for tree/SOUL.md, got %+v", findings)
		}
	}

	// Case 2: Byte flip in manifest -> tamper
	{
		corruptPath := filepath.Join(t.TempDir(), "corrupt-manifest.lararium-backup")
		copyAndMutateZip(t, out, corruptPath, func(name string, data []byte) []byte {
			if name == "manifest.json" {
				// Corrupt hash of SOUL.md
				data = bytes.Replace(data, []byte("tree/SOUL.md"), []byte("tree/SOUL.md"), 1)
				// Corrupt first hash character
				data = bytes.Replace(data, []byte(`"sha256": "`), []byte(`"sha256": "0000000000`), 1)
			}
			return data
		})
		var stdout, stderr bytes.Buffer
		findings, _ := Verify(corruptPath, &stdout, &stderr)
		if len(findings) == 0 {
			t.Fatal("expected findings for corrupted manifest")
		}
		for _, f := range findings {
			if f.Class != "tamper" {
				t.Fatalf("manifest mismatch yielded non-tamper: %+v", f)
			}
		}
	}

	// Case 3: Extra zip entry -> tamper
	{
		corruptPath := filepath.Join(t.TempDir(), "extra-entry.lararium-backup")
		addZipEntry(t, out, corruptPath, "tree/extra.txt", []byte("unlisted"))
		var stdout, stderr bytes.Buffer
		findings, _ := Verify(corruptPath, &stdout, &stderr)
		found := false
		for _, f := range findings {
			if f.Class == "tamper" && f.Path == "tree/extra.txt" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected tamper for extra entry, got %+v", findings)
		}
	}

	// Case 4: Missing manifest entry (manifest lists entry not in zip) -> tamper
	{
		corruptPath := filepath.Join(t.TempDir(), "missing-zip.lararium-backup")
		removeZipEntry(t, out, corruptPath, "tree/SOUL.md")
		var stdout, stderr bytes.Buffer
		findings, _ := Verify(corruptPath, &stdout, &stderr)
		found := false
		for _, f := range findings {
			if f.Class == "tamper" && f.Path == "tree/SOUL.md" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected tamper for missing member, got %+v", findings)
		}
	}

	// Case 5: Seq gap in events.jsonl (with correct manifest hash) -> quarantine
	{
		rootGap := setupTestRoot(t)
		gapFile := filepath.Join(rootGap, "sessions", "main", "events.jsonl")
		// seq 1 followed by seq 3 (gap!)
		_ = os.WriteFile(gapFile, []byte(`{"seq":1,"t":"msg"}`+"\n"+`{"seq":3,"t":"msg"}`+"\n"), 0o644)
		outGap := filepath.Join(t.TempDir(), "gap.lararium-backup")
		if err := CreateOffline(rootGap, cfg, outGap, false, nil); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		findings, err := Verify(outGap, &stdout, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, f := range findings {
			if f.Class == "quarantine" && strings.Contains(f.Path, "events.jsonl") {
				found = true
			}
			if f.Class == "tamper" {
				t.Fatalf("pre-existing logical damage classified as tamper: %+v", f)
			}
		}
		if !found {
			t.Fatalf("expected quarantine finding for seq gap, got %+v", findings)
		}
	}

	// Case 6: Seq duplicate in events.jsonl (with correct manifest hash) -> quarantine
	{
		rootDup := setupTestRoot(t)
		dupFile := filepath.Join(rootDup, "sessions", "main", "events.jsonl")
		// seq 1 followed by seq 1 (duplicate!)
		_ = os.WriteFile(dupFile, []byte(`{"seq":1,"t":"msg"}`+"\n"+`{"seq":1,"t":"msg"}`+"\n"), 0o644)
		outDup := filepath.Join(t.TempDir(), "dup.lararium-backup")
		if err := CreateOffline(rootDup, cfg, outDup, false, nil); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		findings, err := Verify(outDup, &stdout, &stderr)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, f := range findings {
			if f.Class == "quarantine" && strings.Contains(f.Path, "events.jsonl") {
				found = true
			}
			if f.Class == "tamper" {
				t.Fatalf("logical duplicate classified as tamper: %+v", f)
			}
		}
		if !found {
			t.Fatalf("expected quarantine finding for seq duplicate, got %+v", findings)
		}
	}
}

func TestBK6_CreateRefusals(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)

	// Refusal 1: --out in root
	outInRoot := filepath.Join(root, "hearth.lararium-backup")
	if err := CreateOffline(root, cfg, outInRoot, false, nil); err == nil {
		t.Fatal("expected refusal for --out inside root")
	}

	// Refusal 2: symlink in tree
	symlinkPath := filepath.Join(root, "bad-link")
	_ = os.Symlink(filepath.Join(root, "SOUL.md"), symlinkPath)
	outValid := filepath.Join(t.TempDir(), "test.lararium-backup")
	err := CreateOffline(root, cfg, outValid, false, nil)
	if err == nil || !strings.Contains(err.Error(), "bad-link") {
		t.Fatalf("expected tree-shape refusal listing offender 'bad-link', got %v", err)
	}
	_ = os.Remove(symlinkPath)

	// Refusal 3: named pipe (FIFO)
	fifoPath := filepath.Join(root, "bad-pipe")
	if err := syscall.Mkfifo(fifoPath, 0o600); err == nil {
		err = CreateOffline(root, cfg, outValid, false, nil)
		if err == nil || !strings.Contains(err.Error(), "bad-pipe") {
			t.Fatalf("expected tree-shape refusal listing offender 'bad-pipe', got %v", err)
		}
		_ = os.Remove(fifoPath)
	}
}

func TestBK7_VersionLaw(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)
	out := filepath.Join(t.TempDir(), "base.lararium-backup")
	if err := CreateOffline(root, cfg, out, false, nil); err != nil {
		t.Fatal(err)
	}

	// 1. backup/2.0 -> refuse with migration pointer
	{
		v2Path := filepath.Join(t.TempDir(), "v2.lararium-backup")
		copyAndMutateZip(t, out, v2Path, func(name string, data []byte) []byte {
			if name == "manifest.json" {
				data = bytes.Replace(data, []byte(`"format": "backup/1.0"`), []byte(`"format": "backup/2.0"`), 1)
			}
			return data
		})
		var stdout, stderr bytes.Buffer
		_, err := Verify(v2Path, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "migration") {
			t.Fatalf("expected version 2.0 refusal with migration pointer, got %v", err)
		}
	}

	// 2. backup/1.9 with unknown fields -> proceeds + reports drops
	{
		v19Path := filepath.Join(t.TempDir(), "v19.lararium-backup")
		copyAndMutateZip(t, out, v19Path, func(name string, data []byte) []byte {
			if name == "manifest.json" {
				var m map[string]any
				_ = json.Unmarshal(data, &m)
				m["format"] = "backup/1.9"
				m["unknown_future_field"] = "future_value"
				data, _ = json.MarshalIndent(m, "", "  ")
			}
			return data
		})
		var stdout, stderr bytes.Buffer
		findings, err := Verify(v19Path, &stdout, &stderr)
		if err != nil {
			t.Fatalf("Verify 1.9 failed: %v", err)
		}
		if len(findings) != 0 {
			t.Fatalf("expected 0 findings for valid 1.9, got %+v", findings)
		}
		if !strings.Contains(stderr.String(), "unknown_future_field") {
			t.Fatalf("expected report drop on stderr, got: %s", stderr.String())
		}
	}

	// 3. Newer product -> warn on stderr, proceed
	{
		newProdPath := filepath.Join(t.TempDir(), "newprod.lararium-backup")
		copyAndMutateZip(t, out, newProdPath, func(name string, data []byte) []byte {
			if name == "manifest.json" {
				var m map[string]any
				_ = json.Unmarshal(data, &m)
				m["product"] = "hearthd/9.9.9"
				data, _ = json.MarshalIndent(m, "", "  ")
			}
			return data
		})
		var stdout, stderr bytes.Buffer
		findings, err := Verify(newProdPath, &stdout, &stderr)
		if err != nil {
			t.Fatalf("Verify newer product failed: %v", err)
		}
		if len(findings) != 0 {
			t.Fatalf("expected 0 findings, got %+v", findings)
		}
		if !strings.Contains(stderr.String(), "hearthd/9.9.9") || !strings.Contains(stderr.String(), "is newer") {
			t.Fatalf("expected product warning on stderr, got: %s", stderr.String())
		}
	}
}

func TestBK9_Concurrency(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)
	out := filepath.Join(t.TempDir(), "test.lararium-backup")

	// Acquire backup.lock explicitly
	locks, err := AcquireLocks(root)
	if err != nil {
		t.Fatalf("acquire locks: %v", err)
	}

	// Second create must refuse IMMEDIATELY (LOCK_NB)
	start := testing.Benchmark(func(b *testing.B) {})
	_ = start
	err2 := CreateOffline(root, cfg, out, false, nil)
	if err2 == nil || !errors.Is(err2, ErrBusy) {
		t.Fatalf("second create must refuse immediately with ErrBusy, got: %v", err2)
	}

	locks.Release()
}

func TestBK10_ListTSV(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)
	out := filepath.Join(t.TempDir(), "test.lararium-backup")
	if err := CreateOffline(root, cfg, out, false, nil); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := List(out, &buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 {
		t.Fatal("empty list output")
	}
	if lines[0] != "#columns: path size sha256 mode" {
		t.Fatalf("header = %q, want '#columns: path size sha256 mode'", lines[0])
	}

	foundDir := false
	foundFile := false
	for _, l := range lines[1:] {
		cols := strings.Split(l, "\t")
		if len(cols) != 4 {
			t.Fatalf("expected 4 TSV columns, got %d: %q", len(cols), l)
		}
		path, size, sha, mode := cols[0], cols[1], cols[2], cols[3]
		_ = mode
		if strings.HasSuffix(path, "/") {
			foundDir = true
			if size != "-" || sha != "-" {
				t.Fatalf("dir entry must have '-' for size and sha256, got size=%s, sha=%s", size, sha)
			}
		} else {
			foundFile = true
			if size == "-" || sha == "-" {
				t.Fatalf("file entry must not have '-' for size and sha256, got size=%s, sha=%s", size, sha)
			}
		}
	}
	if !foundDir || !foundFile {
		t.Fatalf("list output missing dirs or files: foundDir=%v, foundFile=%v", foundDir, foundFile)
	}
}

func TestBK12_FreeSpacePreflight(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)
	out := filepath.Join(t.TempDir(), "test.lararium-backup")

	origStatfs := StatfsFunc
	defer func() { StatfsFunc = origStatfs }()

	// Case 1: Out fs artificially full
	StatfsFunc = func(path string) (FSInfo, error) {
		if strings.Contains(path, "hearth") {
			return FSInfo{AvailableBytes: 1000000000, Fsid: 1}, nil
		}
		// out fs has only 10 bytes free
		return FSInfo{AvailableBytes: 10, Fsid: 2}, nil
	}

	err := CreateOffline(root, cfg, out, false, nil)
	if err == nil || !strings.Contains(err.Error(), "insufficient free space") {
		t.Fatalf("expected preflight free space refusal, got %v", err)
	}
	// Verify no partial file was created
	if _, err := os.Stat(out); err == nil {
		t.Fatal("partial backup was created despite space refusal")
	}

	// Case 2: Same filesystem, space < 2Σ
	treeSz, _ := ComputeTreeSize(root, cfg, true)
	StatfsFunc = func(path string) (FSInfo, error) {
		// Available is 1.5 * treeSz (< 2Σ)
		return FSInfo{AvailableBytes: treeSz + 1, Fsid: 100}, nil
	}
	err = CreateOffline(root, cfg, out, false, nil)
	if err == nil || !strings.Contains(err.Error(), "2Σ") {
		t.Fatalf("expected 2Σ refusal on shared fs, got %v", err)
	}
}

func TestBK13_ModesAndEmptyDirs(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)

	// Create source 04755 file
	suidFile := filepath.Join(root, "suid-tool")
	_ = os.WriteFile(suidFile, []byte("echo hi\n"), 0o755)
	_ = syscall.Chmod(suidFile, 0o4755)

	out := filepath.Join(t.TempDir(), "modes.lararium-backup")
	if err := CreateOffline(root, cfg, out, false, nil); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("bundle mode = %o, want 0600", fi.Mode().Perm())
	}

	var stdout, stderr bytes.Buffer
	findings, err := Verify(out, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings, got %+v", findings)
	}
}

func TestBK14_NFC(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)

	// Create a path in NFD form
	nfdName := norm.NFD.String("café.md")
	_ = os.WriteFile(filepath.Join(root, "memory", nfdName), []byte("coffee"), 0o644)

	out := filepath.Join(t.TempDir(), "nfc.lararium-backup")
	if err := CreateOffline(root, cfg, out, false, nil); err != nil {
		t.Fatal(err)
	}

	zr, _ := zip.OpenReader(out)
	defer zr.Close()

	foundNFC := false
	for _, f := range zr.File {
		if strings.Contains(f.Name, "caf") {
			if norm.NFC.String(f.Name) == f.Name {
				foundNFC = true
			} else {
				t.Fatalf("path %s was not stored NFC normalized", f.Name)
			}
		}
	}
	if !foundNFC {
		t.Fatal("NFC path not found in zip archive")
	}

	var stdout, stderr bytes.Buffer
	findings, err := Verify(out, &stdout, &stderr)
	if err != nil || len(findings) != 0 {
		t.Fatalf("verify on NFC bundle failed: err=%v, findings=%+v", err, findings)
	}
}

func TestBK16_ExtractConfig(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)
	out := filepath.Join(t.TempDir(), "test.lararium-backup")
	if err := CreateOffline(root, cfg, out, false, nil); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "extracted.yaml")
	if err := ExtractConfig(out, dest); err != nil {
		t.Fatalf("ExtractConfig: %v", err)
	}

	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("extracted config mode = %o, want 0600", fi.Mode().Perm())
	}

	// Refuse existing path
	if err := ExtractConfig(out, dest); err == nil {
		t.Fatal("expected refusal on existing dest path")
	}

	// Error on --no-config bundle
	outNoCfg := filepath.Join(t.TempDir(), "no-config.lararium-backup")
	if err := CreateOffline(root, cfg, outNoCfg, true, nil); err != nil {
		t.Fatal(err)
	}
	dest2 := filepath.Join(t.TempDir(), "extracted2.yaml")
	err = ExtractConfig(outNoCfg, dest2)
	if err == nil || !strings.Contains(err.Error(), "--no-config") {
		t.Fatalf("expected clear error on --no-config bundle, got: %v", err)
	}
}

func TestBK18_ZipSlip(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)
	out := filepath.Join(t.TempDir(), "base.lararium-backup")
	if err := CreateOffline(root, cfg, out, false, nil); err != nil {
		t.Fatal(err)
	}

	slipPaths := []string{
		"../escape.txt",
		"/abs.txt",
		"tree/../../escape2.txt",
		"C:drive.txt",
	}

	for _, slip := range slipPaths {
		slipZip := filepath.Join(t.TempDir(), "slip.lararium-backup")
		addZipEntry(t, out, slipZip, slip, []byte("danger"))

		var stdout, stderr bytes.Buffer
		findings, _ := Verify(slipZip, &stdout, &stderr)
		if len(findings) == 0 {
			t.Fatalf("expected verify refusal for zip slip path %q", slip)
		}
		found := false
		for _, f := range findings {
			if f.Class == "tamper" && f.Path == slip {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected tamper finding for %q, got %+v", slip, findings)
		}
	}
}

// Helpers for corrupting zip bundles
func copyAndMutateZip(t *testing.T, src, dst string, mutate func(name string, data []byte) []byte) {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	outF, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer outF.Close()

	zw := zip.NewWriter(outF)
	defer zw.Close()

	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}

		newData := mutate(f.Name, data)
		fh := &zip.FileHeader{
			Name:   f.Name,
			Method: f.Method,
		}
		fh.SetMode(f.Mode())
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(newData); err != nil {
			t.Fatal(err)
		}
	}
}

func addZipEntry(t *testing.T, src, dst string, name string, data []byte) {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	outF, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer outF.Close()

	zw := zip.NewWriter(outF)
	defer zw.Close()

	for _, f := range zr.File {
		rc, _ := f.Open()
		content, _ := io.ReadAll(rc)
		rc.Close()
		fh := &zip.FileHeader{Name: f.Name, Method: f.Method}
		fh.SetMode(f.Mode())
		w, _ := zw.CreateHeader(fh)
		_, _ = w.Write(content)
	}

	fh := &zip.FileHeader{Name: name, Method: zip.Deflate}
	fh.SetMode(0o644)
	w, _ := zw.CreateHeader(fh)
	_, _ = w.Write(data)
}

func removeZipEntry(t *testing.T, src, dst string, nameToRemove string) {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	outF, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer outF.Close()

	zw := zip.NewWriter(outF)
	defer zw.Close()

	for _, f := range zr.File {
		if f.Name == nameToRemove {
			continue
		}
		rc, _ := f.Open()
		content, _ := io.ReadAll(rc)
		rc.Close()
		fh := &zip.FileHeader{Name: f.Name, Method: f.Method}
		fh.SetMode(f.Mode())
		w, _ := zw.CreateHeader(fh)
		_, _ = w.Write(content)
	}
}
