package surface

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// KEYS-SPEC K6: control socket — RELOAD-KEYS/OK protocol, stale-file
// reclaim, live-listener refusal, and the CLI-side error taxonomy.

func tempSocket(t *testing.T) string {
	t.Helper()
	// Unix socket paths are length-limited (~104 chars); t.TempDir can
	// exceed that on some systems, so keep the name short. This is a
	// deliberate deviation from usetesting for path-length safety.
	dir, err := os.MkdirTemp("", "lhsock") //nolint:usetesting // short path required
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func TestSocketReloadHappyPath(t *testing.T) {
	path := tempSocket(t)
	reloads := make(chan struct{}, 8)
	srv := &Server{}
	closer, err := srv.ServeSocket(path, func() error {
		reloads <- struct{}{}
		return nil
	})
	if err != nil {
		t.Fatalf("ServeSocket: %v", err)
	}
	defer closer.Close()

	if err := ReloadViaSocket(path, 2*time.Second); err != nil {
		t.Fatalf("ReloadViaSocket: %v", err)
	}
	select {
	case <-reloads:
	case <-time.After(2 * time.Second):
		t.Fatal("reload never ran")
	}
}

func TestSocketMode0600(t *testing.T) {
	path := tempSocket(t)
	srv := &Server{}
	closer, err := srv.ServeSocket(path, func() error { return nil })
	if err != nil {
		t.Fatalf("ServeSocket: %v", err)
	}
	defer closer.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if m := fi.Mode().Perm(); m != 0o600 {
		t.Errorf("socket mode = %o, want 600", m)
	}
}

func TestSocketStaleFileReclaimed(t *testing.T) {
	path := tempSocket(t)
	// A dead socket file: nothing listening.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // file remains, listener gone

	srv := &Server{}
	closer, err := srv.ServeSocket(path, func() error { return nil })
	if err != nil {
		t.Fatalf("stale file not reclaimed: %v", err)
	}
	defer closer.Close()
	if err := ReloadViaSocket(path, 2*time.Second); err != nil {
		t.Fatalf("reload after reclaim: %v", err)
	}
}

func TestSocketLiveListenerRefused(t *testing.T) {
	path := tempSocket(t)
	srv := &Server{}
	closer, err := srv.ServeSocket(path, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	if _, err := srv.ServeSocket(path, func() error { return nil }); err == nil {
		t.Fatal("second ServeSocket on a live listener must refuse")
	}
}

func TestSocketUnknownCommand(t *testing.T) {
	path := tempSocket(t)
	srv := &Server{}
	closer, err := srv.ServeSocket(path, func() error {
		t.Error("reload must not run for unknown commands")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("BOGUS\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), "unknown command") {
		t.Errorf("reply = %q, want unknown command error", string(buf[:n]))
	}
}

func TestReloadViaSocketNoDaemon(t *testing.T) {
	path := tempSocket(t) // never bound: file absent
	err := ReloadViaSocket(path, time.Second)
	if err == nil {
		t.Fatal("dial to absent socket must fail")
	}
	if !errors.Is(err, ErrNotListening) {
		t.Errorf("err = %v, want ErrNotListening wrap", err)
	}
}

func TestReloadViaSocketStaleFile(t *testing.T) {
	path := tempSocket(t)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // stale file: ECONNREFUSED on dial
	err = ReloadViaSocket(path, time.Second)
	if !errors.Is(err, ErrNotListening) {
		t.Errorf("stale socket err = %v, want ErrNotListening wrap", err)
	}
}

func TestSocketCloseRemovesFile(t *testing.T) {
	path := tempSocket(t)
	srv := &Server{}
	closer, err := srv.ServeSocket(path, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket file survived Close")
	}
}

func TestSocketSelfInclusionSymlinkedSubdir(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	outsideDir := t.TempDir()
	symlinkPath := filepath.Join(outsideDir, "sym_state")
	if err := os.Symlink(stateDir, symlinkPath); err != nil {
		t.Fatal(err)
	}

	// 1. Direct symlinked directory as out path
	if err := checkSelfInclusionSocket(root, symlinkPath); err == nil {
		t.Fatal("expected error passing symlinked state subdir as output, got nil (L-1)")
	}

	// 2. File inside symlinked state subdir
	outInsideSymlink := filepath.Join(symlinkPath, "bundle.lararium-backup")
	if err := checkSelfInclusionSocket(root, outInsideSymlink); err == nil {
		t.Fatal("expected error passing backup path inside symlinked state subdir, got nil (L-1)")
	}

	// 3. Legitimate outside path should not fail
	legitOut := filepath.Join(outsideDir, "legit.lararium-backup")
	if err := checkSelfInclusionSocket(root, legitOut); err != nil {
		t.Fatalf("expected legit outside path to succeed, got: %v", err)
	}
}

func TestSocketCommandReadBounded(t *testing.T) {
	path := tempSocket(t)
	srv := &Server{}
	closer, err := srv.ServeSocket(path, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	// Send 1MiB payload with no newline (L-2)
	payload := bytes.Repeat([]byte("A"), 1024*1024)
	go func() {
		_, _ = conn.Write(payload)
	}()

	replyBuf := make([]byte, 128)
	n, readErr := conn.Read(replyBuf)
	if n == 0 && readErr != nil {
		t.Fatalf("read failed: %v", readErr)
	}
	reply := string(replyBuf[:n])
	if !strings.HasPrefix(reply, "ERR") || !strings.Contains(reply, "too long") {
		t.Errorf("reply = %q, want ERR line too long", reply)
	}

	// Assert connection is closed by server
	buf := make([]byte, 16)
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("expected connection to be closed by server, but read succeeded")
	}
}
