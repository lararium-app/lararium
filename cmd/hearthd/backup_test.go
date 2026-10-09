package main

import (
	"archive/zip"
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/backup"
	"github.com/lararium-app/lararium/internal/surface"
)

func createTestHearthRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "hearth")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "SOUL.md"), []byte("# Soul\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "IDENTITY.md"), []byte("# Identity\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "USER.md"), []byte("# User\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "MEMORY.md"), []byte("# Memory\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "HEARTBEAT.md"), []byte("# Orders\n"), 0o644)

	_ = os.WriteFile(filepath.Join(root, "keys.json"), []byte(`{"anthropic":"sk-ant-test"}`), 0o600)
	_ = os.WriteFile(filepath.Join(root, "tokens.json"), []byte(`{"label1":"tok"}`), 0o600)

	custosDir := filepath.Join(root, "custos")
	_ = os.MkdirAll(filepath.Join(custosDir, "snapshots", "snap1"), 0o755)
	_ = os.WriteFile(filepath.Join(custosDir, "vault.key"), []byte("vault-secret"), 0o600)
	_ = os.WriteFile(filepath.Join(custosDir, "snapshots", "snap1", "vault.db"), []byte("vault-snap"), 0o600)

	_ = os.MkdirAll(filepath.Join(root, "sessions", "main"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "sessions", "main", "session.json"), []byte(`{"id":"main"}`), 0o644)
	ev := `{"seq":1,"t":"msg","ts":"2026-10-09T18:00:00Z"}` + "\n"
	_ = os.WriteFile(filepath.Join(root, "sessions", "main", "events.jsonl"), []byte(ev), 0o644)

	return root
}

func createTestConfig(t *testing.T, home string) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "lararium.yaml")
	content := "hearth:\n  home: " + home + "\nmodels:\n  default: [\"local/model\"]\nproviders:\n  - name: local\n    base_url: http://127.0.0.1:8000\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// TestCLI_SocketBackupHappyPath tests control-socket verb `backup <out_abs> [--no-config]`
// with live daemon listener, ack, progress lines, and done (§4.5.3.1).
func TestCLI_SocketBackupHappyPath(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)
	sockPath := filepath.Join(home, "hearthd.sock")

	srv := &surface.Server{}
	var singleWriterMu sync.RWMutex
	singleWriterLock := func() func() {
		singleWriterMu.Lock()
		return func() { singleWriterMu.Unlock() }
	}

	srv.Backup = func(outAbs string, noConfig bool, progress func(string)) error {
		return backup.CreateFromDaemon(home, cfgPath, outAbs, noConfig, singleWriterLock, progress)
	}

	closer, err := srv.ServeSocket(sockPath, func() error { return nil })
	if err != nil {
		t.Fatalf("ServeSocket: %v", err)
	}
	defer closer.Close()

	// 1. Verify ping
	if err := surface.PingViaSocket(sockPath, 2*time.Second); err != nil {
		t.Fatalf("PingViaSocket: %v", err)
	}

	// 2. Run socket backup
	outAbs := filepath.Join(t.TempDir(), "socket-out.lararium-backup")
	var progressLines []string
	err = surface.BackupViaSocket(sockPath, outAbs, false, func(p string) {
		progressLines = append(progressLines, p)
	})
	if err != nil {
		t.Fatalf("BackupViaSocket failed: %v", err)
	}

	if len(progressLines) == 0 {
		t.Fatal("expected progress lines from socket backup")
	}

	// Verify generated bundle passes verify
	var stdout, stderr bytes.Buffer
	findings, err := backup.Verify(outAbs, &stdout, &stderr)
	if err != nil || len(findings) != 0 {
		t.Fatalf("verify socket backup failed: err=%v, findings=%+v", err, findings)
	}
}

// TestCLI_StaleSocketRefusal tests that when hearthd.sock exists but cannot be pinged,
// create refuses with the exact operator step text (§4.5.3.2, BK6).
func TestCLI_StaleSocketRefusal(t *testing.T) {
	home := createTestHearthRoot(t)
	sockPath := filepath.Join(home, "hearthd.sock")

	// Create a dead/stale socket file (listen then close)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // listener gone, dead socket file remains

	// Attempt ping must fail
	err = surface.PingViaSocket(sockPath, 500*time.Millisecond)
	if err == nil {
		t.Fatal("ping succeeded on dead socket")
	}

	// Verify operator step constant text
	want := OperatorStepStaleSocket
	if !strings.Contains(want, "confirm the daemon process is dead, remove hearthd.sock, retry") {
		t.Fatalf("operator step text mismatch: %s", want)
	}
}

// TestBK5_CustodyCoherenceAndHeldLockRefusal tests that held custos.lock
// causes immediate busy refusal with no leftovers (§4.5.3.3, BK5).
func TestBK5_CustodyCoherenceAndHeldLockRefusal(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)

	// Hold custos.lock
	custosLockPath := filepath.Join(home, "custos.lock")
	f, err := os.OpenFile(custosLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock custos.lock: %v", err)
	}

	outAbs := filepath.Join(t.TempDir(), "held-lock.lararium-backup")
	err = backup.CreateOffline(home, cfgPath, outAbs, false, nil)
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("expected immediate busy refusal for held custos.lock, got %v", err)
	}

	// Verify no leftovers
	if _, err := os.Stat(outAbs); err == nil {
		t.Fatal("partial output file was left behind")
	}
	// Verify no staging dir was left in parent
	parent := filepath.Dir(home)
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".backup-staging-") {
			t.Fatalf("leftover staging directory found: %s", e.Name())
		}
	}
}

// TestBK9_SocketConcurrency tests that a second backup request over socket
// during an active backup refuses immediately (LOCK_NB) (§4.5.3.3, BK9).
func TestBK9_SocketConcurrency(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)
	_ = cfgPath
	sockPath := filepath.Join(home, "hearthd.sock")

	srv := &surface.Server{}
	started := make(chan struct{})
	unblock := make(chan struct{})

	srv.Backup = func(outAbs string, noConfig bool, progress func(string)) error {
		// Acquire backup.lock explicitly
		locks, err := backup.AcquireLocks(home)
		if err != nil {
			return err
		}
		defer locks.Release()
		close(started)
		<-unblock
		return nil
	}

	closer, err := srv.ServeSocket(sockPath, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	// Launch first backup
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = surface.BackupViaSocket(sockPath, filepath.Join(t.TempDir(), "out1.lararium-backup"), false, nil)
	}()

	<-started

	// Second backup over socket while first holds locks must refuse immediately
	out2 := filepath.Join(t.TempDir(), "out2.lararium-backup")
	err2 := surface.BackupViaSocket(sockPath, out2, false, nil)
	if err2 == nil || !strings.Contains(err2.Error(), "busy") {
		t.Fatalf("second socket backup must refuse with busy, got: %v", err2)
	}

	close(unblock)
	wg.Wait()
}

// TestBK15_OfflineAndDaemonCountsAgree tests that offline and daemon paths
// compute identical counts on the same root (§4.5.2, BK15).
func TestBK15_OfflineAndDaemonCountsAgree(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)

	// Add additional session and memory files
	_ = os.MkdirAll(filepath.Join(home, "sessions", "s2"), 0o755)
	_ = os.MkdirAll(filepath.Join(home, "sessions", "s3"), 0o755)
	_ = os.MkdirAll(filepath.Join(home, "memory", "projects"), 0o755)
	_ = os.WriteFile(filepath.Join(home, "memory", "MEMORY.md"), []byte("mem"), 0o644)
	_ = os.WriteFile(filepath.Join(home, "memory", "projects", "notes.md"), []byte("notes"), 0o644)
	_ = os.WriteFile(filepath.Join(home, "memory", "nested.txt"), []byte("not md"), 0o644)

	// Offline backup
	outOffline := filepath.Join(t.TempDir(), "offline.lararium-backup")
	if err := backup.CreateOffline(home, cfgPath, outOffline, false, nil); err != nil {
		t.Fatal(err)
	}

	// Daemon backup
	outDaemon := filepath.Join(t.TempDir(), "daemon.lararium-backup")
	if err := backup.CreateFromDaemon(home, cfgPath, outDaemon, false, nil, nil); err != nil {
		t.Fatal(err)
	}

	readCounts := func(p string) backup.CountsInfo {
		zr, err := zip.OpenReader(p)
		if err != nil {
			t.Fatal(err)
		}
		defer zr.Close()
		mf, _ := zr.File[0].Open()
		b, _ := io.ReadAll(mf)
		mf.Close()
		m, _, _, _ := backup.ParseManifest(b)
		return m.Counts
	}

	cOffline := readCounts(outOffline)
	cDaemon := readCounts(outDaemon)

	if cOffline != cDaemon {
		t.Fatalf("counts disagree: offline=%+v, daemon=%+v", cOffline, cDaemon)
	}
	if cOffline.Sessions != 3 {
		t.Errorf("expected 3 sessions, got %d", cOffline.Sessions)
	}
	if cOffline.MemoryFiles != 2 {
		t.Errorf("expected 2 memory files, got %d", cOffline.MemoryFiles)
	}
}

// TestSecretsHonestyLineVerbatim verifies the secrets-honesty line verbatim (§4.5.2).
func TestSecretsHonestyLineVerbatim(t *testing.T) {
	want := "a bundle is your disk — store it where your disk would be unsafe."
	if backup.SecretsHonestyLine != want {
		t.Fatalf("SecretsHonestyLine = %q, want %q", backup.SecretsHonestyLine, want)
	}
}
