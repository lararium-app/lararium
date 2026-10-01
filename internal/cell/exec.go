package cell

import (
	"bytes"
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
	Stdin   []byte // piped to the in-cell command (spec §6)
}

// ExecResult is the outcome of a command run inside a cell. Streams
// stay separated and the exact in-cell exit code is preserved (spec §6:
// exit status is the contract between guest command and caller).
type ExecResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// res0 wraps raw streams into an ExecResult for error paths.
func res0(stdout, stderr []byte) *ExecResult {
	return &ExecResult{Stdout: stdout, Stderr: stderr, ExitCode: -1}
}

// runWithStdin is the Stdin-capable twin of Runner.Run: os/exec with
// the byte slice piped to the child (Runner stays byte-only; adding
// stdin to the interface churns every fake for one call site).
//
//nolint:noctx // exec unit churns per call site; timeout is enforced by systemd-run.
func (s *Store) runWithStdin(stdin []byte, cmd string, args ...string) ([]byte, []byte, error) {
	c := exec.Command(cmd, args...)
	c.Stdin = strings.NewReader(string(stdin))
	var out, errBuf bytes.Buffer
	c.Stdout = &out
	c.Stderr = &errBuf
	err := c.Run()
	return out.Bytes(), errBuf.Bytes(), err
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
	// Env reaches the in-guest unit via --setenv (spec §4 proxy vars
	// depend on it; round-2 F9: RunOpts.Env was silently dropped).
	for _, kv := range opts.Env {
		execArgs = append(execArgs, "--setenv="+kv)
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
	var unit string
	for attempt := 0; ; attempt++ {
		unit = fmt.Sprintf("%s-%03d.service", unitBase, attempt)
		attemptArgs := append([]string{"--unit=" + unit}, execArgs...)
		if opts.Stdin != nil {
			stdout, stderr, err = s.runWithStdin(opts.Stdin, "/usr/bin/systemd-run", attemptArgs...)
		} else {
			stdout, stderr, err = s.runner.Run("/usr/bin/systemd-run", attemptArgs...)
		}
		if err == nil || !isBusTransportError(stderr) || time.Now().After(deadline) {
			break
		}
		time.Sleep(1 * time.Second)
	}
	// If the LAST failure is still the machine bus being unreachable,
	// that is a transport failure, NOT the command's exit code —
	// returning ExitCode 1 with nil error made an unreachable cell
	// indistinguishable from `false` (agy round-2 F4).
	if err != nil && isBusTransportError(stderr) {
		return res0(stdout, stderr), fmt.Errorf(
			"machine bus unreachable for %s: %w", id, ErrNotRunning)
	}
	if err != nil && opts.Timeout > 0 {
		// Safety net: stop THE EXACT unit we launched (not a glob —
		// a wildcard stop killed concurrent exec jobs in the same
		// cell on any non-zero exit; agy round-2 F5). If the command
		// already exited, stopping its collected unit is a no-op.
		//nolint:errcheck // already-exited units stop cleanly; nothing to undo
		s.runner.Run("systemctl", "--machine="+id, "stop", unit)
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
