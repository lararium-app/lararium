package surface_test

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/surface"
)

// L-1: surface/socket.go self-inclusion check (~203): use filepath.EvalSymlinks
// on both the state dir and the requested output path before the prefix test, so a
// symlink inside state can't alias out (or in). Test with a symlinked subdir.
func TestL1_SocketSelfInclusionSymlinkedSubdir(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	outsideDir := t.TempDir()

	// 1. Create a real subdir inside root and a symlink inside root pointing to it.
	realSub := filepath.Join(realRoot, "real_sub")
	if err := os.Mkdir(realSub, 0o700); err != nil {
		t.Fatal(err)
	}
	symSub := filepath.Join(realRoot, "sym_sub")
	if err := os.Symlink(realSub, symSub); err != nil {
		t.Fatal(err)
	}

	// 2. Create an outside symlink pointing into root.
	outsideSymlinkToRoot := filepath.Join(outsideDir, "link_to_root")
	if err := os.Symlink(realRoot, outsideSymlinkToRoot); err != nil {
		t.Fatal(err)
	}

	// 3. Create a symlink inside root pointing outside.
	insideSymlinkToOutside := filepath.Join(realRoot, "link_to_outside")
	if err := os.Symlink(outsideDir, insideSymlinkToOutside); err != nil {
		t.Fatal(err)
	}

	// Setup Server and socket
	sockPath := tempSocket(t)
	srv := &surface.Server{}
	srv.Backup = func(outAbs string, noConfig bool, progress func(string)) error {
		return nil
	}

	closer, err := srv.ServeSocket(sockPath, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	surface.SetServerHearthHome(srv, realRoot)

	sendBackup := func(outPath string) string {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Fatalf("dial socket: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := fmt.Fprintf(conn, "backup %s\n", outPath); err != nil {
			t.Fatalf("write backup command: %v", err)
		}
		reader := bufio.NewReader(conn)
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		return strings.TrimSpace(line)
	}

	// Test A: Output path inside symlinked subdir of root must be refused (self-inclusion).
	respA := sendBackup(filepath.Join(symSub, "backup.tar"))
	if !strings.HasPrefix(respA, "ERR") || !strings.Contains(respA, "self-inclusion refused") {
		t.Errorf("backup inside sym_sub: got %q, want ERR self-inclusion refused", respA)
	}

	// Test B: Output path via outside symlink pointing into root must be refused (self-inclusion).
	respB := sendBackup(filepath.Join(outsideSymlinkToRoot, "backup.tar"))
	if !strings.HasPrefix(respB, "ERR") || !strings.Contains(respB, "self-inclusion refused") {
		t.Errorf("backup via outsideSymlinkToRoot: got %q, want ERR self-inclusion refused", respB)
	}

	// Test C: Output path via symlink inside root pointing outside must be accepted.
	respC := sendBackup(filepath.Join(insideSymlinkToOutside, "backup.tar"))
	if !strings.HasPrefix(respC, "OK") {
		t.Errorf("backup via insideSymlinkToOutside: got %q, want OK", respC)
	}
}

// tempSocket helper for tests in surface_test package.
func tempSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lhsock") //nolint:usetesting // short path required for unix socket
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}
