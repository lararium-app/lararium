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

	// Case 7: Unparseable mode string in manifest entry -> tamper (B6)
	{
		corruptPath := filepath.Join(t.TempDir(), "invalid-mode.lararium-backup")
		copyAndMutateZip(t, out, corruptPath, func(name string, data []byte) []byte {
			if name == "manifest.json" {
				data = bytes.Replace(data, []byte(`"mode": "0600"`), []byte(`"mode": "invalid"`), 1)
				if !bytes.Contains(data, []byte(`"mode": "invalid"`)) {
					data = bytes.Replace(data, []byte(`"mode": "0644"`), []byte(`"mode": "invalid"`), 1)
				}
			}
			return data
		})
		var stdout, stderr bytes.Buffer
		findings, _ := Verify(corruptPath, &stdout, &stderr)
		found := false
		for _, f := range findings {
			if f.Class == "tamper" && strings.Contains(f.Detail, "unparseable mode") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected tamper finding for unparseable mode string, got %+v", findings)
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
	// Refusal 1b: --out equal to root itself (F2)
	if err := CreateOffline(root, cfg, root, false, nil); err == nil {
		t.Fatal("expected refusal for --out == root itself")
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
	outDir := filepath.Dir(out)

	origStatfs := StatfsFunc
	defer func() { StatfsFunc = origStatfs }()

	// Filesystem-id-aware statfs mock (map path -> FSInfo, resolving via longest matching prefix)
	fsMap := make(map[string]FSInfo)
	StatfsFunc = func(path string) (FSInfo, error) {
		clean := filepath.Clean(path)
		var bestPrefix string
		var bestInfo FSInfo
		var found bool
		for prefix, info := range fsMap {
			cPrefix := filepath.Clean(prefix)
			if clean == cPrefix || strings.HasPrefix(clean, cPrefix+string(filepath.Separator)) {
				if len(cPrefix) >= len(bestPrefix) {
					bestPrefix = cPrefix
					bestInfo = info
					found = true
				}
			}
		}
		if found {
			return bestInfo, nil
		}
		return FSInfo{AvailableBytes: 1000000000, Fsid: 1}, nil
	}

	// Case 1: Out fs artificially full (different fsid, space < Σ)
	fsMap[root] = FSInfo{AvailableBytes: 1000000000, Fsid: 1}
	fsMap[outDir] = FSInfo{AvailableBytes: 10, Fsid: 2}

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
	fsMap[root] = FSInfo{AvailableBytes: treeSz + 1, Fsid: 100}
	fsMap[outDir] = FSInfo{AvailableBytes: treeSz + 1, Fsid: 100}

	err = CreateOffline(root, cfg, out, false, nil)
	if err == nil || !strings.Contains(err.Error(), "2Σ") {
		t.Fatalf("expected 2Σ refusal on shared fs, got %v", err)
	}

	// B4: assert staging fs == root fs (compare StatfsFunc results)
	stagingDir, err := CreateStagingDir(root)
	if err != nil {
		t.Fatalf("CreateStagingDir: %v", err)
	}
	defer os.RemoveAll(stagingDir)
	stFS, err := StatfsFunc(stagingDir)
	if err != nil {
		t.Fatalf("StatfsFunc(stagingDir): %v", err)
	}
	rFS, err := StatfsFunc(root)
	if err != nil {
		t.Fatalf("StatfsFunc(root): %v", err)
	}
	if stFS.Fsid != rFS.Fsid {
		t.Fatalf("staging fsid %d != root fsid %d (B4)", stFS.Fsid, rFS.Fsid)
	}
	os.RemoveAll(stagingDir)

	// Case 3 (F3): filepath.Dir(root) reports a DIFFERENT fsid than root
	// Assert staging dir created INSIDE root, excluded from walk (existing .backup-staging- clause),
	// and same-fsid out still demands 2Σ.
	parentDir := filepath.Dir(root)
	fsMap = make(map[string]FSInfo)
	fsMap[parentDir] = FSInfo{AvailableBytes: 1000000000, Fsid: 200} // parent has DIFFERENT fsid
	fsMap[root] = FSInfo{AvailableBytes: treeSz + 1, Fsid: 100}       // root has fsid 100
	fsMap[outDir] = FSInfo{AvailableBytes: treeSz + 1, Fsid: 100}     // outDir shares fsid 100 with root

	stagingDirInside, err := CreateStagingDir(root)
	if err != nil {
		t.Fatalf("CreateStagingDir with different parent fsid: %v", err)
	}
	defer os.RemoveAll(stagingDirInside)

	// Assert staging dir created INSIDE root
	if filepath.Dir(stagingDirInside) != root {
		t.Fatalf("expected staging dir created inside root %s, got %s", root, stagingDirInside)
	}

	// Assert staging dir is excluded from walk (existing .backup-staging- clause)
	dummyFile := filepath.Join(stagingDirInside, "staging-dummy.txt")
	if err := os.WriteFile(dummyFile, []byte("staged content"), 0o600); err != nil {
		t.Fatal(err)
	}

	relStaging, err := filepath.Rel(root, stagingDirInside)
	if err != nil {
		t.Fatal(err)
	}
	stagingFi, err := os.Stat(stagingDirInside)
	if err != nil {
		t.Fatal(err)
	}
	if !IsExcluded(relStaging, stagingFi) {
		t.Fatalf("expected staging dir %s to be excluded by IsExcluded", relStaging)
	}

	foundStagingInWalk := false
	err = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		r, _ := filepath.Rel(root, p)
		if IsExcluded(r, fi) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.Contains(r, ".backup-staging-") {
			foundStagingInWalk = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk failed: %v", err)
	}
	if foundStagingInWalk {
		t.Fatal("staging dir created inside root was not excluded from walk")
	}

	// ComputeTreeSize must not include files inside staging dir
	treeSzWithStaging, err := ComputeTreeSize(root, cfg, true)
	if err != nil {
		t.Fatalf("ComputeTreeSize: %v", err)
	}
	if treeSzWithStaging != treeSz {
		t.Fatalf("tree size changed when staging dir inside root: got %d, want %d", treeSzWithStaging, treeSz)
	}

	// Same-fsid out still demands 2Σ (available treeSz + 1 < 2Σ)
	err = PreflightFreeSpace(root, out, treeSz)
	if err == nil || !strings.Contains(err.Error(), "2Σ") {
		t.Fatalf("expected 2Σ refusal on shared fsid, got %v", err)
	}
	err = CreateOffline(root, cfg, out, false, nil)
	if err == nil || !strings.Contains(err.Error(), "2Σ") {
		t.Fatalf("expected CreateOffline 2Σ refusal on shared fsid, got %v", err)
	}

	// When space is >= 2Σ, same-fsid preflight succeeds
	fsMap[root] = FSInfo{AvailableBytes: 2 * treeSz, Fsid: 100}
	fsMap[outDir] = FSInfo{AvailableBytes: 2 * treeSz, Fsid: 100}
	if err := PreflightFreeSpace(root, out, treeSz); err != nil {
		t.Fatalf("expected PreflightFreeSpace to succeed with 2Σ: %v", err)
	}
}

func TestBK13_ModesAndEmptyDirs(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)

	// Create source 04755 file (B5)
	suidFile := filepath.Join(root, "suid-tool")
	_ = os.WriteFile(suidFile, []byte("echo hi\n"), 0o755)
	_ = syscall.Chmod(suidFile, 0o4755)

	// Create empty dir in source root (B5)
	emptyDir := filepath.Join(root, "empty-dir")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}

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

	// B5 pin: assert manifest entries
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	if len(zr.File) == 0 || zr.File[0].Name != "manifest.json" {
		t.Fatal("manifest.json must be strictly first zip entry")
	}
	mf, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	mfData, err := io.ReadAll(mf)
	mf.Close()
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, _, err := ParseManifest(mfData)
	if err != nil {
		t.Fatal(err)
	}

	foundSuid := false
	foundEmptyDirManifest := false
	for _, entry := range manifest.Entries {
		if entry.Path == "tree/suid-tool" {
			foundSuid = true
			if entry.Mode != "4755" && entry.Mode != "04755" {
				t.Fatalf("suid source recorded mode = %q, want 4755 or 04755", entry.Mode)
			}
			parsedMode, err := ParseOctalMode(entry.Mode)
			if err != nil {
				t.Fatalf("parse suid mode: %v", err)
			}
			extMode := ExtractionMode(parsedMode)
			if extMode != 0o755 {
				t.Fatalf("extraction mode = %04o, want 0755", extMode)
			}
		}
		if entry.Path == "tree/empty-dir/" {
			foundEmptyDirManifest = true
			if !entry.Dir {
				t.Fatalf("empty dir entry must have dir: true")
			}
			if entry.SHA256 != "" || entry.Size != nil {
				t.Fatalf("empty dir entry must carry no sha256 or size, got sha=%q size=%v", entry.SHA256, entry.Size)
			}
		}
	}
	if !foundSuid {
		t.Fatal("tree/suid-tool not found in manifest")
	}
	if !foundEmptyDirManifest {
		t.Fatal("tree/empty-dir/ not found in manifest")
	}

	foundEmptyDirZip := false
	for _, zf := range zr.File {
		if zf.Name == "tree/empty-dir/" {
			foundEmptyDirZip = true
			if !zf.FileInfo().IsDir() {
				t.Fatal("empty dir zip entry must be a directory")
			}
		}
	}
	if !foundEmptyDirZip {
		t.Fatal("tree/empty-dir/ not found in zip archive")
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

// TestBK_M11_ReadOnlyDirs tests that read-only directories (e.g. 0555) do not fail staging (M11).
func TestBK_M11_ReadOnlyDirs(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)

	roDir := filepath.Join(root, "readonly-dir")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	childFile := filepath.Join(roDir, "child.txt")
	if err := os.WriteFile(childFile, []byte("child"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make dir read-only
	if err := syscall.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer syscall.Chmod(roDir, 0o755) // cleanup permission

	out := filepath.Join(t.TempDir(), "ro-dirs.lararium-backup")
	if err := CreateOffline(root, cfg, out, false, nil); err != nil {
		t.Fatalf("CreateOffline failed on read-only dir: %v", err)
	}

	var stdout, stderr bytes.Buffer
	findings, err := Verify(out, &stdout, &stderr)
	if err != nil || len(findings) != 0 {
		t.Fatalf("verify failed on read-only dir bundle: err=%v, findings=%+v", err, findings)
	}
}

// TestBK_M12_StagingTreeSymlinkAborts tests that encountering a symlink during StageTree aborts (M12).
func TestBK_M12_StagingTreeSymlinkAborts(t *testing.T) {
	root := setupTestRoot(t)
	stagingDir := filepath.Join(t.TempDir(), "staging")
	_ = os.MkdirAll(stagingDir, 0o700)

	symlinkPath := filepath.Join(root, "mid-copy-link")
	_ = os.Symlink(filepath.Join(root, "SOUL.md"), symlinkPath)
	defer os.Remove(symlinkPath)

	err := StageTree(root, stagingDir)
	if err == nil || !strings.Contains(err.Error(), "tree-shape law violation") {
		t.Fatalf("expected tree-shape law violation during StageTree, got: %v", err)
	}
}

// TestBK_M13_ConfigReadErrors tests that missing or unreadable config when !noConfig refuses clearly (M13).
func TestBK_M13_ConfigReadErrors(t *testing.T) {
	root := setupTestRoot(t)
	missingCfg := filepath.Join(t.TempDir(), "nonexistent.yaml")
	out := filepath.Join(t.TempDir(), "test.lararium-backup")

	err := CreateOffline(root, missingCfg, out, false, nil)
	if err == nil || !strings.Contains(err.Error(), "config in force must be in bundle") {
		t.Fatalf("expected clear refusal for missing config file, got: %v", err)
	}
}

// TestBK_M15_MalformedMemberPath_EvilTxt tests that bundle schema at verify rejects root-level evil.txt (M15).
func TestBK_M15_MalformedMemberPath_EvilTxt(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)
	outValid := filepath.Join(t.TempDir(), "valid.lararium-backup")
	if err := CreateOffline(root, cfg, outValid, false, nil); err != nil {
		t.Fatal(err)
	}

	evilPath := filepath.Join(t.TempDir(), "evil.lararium-backup")
	// Add evil.txt at root of zip AND in manifest.json
	copyAndMutateZip(t, outValid, evilPath, func(name string, data []byte) []byte {
		if name == "manifest.json" {
			var m Manifest
			_ = json.Unmarshal(data, &m)
			sz := int64(4)
			m.Entries = append(m.Entries, ManifestEntry{
				Path:   "evil.txt",
				Mode:   "0644",
				Size:   &sz,
				SHA256: "abcd",
			})
			mutated, _ := json.MarshalIndent(m, "", "  ")
			return mutated
		}
		return data
	})
	// Also add evil.txt file to zip
	evilZipWithMember := filepath.Join(t.TempDir(), "evil-zip.lararium-backup")
	addZipEntry(t, evilPath, evilZipWithMember, "evil.txt", []byte("evil"))

	var stdout, stderr bytes.Buffer
	findings, _ := Verify(evilZipWithMember, &stdout, &stderr)
	found := false
	for _, f := range findings {
		if f.Class == "tamper" && (f.Path == "evil.txt" || strings.Contains(f.Detail, "malformed")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected tamper finding for root-level evil.txt (M15), got: %+v", findings)
	}
}

// TestBK_m17_IsSecretPathSnapshots tests that snapshots/** matches children only, not the directory itself (m17).
func TestBK_m17_IsSecretPathSnapshots(t *testing.T) {
	if IsSecretPath("tree/custos/snapshots/") {
		t.Fatal("tree/custos/snapshots/ directory entry itself must NOT be a secret (m17)")
	}
	if !IsSecretPath("tree/custos/snapshots/snap-1/manifest.json") {
		t.Fatal("child of tree/custos/snapshots/ MUST be a secret (m17)")
	}
}

// TestBK_m18_IsProductNewerUnparseable tests that isProductNewer returns false with no warning on parse failure (m18).
func TestBK_m18_IsProductNewerUnparseable(t *testing.T) {
	if isProductNewer("hearthd/not-a-version", Product) {
		t.Fatal("expected false for unparseable product version (m18)")
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

// TestBK_F1_PreciseExclusionLaw tests that:
// (a) bundle includes prompt.tmpl, notes.tmp.md, config.tmp.json
// (b) bundle excludes tokens-abc.tmp at root and nuntius/state.tmp4182 (fixture)
func TestBK_F1_PreciseExclusionLaw(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)
	out := filepath.Join(t.TempDir(), "test-f1.lararium-backup")

	// (a) files that MUST be included: prompt.tmpl, notes.tmp.md, config.tmp.json
	if err := os.WriteFile(filepath.Join(root, "prompt.tmpl"), []byte("prompt template"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.tmp.md"), []byte("notes temp md"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.tmp.json"), []byte("config temp json"), 0o644); err != nil {
		t.Fatal(err)
	}

	// (b) files that MUST be excluded: tokens-abc.tmp at root and nuntius/state.tmp4182
	if err := os.WriteFile(filepath.Join(root, "tokens-abc.tmp"), []byte("secret token tmp"), 0o600); err != nil {
		t.Fatal(err)
	}
	nuntiusDir := filepath.Join(root, "nuntius")
	if err := os.MkdirAll(nuntiusDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nuntiusDir, "state.tmp4182"), []byte("nuntius state tmp"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := CreateOffline(root, cfg, out, false, nil); err != nil {
		t.Fatalf("CreateOffline failed: %v", err)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()

	names := make(map[string]bool)
	for _, f := range zr.File {
		names[f.Name] = true
	}

	// (a) bundle includes prompt.tmpl, notes.tmp.md, config.tmp.json
	for _, included := range []string{"tree/prompt.tmpl", "tree/notes.tmp.md", "tree/config.tmp.json"} {
		if !names[included] {
			t.Errorf("expected bundle to include %s, but it was missing", included)
		}
	}

	// (b) bundle excludes tokens-abc.tmp at root and nuntius/state.tmp4182
	for _, excluded := range []string{"tree/tokens-abc.tmp", "tree/nuntius/state.tmp4182"} {
		if names[excluded] {
			t.Errorf("expected bundle to exclude %s, but it was present", excluded)
		}
	}

	// Also verify manifest.json entries
	manifestFile, err := zr.Open("manifest.json")
	if err != nil {
		t.Fatalf("open manifest.json: %v", err)
	}
	defer manifestFile.Close()
	var m Manifest
	if err := json.NewDecoder(manifestFile).Decode(&m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}

	manifestPaths := make(map[string]bool)
	for _, entry := range m.Entries {
		manifestPaths[entry.Path] = true
	}

	for _, included := range []string{"tree/prompt.tmpl", "tree/notes.tmp.md", "tree/config.tmp.json"} {
		if !manifestPaths[included] {
			t.Errorf("expected manifest to include %s, but it was missing", included)
		}
	}
	for _, excluded := range []string{"tree/tokens-abc.tmp", "tree/nuntius/state.tmp4182"} {
		if manifestPaths[excluded] {
			t.Errorf("expected manifest to exclude %s, but it was present", excluded)
		}
	}
}

// TestBK_F1_ENOENTBenignSkip tests that:
// (c) ENOENT benign-skip: call the staging copy on a dir where a file vanishes
// (using injected hook) and assert no error and exclusion from manifest.
func TestBK_F1_ENOENTBenignSkip(t *testing.T) {
	root := setupTestRoot(t)
	cfg := setupTestConfig(t)
	out := filepath.Join(t.TempDir(), "test-vanish.lararium-backup")

	vanishPath := filepath.Join(root, "vanish.txt")
	if err := os.WriteFile(vanishPath, []byte("I will disappear"), 0o644); err != nil {
		t.Fatal(err)
	}
	keptPath := filepath.Join(root, "kept.txt")
	if err := os.WriteFile(keptPath, []byte("I will stay"), 0o644); err != nil {
		t.Fatal(err)
	}

	origHook := StagePreCopyHook
	defer func() { StagePreCopyHook = origHook }()

	vanished := false
	StagePreCopyHook = func(srcPath string) error {
		if filepath.Base(srcPath) == "vanish.txt" {
			// Delete the listed file between walk-visit and copy
			if err := os.Remove(srcPath); err != nil {
				return err
			}
			vanished = true
		}
		return nil
	}

	// Call the staging copy directly
	stagingDir, err := CreateStagingDir(root)
	if err != nil {
		t.Fatalf("CreateStagingDir: %v", err)
	}
	defer os.RemoveAll(stagingDir)

	err = StageTree(root, stagingDir)
	if err != nil {
		t.Fatalf("StageTree returned error on vanishing file: %v", err)
	}
	if !vanished {
		t.Fatal("vanish.txt was not encountered/deleted by hook")
	}

	// Verify vanished file is not in staging dir, and kept file is
	if _, err := os.Stat(filepath.Join(stagingDir, "vanish.txt")); err == nil {
		t.Fatal("vanish.txt unexpectedly exists in staging directory")
	}
	if _, err := os.Stat(filepath.Join(stagingDir, "kept.txt")); err != nil {
		t.Fatalf("kept.txt missing from staging directory: %v", err)
	}

	// Compress staging into bundle and assert exclusion from manifest
	if err := WriteBundle(stagingDir, cfg, false, root, out); err != nil {
		t.Fatalf("WriteBundle failed: %v", err)
	}

	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()

	manifestFile, err := zr.Open("manifest.json")
	if err != nil {
		t.Fatalf("open manifest.json: %v", err)
	}
	defer manifestFile.Close()

	var m Manifest
	if err := json.NewDecoder(manifestFile).Decode(&m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}

	for _, entry := range m.Entries {
		if entry.Path == "tree/vanish.txt" {
			t.Fatal("vanished file found in manifest entries")
		}
	}

	foundKept := false
	for _, entry := range m.Entries {
		if entry.Path == "tree/kept.txt" {
			foundKept = true
			break
		}
	}
	if !foundKept {
		t.Fatal("kept.txt missing from manifest entries")
	}
}

// TestBK_F2_SelfInclusionRoot tests that --out equal to the root itself must refuse (F2).
func TestBK_F2_SelfInclusionRoot(t *testing.T) {
	root := setupTestRoot(t)

	// CLI helper CheckSelfInclusion: --out == root itself must refuse
	err := CheckSelfInclusion(root, root)
	if err == nil || !strings.Contains(err.Error(), "inside hearth root") {
		t.Fatalf("expected CheckSelfInclusion(root, root) refusal, got: %v", err)
	}

	// CheckSelfInclusion with trailing slash
	err = CheckSelfInclusion(root, root+string(filepath.Separator))
	if err == nil || !strings.Contains(err.Error(), "inside hearth root") {
		t.Fatalf("expected CheckSelfInclusion with trailing slash refusal, got: %v", err)
	}
}
