package web

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrIsolationUnavailable is the honest refusal when web.isolate is on but
// bubblewrap is not installed. Nothing ever runs unsandboxed silently.
const ErrIsolationUnavailable = "browser isolation unavailable: install bubblewrap or set web.isolate=false"

// x11SocketDir is the Xvfb unix-socket directory the headed sign-in window
// must reach (the sandbox hides the rest of /tmp).
const x11SocketDir = "/tmp/.X11-unix"

var (
	// isolationHome is a test seam for the daemon user's HOME.
	isolationHome = func() string { return os.Getenv("HOME") }
	// isolationLogf is a test seam for the isolation warning line.
	isolationLogf = log.Printf
)

// isolationWarnOnce logs the single "no process isolation" warning for the
// owner of once (one Browser, one RealRunner).
func isolationWarnOnce(once *sync.Once) {
	once.Do(func() {
		isolationLogf("WARN web: browser runs WITHOUT process isolation (web.isolate=false)")
	})
}

// bwrapAvailable resolves the bubblewrap binary through the execLookPath
// seam. A missing binary yields ErrIsolationUnavailable.
func bwrapAvailable() (string, error) {
	p, err := execLookPath("bwrap")
	if err != nil || p == "" {
		return "", errors.New(ErrIsolationUnavailable)
	}
	return p, nil
}

// isolationParams describes one sandboxed launch.
type isolationParams struct {
	Home        string   // daemon user's HOME; hidden behind a tmpfs
	StateRoot   string   // daemon state root; hidden unless inside Home
	UserDataDir string   // the ONE read-write subtree (the browser profile)
	ROBinds     []string // extra read-only host paths (sign-in: X11 socket)
}

// within reports whether path is dir or lies beneath it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// bwrapProfile returns the fixed bubblewrap argument vector (everything
// before the "--" and the browser command). Flags per bwrap(1):
//
//	--die-with-parent   kill the sandbox if the daemon dies
//	--unshare-user/-uts/-ipc/-pid   fresh user, hostname, IPC and PID namespaces
//	--ro-bind / /       whole host filesystem read-only
//	--dev /dev --proc /proc   minimal device and process views
//	--tmpfs <HOME>      hides ~/.lar-*, ~/.config, ~/.ssh
//	--tmpfs <state>     hides the hearth state (keystore, wallet, tokens);
//	                    skipped when inside HOME; covers /data in Docker
//	--tmpfs /tmp        private scratch
//	--bind <profile>    the only writable host path
//
// The network namespace stays SHARED (no --unshare-net): browsing must
// work, and the host-resolver-rules private-range floor remains the SSRF law.
func bwrapProfile(p isolationParams) []string {
	args := []string{
		"--die-with-parent",
		"--unshare-user", "--unshare-uts", "--unshare-ipc", "--unshare-pid",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
	}
	home := ""
	if p.Home != "" {
		home = filepath.Clean(p.Home)
		args = append(args, "--tmpfs", home)
	}
	if p.StateRoot != "" {
		state := filepath.Clean(p.StateRoot)
		if home == "" || !within(state, home) {
			args = append(args, "--tmpfs", state)
		}
	}
	args = append(args, "--tmpfs", "/tmp")
	for _, ro := range p.ROBinds {
		args = append(args, "--ro-bind-try", ro, ro)
	}
	if p.UserDataDir != "" {
		dir := filepath.Clean(p.UserDataDir)
		args = append(args, "--bind", dir, dir)
	}
	return args
}

// shQuote single-quotes s for a POSIX shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeBwrapShim generates the per-launch launcher script: a 0755 POSIX sh
// file inside a fresh private dir that execs bwrap with the fixed profile and
// forwards "$@" (chromedp's CDP flags) to the real browser. chromedp v0.22
// has no ExecWrapper option, hence the shim behind chromedp.ExecPath.
func writeBwrapShim(bwrap, chromePath string, p isolationParams) (shim string, cleanup func(), err error) {
	// 0700, not 0600: a directory needs the search bit to hold an executable.
	dir, err := os.MkdirTemp("", "lar-isolate-")
	if err != nil {
		return "", nil, fmt.Errorf("create isolation shim dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("secure isolation shim dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	var sb strings.Builder
	sb.WriteString("#!/bin/sh\n# generated per launch: bubblewrap isolation shim\nexec " + shQuote(bwrap))
	for _, a := range bwrapProfile(p) {
		sb.WriteString(" " + shQuote(a))
	}
	sb.WriteString(" -- " + shQuote(chromePath) + " \"$@\"\n")

	shim = filepath.Join(dir, "chrome-isolated.sh")
	if err := os.WriteFile(shim, []byte(sb.String()), 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write isolation shim: %w", err)
	}
	if err := os.Chmod(shim, 0o755); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("chmod isolation shim: %w", err)
	}
	return shim, cleanup, nil
}

// launcherPath returns the path chromedp should exec for this launch: the
// bwrap shim when isolation is on, or chromePath unchanged (plus a one-time
// warning) when it is off.
func (b *Browser) launcherPath(chromePath, userDataDir string) (string, func(), error) {
	if !b.cfg.Isolate {
		isolationWarnOnce(&b.isolationWarn)
		return chromePath, func() {}, nil
	}
	bwrap, err := bwrapAvailable()
	if err != nil {
		return "", nil, err
	}
	return writeBwrapShim(bwrap, chromePath, isolationParams{
		Home:        isolationHome(),
		StateRoot:   b.dataDir,
		UserDataDir: userDataDir,
	})
}
