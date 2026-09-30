package cell

import (
	"errors"
	"fmt"
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

	unitBase := "lararium-exec-" + id + "-" +
		time.Now().UTC().Format("20060102T150405.000000Z")

	execArgs := []string{
		"--machine=" + id,
		// --quiet: suppress "Running as unit…" / "Finished with…"
		// chatter — cell run's contract is the command's own
		// stdout/stderr + exit code (spec §6).
		"--quiet",
		"--wait",
		"--pipe",
		"--collect",
		"--property=Restart=no",
		"--property=User=keeper",
		"--property=WorkingDirectory=" + cwd,
	}
	// Host-enforced timeout belongs to the in-guest unit: systemd
	// kills the exec scope when RuntimeMaxSec expires. (agy review
	// F2: opts.Timeout was parsed but never enforced anywhere.)
	if opts.Timeout > 0 {
		execArgs = append(execArgs,
			fmt.Sprintf("--property=RuntimeMaxSec=%d", opts.Timeout))
	}
	execArgs = append(execArgs, "--", "/bin/sh", "-c", cmd)

	// systemd-run can race the in-guest dbus daemon: `cell start`
	// returns once the unit is active, but the machine bus can take
	// up to ~60s to answer on a COLD first boot of a fresh cell on
	// slow storage (live-probed 2026-09-30 on an eMMC bench: manual
	// systemd-run failed for ~45s post-create, then succeeded).
	// Retry *only* the connect failure — a ran command's non-zero
	// exit is not retried. Each attempt uses a fresh unit name: the
	// failed attempt's --collect'd unit can linger as failed under
	// the old name.
	var stdout, stderr []byte
	var err error
	deadline := time.Now().Add(120 * time.Second)
	for attempt := 0; ; attempt++ {
		unit := fmt.Sprintf("%s-%03d.service", unitBase, attempt)
		attemptArgs := append([]string{"--unit=" + unit}, execArgs...)
		stdout, stderr, err = s.runner.Run("/usr/bin/systemd-run", attemptArgs...)
		if err == nil || !isBusTransportError(stderr) || time.Now().After(deadline) {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if err != nil && opts.Timeout > 0 {
		// Safety net: if the caller's deadline surfaced before the
		// in-guest RuntimeMaxSec kill landed, stop any exec units
		// still running INSIDE the cell (--machine — host systemctl
		// has no such unit; agy review F2). Kills exec scopes only,
		// never the cell (spec §6).
		s.runner.Run("systemctl", "--machine="+id, "stop",
			"lararium-exec-"+id+"-*.service")
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
