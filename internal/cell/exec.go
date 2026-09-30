package cell

import (
	"errors"
	"os/exec"
	"strings"
	"time"
)

// isBusTransportError detects systemd-run failures caused by the
// machine bus not being up yet (in-guest dbus still booting).
func isBusTransportError(stderr []byte) bool {
	s := string(stderr)
	return strings.Contains(s, "Failed to connect") && strings.Contains(s, "machine transport")
}

// RunOpts configures a cell exec.
type RunOpts struct {
	CWD     string
	Timeout int // seconds; 0 = no timeout
	Env     []string
}

// ExecResult is the outcome of a command run inside a cell. Streams
// stay separated and the exact in-cell exit code is preserved (spec §6:
// exit status is the contract between guest command and caller).
type ExecResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Run executes a command inside a booted cell via machined.
//
// Per CELL-SPEC §6: systemd-run --machine=<id> --wait --pipe with an
// explicit unit name so a host-side timeout can stop exactly the exec
// scope. (systemd-nspawn has no "exec" verb and no -w flag — live-probed;
// exit code propagates through --wait — live-probed.)
//
// Refuses if machine not registered: returns ErrNotRunning.
func (s *Store) Run(id string, cmd string, opts RunOpts) (*ExecResult, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}

	// Check machine is registered.
	if _, _, err := s.runner.Run("machinectl", "show", id); err != nil {
		return nil, ErrNotRunning
	}

	cwd := opts.CWD
	if cwd == "" {
		cwd = "/workspace"
	}

	unit := "lararium-exec-" + id + "-" +
		time.Now().UTC().Format("20060102T150405.000000Z") + ".service"

	execArgs := []string{
		"--machine=" + id,
		// --quiet: suppress "Running as unit…" / "Finished with…"
		// chatter — cell run's contract is the command's own
		// stdout/stderr + exit code (spec §6).
		"--quiet",
		"--wait",
		"--pipe",
		"--collect",
		"--unit=" + unit,
		"--property=Restart=no",
		"--property=User=keeper",
		"--property=WorkingDirectory=" + cwd,
		"--",
		"/bin/sh", "-c", cmd,
	}

	// systemd-run can race the in-guest dbus daemon: `cell start`
	// returns once the unit is active, but the machine bus only answers
	// after dbus.service finishes booting (~1-5s). Retry *only* the
	// connect failure — a ran command's non-zero exit is not retried.
	var stdout, stderr []byte
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for {
		stdout, stderr, err = s.runner.Run("/usr/bin/systemd-run", execArgs...)
		if err == nil || !isBusTransportError(stderr) || time.Now().After(deadline) {
			break
		}
		// Drop the failed exec unit before retrying under the same name.
		s.runner.Run("systemctl", "reset-failed", unit)
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil && opts.Timeout > 0 {
		// Heuristic host-side timeout: if the caller's context surfaced
		// a deadline error, ensure the exec unit is gone (kills the exec
		// scope only, never the cell — spec §6).
		s.runner.Run("systemctl", "stop", unit)
	}

	res := &ExecResult{Stdout: stdout, Stderr: stderr, ExitCode: 0}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// Command ran; it just exited non-zero. That is a
			// result, not a transport failure.
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, err
	}
	return res, nil
}
