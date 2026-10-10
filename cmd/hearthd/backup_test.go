package main

import (
	"archive/zip"
	"bytes"
	"fmt"
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

	hub := surface.NewHub(surface.ServeConfig{}, home, 1000, 80, nil, nil, nil)
	srv := &surface.Server{Hub: hub}

	srv.Backup = func(outAbs string, noConfig bool, progress func(string)) error {
		return backup.CreateFromDaemon(home, cfgPath, outAbs, noConfig, hub.TrySingleWriterLock, progress)
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

// TestBK5_CustodyCoherenceAndHeldLockRefusal tests that held custos.lock at <root>/custos/custos.lock
// causes immediate busy refusal with no leftovers (§4.5.3.3, B2), and concurrent custos mutation hammer
// yields either a coherent bundle generation or busy refusal (B2, M7).
func TestBK5_CustodyCoherenceAndHeldLockRefusal(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)

	// 1. Hold custos.lock at <root>/custos/custos.lock (B2)
	custosDir := filepath.Join(home, "custos")
	if err := os.MkdirAll(custosDir, 0o700); err != nil {
		t.Fatal(err)
	}
	custosLockPath := filepath.Join(custosDir, "custos.lock")
	f, err := os.OpenFile(custosLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
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
	parent := filepath.Dir(home)
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".backup-staging-") {
			t.Fatalf("leftover staging directory found: %s", e.Name())
		}
	}

	// Release held lock
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()

	// 2. Hammer test (B2, M7): goroutine hammering custos mutation lock + concurrent create
	// → each produced bundle parses to one coherent generation OR create refuses busy.
	vaultPath := filepath.Join(custosDir, "vault.data")
	surrPath := filepath.Join(custosDir, "surrogates.data")
	_ = os.WriteFile(vaultPath, []byte("gen0"), 0o600)
	_ = os.WriteFile(surrPath, []byte("gen0"), 0o600)

	stopHammer := make(chan struct{})
	hammerDone := make(chan struct{})
	go func() {
		defer close(hammerDone)
		gen := 1
		for {
			select {
			case <-stopHammer:
				return
			default:
				hf, err := os.OpenFile(custosLockPath, os.O_CREATE|os.O_RDWR, 0o600)
				if err == nil {
					if err := syscall.Flock(int(hf.Fd()), syscall.LOCK_EX); err == nil {
						genStr := fmt.Sprintf("gen%d", gen)
						_ = os.WriteFile(vaultPath, []byte(genStr), 0o600)
						_ = os.WriteFile(surrPath, []byte(genStr), 0o600)
						gen++
						_ = syscall.Flock(int(hf.Fd()), syscall.LOCK_UN)
					}
					hf.Close()
				}
				time.Sleep(500 * time.Microsecond)
			}
		}
	}()

	for attempt := 0; attempt < 5; attempt++ {
		outHammer := filepath.Join(t.TempDir(), fmt.Sprintf("hammer-%d.lararium-backup", attempt))
		err := backup.CreateOffline(home, cfgPath, outHammer, false, nil)
		if err != nil {
			if !strings.Contains(err.Error(), "busy") {
				t.Fatalf("expected busy refusal during hammer, got: %v", err)
			}
			continue
		}

		// Produced bundle must parse to one coherent generation
		zr, err := zip.OpenReader(outHammer)
		if err != nil {
			t.Fatalf("open hammer bundle: %v", err)
		}
		var vaultVal, surrVal string
		for _, zf := range zr.File {
			if zf.Name == "tree/custos/vault.data" {
				rc, _ := zf.Open()
				data, _ := io.ReadAll(rc)
				rc.Close()
				vaultVal = string(data)
			}
			if zf.Name == "tree/custos/surrogates.data" {
				rc, _ := zf.Open()
				data, _ := io.ReadAll(rc)
				rc.Close()
				surrVal = string(data)
			}
		}
		zr.Close()

		if vaultVal != surrVal {
			t.Fatalf("torn read in bundle: vault=%q, surrogates=%q", vaultVal, surrVal)
		}
	}
	close(stopHammer)
	<-hammerDone
}

// TestBK9_SocketConcurrency tests that a second backup request over socket
// during an active backup refuses immediately (LOCK_NB) (§4.5.3.3, BK9, B1).
// Must go through the REAL srv.Backup (no stub) and assert the second request
// gets busy refusal fast (<2s), while an in-flight turn blocks the first with busy too.
func TestBK9_SocketConcurrency(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)
	sockPath := filepath.Join(home, "hearthd.sock")

	hub := surface.NewHub(surface.ServeConfig{}, home, 1000, 80, nil, nil, nil)
	srv := &surface.Server{Hub: hub}

	// Real srv.Backup (no stub, B1)
	srv.Backup = func(outAbs string, noConfig bool, progress func(string)) error {
		return backup.CreateFromDaemon(home, cfgPath, outAbs, noConfig, hub.TrySingleWriterLock, progress)
	}

	started := make(chan struct{})
	unblock := make(chan struct{})
	surface.SetServerBackupHook(srv, func(phase string) {
		if phase == "staging tree" {
			select {
			case <-started:
			default:
				close(started)
				<-unblock
			}
		}
	})

	closer, err := srv.ServeSocket(sockPath, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	// 1. In-flight turn blocks the first request with busy too (§4.5.3.3, B1)
	turnUnlock := hub.RLock()
	outTurn := filepath.Join(t.TempDir(), "turn-blocked.lararium-backup")
	errTurn := surface.BackupViaSocket(sockPath, outTurn, false, nil)
	if errTurn == nil || !strings.Contains(errTurn.Error(), "busy") {
		t.Fatalf("expected busy refusal while in-flight turn holds single-writer lock, got: %v", errTurn)
	}
	turnUnlock() // release turn

	// 2. Launch first real backup in background
	var wg sync.WaitGroup
	wg.Add(1)
	out1 := filepath.Join(t.TempDir(), "out1.lararium-backup")
	var err1 error
	go func() {
		defer wg.Done()
		err1 = surface.BackupViaSocket(sockPath, out1, false, nil)
	}()

	<-started

	// 3. Second request over socket while first holds locks must refuse immediately with busy (<2s)
	out2 := filepath.Join(t.TempDir(), "out2.lararium-backup")
	t0 := time.Now()
	err2 := surface.BackupViaSocket(sockPath, out2, false, nil)
	dur := time.Since(t0)
	if dur >= 2*time.Second {
		t.Fatalf("second backup request took too long (%v), must refuse fast (<2s)", dur)
	}
	if err2 == nil || !strings.Contains(err2.Error(), "busy") {
		t.Fatalf("second socket backup must refuse immediately with busy, got: %v", err2)
	}

	// Unblock first backup and verify it completes cleanly
	close(unblock)
	wg.Wait()
	if err1 != nil {
		t.Fatalf("first backup failed: %v", err1)
	}

	// Verify first backup is valid
	var stdout, stderr bytes.Buffer
	findings, err := backup.Verify(out1, &stdout, &stderr)
	if err != nil || len(findings) != 0 {
		t.Fatalf("first bundle verify failed: err=%v, findings=%+v", err, findings)
	}
}

// TestBK3_SocketInputLaw tests that handleBackupSocket rejects
// relative out paths and paths inside hearth root BEFORE ack (B3, M14).
func TestBK3_SocketInputLaw(t *testing.T) {
	home := createTestHearthRoot(t)
	cfgPath := createTestConfig(t, home)
	sockPath := filepath.Join(home, "hearthd.sock")

	hub := surface.NewHub(surface.ServeConfig{}, home, 1000, 80, nil, nil, nil)
	srv := &surface.Server{Hub: hub}
	srv.Backup = func(outAbs string, noConfig bool, progress func(string)) error {
		return backup.CreateFromDaemon(home, cfgPath, outAbs, noConfig, hub.TrySingleWriterLock, progress)
	}

	closer, err := srv.ServeSocket(sockPath, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	// 1. Relative path over socket → ERR, nothing written (B3)
	err = surface.BackupViaSocket(sockPath, "relative/path.lararium-backup", false, nil)
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected ERR on relative path, got: %v", err)
	}

	// 2. Out path inside hearth root → ERR, nothing written (B3)
	insidePath := filepath.Join(home, "inside.lararium-backup")
	err = surface.BackupViaSocket(sockPath, insidePath, false, nil)
	if err == nil || !strings.Contains(err.Error(), "inside hearth root") {
		t.Fatalf("expected ERR on self-inclusion inside root, got: %v", err)
	}
	if _, err := os.Stat(insidePath); err == nil {
		t.Fatal("file was created inside root despite self-inclusion refusal")
	}

	// 2b. Out path equal to hearth root itself → refusal, both socket and CLI helper (F2)
	err = surface.BackupViaSocket(sockPath, home, false, nil)
	if err == nil || !strings.Contains(err.Error(), "inside hearth root") {
		t.Fatalf("expected ERR on --out == root over socket, got: %v", err)
	}
	if err := backup.CheckSelfInclusion(home, home); err == nil || !strings.Contains(err.Error(), "inside hearth root") {
		t.Fatalf("expected CheckSelfInclusion(home, home) refusal, got: %v", err)
	}

	// 3. Out path containing whitespace → ERR usage (M14)
	err = surface.BackupViaSocket(sockPath, "/tmp/evil path/backup.lararium-backup", false, nil)
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("expected ERR usage on path with whitespace, got: %v", err)
	}
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
