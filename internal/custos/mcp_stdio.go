// Stdio transport runtime, exact child environment composition, rotating stderr
// capture, and process-group lifecycle per MCP-SPEC §6, §7, §11 (I1, I2, I7).

package custos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxStderrFileBytes = 1024 * 1024 // 1 MiB per file per §6
	maxStderrFiles     = 3           // 3 files max
	defaultGracePeriod = 5 * time.Second
)

// RotatingStderrWriter captures child stderr to rotating files (1 MiB x 3)
// in the connector-private directory, after scrubbing injected credential
// needles per MCP-SPEC §6.
type RotatingStderrWriter struct {
	mu      sync.Mutex
	dir     string
	needles []string
	file    *os.File
	curSize int64
}

// NewRotatingStderrWriter creates a writer writing to <dir>/stderr.log.
func NewRotatingStderrWriter(dir string, needles []string) (*RotatingStderrWriter, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir connector dir: %w", err)
	}

	w := &RotatingStderrWriter{
		dir:     dir,
		needles: needles,
	}

	p := filepath.Join(dir, "stderr.log")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open stderr file: %w", err)
	}
	w.file = f

	fi, err := f.Stat()
	if err == nil {
		w.curSize = fi.Size()
	}

	return w, nil
}

// Write scrubs injected values and appends to stderr.log, rotating when
// exceeding 1 MiB.
func (w *RotatingStderrWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Scrub injected needles from stderr output per §6
	scrubbed := ScrubString(string(p), w.needles)
	b := []byte(scrubbed)

	if w.file == nil {
		return len(p), nil
	}

	if w.curSize+int64(len(b)) > maxStderrFileBytes {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(b)
	w.curSize += int64(n)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// rotateLocked rotates stderr.log -> stderr.log.1 -> stderr.log.2 under lock.
func (w *RotatingStderrWriter) rotateLocked() error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}

	p0 := filepath.Join(w.dir, "stderr.log")
	p1 := filepath.Join(w.dir, "stderr.log.1")
	p2 := filepath.Join(w.dir, "stderr.log.2")

	// Oldest dropped: remove .2
	_ = os.Remove(filepath.Clean(p2)) //nolint:gosec // rotation within connector private directory
	// Rename .1 -> .2
	_ = os.Rename(filepath.Clean(p1), filepath.Clean(p2)) //nolint:gosec // rotation within connector private directory
	// Rename .log -> .1
	_ = os.Rename(filepath.Clean(p0), filepath.Clean(p1)) //nolint:gosec // rotation within connector private directory

	f, err := os.OpenFile(filepath.Clean(p0), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // rotation within connector private directory
	if err != nil {
		return fmt.Errorf("rotate open stderr: %w", err)
	}
	w.file = f
	w.curSize = 0
	return nil
}

// Close closes the underlying open file.
func (w *RotatingStderrWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

// ComposeChildEnv creates the exact environment for the child process per §6:
// declared env entries resolved + safe base PATH, HOME, TMPDIR.
// Nothing from the daemon env is inherited (I1).
// Returns child env slice, resolved needles, literal env keys, and error.
func ComposeChildEnv(
	cfg *MCPServerConfig,
	connectorDir string,
	resolver func(string) (string, error),
) ([]string, []string, []string, error) {
	basePath := "/usr/local/bin:/usr/bin:/bin:" + filepath.Dir(cfg.Command)
	childEnv := []string{
		"HOME=" + connectorDir,
		"TMPDIR=" + connectorDir,
		"PATH=" + basePath,
	}

	var needles []string
	var literalKeys []string

	for k, v := range cfg.Env {
		switch {
		case strings.HasPrefix(v, "${vault:") && strings.HasSuffix(v, "}"):
			if resolver == nil {
				return nil, nil, nil, errors.New("resolver required for vault reference")
			}
			resolved, err := resolver(v)
			if err != nil || resolved == "" {
				return nil, nil, nil, fmt.Errorf("resolve %s: %w", v, err)
			}
			childEnv = append(childEnv, k+"="+resolved)
			if len(resolved) >= scrubMinLength {
				needles = append(needles, resolved)
			}

		case strings.HasPrefix(v, "${env:") && strings.HasSuffix(v, "}"):
			var resolved string
			if resolver != nil {
				r, err := resolver(v)
				if err == nil {
					resolved = r
				}
			}
			if resolved == "" {
				envVar := strings.TrimSuffix(strings.TrimPrefix(v, "${env:"), "}")
				resolved = os.Getenv(envVar)
			}
			if resolved == "" {
				return nil, nil, nil, fmt.Errorf("environment variable %s missing", v)
			}
			childEnv = append(childEnv, k+"="+resolved)
			if len(resolved) >= scrubMinLength {
				needles = append(needles, resolved)
			}

		default:
			// Literal value
			childEnv = append(childEnv, k+"="+v)
			literalKeys = append(literalKeys, k)
		}
	}

	return childEnv, needles, literalKeys, nil
}

// ChildProcess tracks a running child process group and handles synchronous
// group-kill per MCP-SPEC §7 / I7.
type ChildProcess struct {
	cmd      *exec.Cmd
	waitDone chan error
	waitErr  error
	pgid     int
	stderr   io.Closer
}

// SpawnChildProcess starts a subprocess with Setpgid: true and configured
// exact env.
func SpawnChildProcess(
	command string,
	args []string,
	env []string,
	dir string,
	stderr io.Writer,
	stderrCloser io.Closer,
) (*ChildProcess, io.ReadCloser, io.WriteCloser, error) {
	cmd := exec.CommandContext(context.Background(), command, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("cmd stdout pipe: %w", err)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, nil, nil, fmt.Errorf("cmd stdin pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stdin.Close()
		return nil, nil, nil, err
	}

	cp := &ChildProcess{
		cmd:      cmd,
		pgid:     cmd.Process.Pid,
		waitDone: make(chan error, 1),
		stderr:   stderrCloser,
	}

	go func() {
		err := cmd.Wait()
		cp.waitErr = err
		cp.waitDone <- err
		close(cp.waitDone)
		if cp.stderr != nil {
			_ = cp.stderr.Close()
		}
	}()

	return cp, stdout, stdin, nil
}

// WaitChan returns a channel that receives when the child process exits.
func (cp *ChildProcess) WaitChan() <-chan error {
	return cp.waitDone
}

// KillGroup kills the process group with SIGTERM, waits up to grace period,
// then sends SIGKILL to the GROUP per MCP-SPEC §7 / I7.
// Guaranteed to complete before returning.
func (cp *ChildProcess) KillGroup(grace time.Duration) error {
	if cp == nil || cp.cmd == nil || cp.cmd.Process == nil {
		return nil
	}

	if grace <= 0 {
		grace = defaultGracePeriod
	}

	pgid := cp.pgid

	// 1. Send SIGTERM to the process group (-pgid)
	_ = syscall.Kill(-pgid, syscall.SIGTERM)

	select {
	case err := <-cp.waitDone:
		return err
	case <-time.After(grace):
		// 2. Timeout: send SIGKILL to the process group (-pgid)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return <-cp.waitDone
	}
}
