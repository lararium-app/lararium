//go:build !windows

package web

// Opt-in end-to-end test for the guided sign-in window: drives the REAL
// Xvfb -> chromium -> x11vnc chain and proves the sealed wallet round-trips.
// Requires Xvfb, x11vnc, chromium on PATH and network access.
// Run with: LARARIUM_SIGNIN_E2E=1 go test ./internal/web/ -run E2E -v

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func treeHashes(root string) (map[string]string, error) {
	out := map[string]string{}
	//nolint:gosec // G122: walks a temp tree this test itself created with its own files.
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil // unreadable stragglers: skip
		}
		f, oerr := os.Open(path)
		if oerr != nil {
			return nil
		}
		defer f.Close()
		h := sha256.New()
		if _, cerr := io.Copy(h, f); cerr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		out[rel] = fmt.Sprintf("%x", h.Sum(nil))
		return nil
	})
	return out, err
}

func TestSigninWindow_E2E_RealChain(t *testing.T) {
	if os.Getenv("LARARIUM_SIGNIN_E2E") != "1" {
		t.Skip("set LARARIUM_SIGNIN_E2E=1 to run the real Xvfb/chromium/x11vnc chain")
	}
	for _, bin := range []string{"Xvfb", "x11vnc", "chromium"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("prereq %s missing from PATH", bin)
		}
	}

	home := t.TempDir()
	w := NewWallet(home, nil)
	mgr := NewSigninManager(w, nil, WithTimeout(3*time.Minute))

	origin := "https://example.com"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	info, err := mgr.Start(ctx, origin)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	staging := mgr.runner.ProfileDir()

	// VNC endpoint must accept connections on localhost only.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", info.Port), 2*time.Second)
	if err != nil {
		t.Fatalf("vnc dial: %v", err)
	}
	_ = conn.Close()
	if info.Password == "" {
		t.Fatal("empty vnc password")
	}

	// Give chromium time to launch, load, and write profile files.
	time.Sleep(8 * time.Second)

	if err := mgr.Complete(); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := mgr.State(); got != SigninDone {
		t.Fatalf("state after Complete = %s, want Done", got)
	}

	// Sealed and loadable.
	if !w.Has(origin) {
		t.Fatal("wallet has no session after Complete")
	}
	dir1, release1, err := w.Load(origin)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t1, err := treeHashes(dir1)
	if err != nil {
		t.Fatalf("hash load1: %v", err)
	}
	if len(t1) < 5 {
		t.Fatalf("loaded profile has only %d files — chromium wrote nothing?", len(t1))
	}
	if err := release1(); err != nil {
		t.Fatalf("release1: %v", err)
	}
	dir2, release2, err := w.Load(origin)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	t2, err := treeHashes(dir2)
	if err != nil {
		t.Fatalf("hash load2: %v", err)
	}
	if err := release2(); err != nil {
		t.Fatalf("release2: %v", err)
	}
	if len(t1) != len(t2) {
		t.Fatalf("round-trip file count changed: %d vs %d", len(t1), len(t2))
	}
	for k, v := range t1 {
		if t2[k] != v {
			t.Fatalf("round-trip mismatch for %s", k)
		}
	}

	// Staging dir and its passwdfile must be gone.
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging dir still present: %v", err)
	}
	if entries, err := filepath.Glob(filepath.Join(filepath.Dir(staging), ".vnc-*.pwd")); err == nil && len(entries) > 0 {
		t.Fatalf("leftover vnc passwd files: %v", entries)
	}

	// No process referencing the staging profile may survive teardown.
	if out, err := exec.Command("pgrep", "-f", staging).Output(); err == nil && len(out) > 0 {
		t.Fatalf("processes survived teardown: %s", out)
	}
}
