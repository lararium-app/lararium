package surface

import (
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
