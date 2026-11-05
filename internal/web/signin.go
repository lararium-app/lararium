package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SigninState represents the lifecycle state of a guided sign-in window session.
type SigninState string

// Lifecycle states of a guided sign-in window.
const (
	SigninIdle     SigninState = "Idle"
	SigninOpening  SigninState = "Opening"
	SigninLive     SigninState = "Live"
	SigninSealing  SigninState = "Sealing"
	SigninDone     SigninState = "Done"
	SigninFailed   SigninState = "Failed"
	SigninTimeout  SigninState = "Timeout"
	SigninCanceled SigninState = "Canceled"
)

// Sentinel errors for sign-in window operations.
var (
	ErrSigninBusy              = errors.New("signin manager is busy")
	ErrSigninIllegalTransition = errors.New("illegal state transition")
)

// ErrPrereqMissing indicates a required system binary is not found on PATH.
type ErrPrereqMissing struct {
	Binary string
	Err    error
}

func (e *ErrPrereqMissing) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("prerequisite binary %q missing: %v", e.Binary, e.Err)
	}
	return fmt.Sprintf("prerequisite binary %q missing", e.Binary)
}

func (e *ErrPrereqMissing) Unwrap() error {
	return e.Err
}

var execLookPath = exec.LookPath

// CheckPrereqs verifies that Xvfb, x11vnc, and chromium are available in PATH.
// If any is missing, it returns a typed *ErrPrereqMissing.
func CheckPrereqs() error {
	for _, bin := range []string{"Xvfb", "x11vnc", "chromium"} {
		if err := CheckPrereqBinary(bin); err != nil {
			return err
		}
	}
	return nil
}

// CheckPrereqBinary checks for the presence of a single binary in PATH.
func CheckPrereqBinary(bin string) error {
	if _, err := execLookPath(bin); err != nil {
		return &ErrPrereqMissing{
			Binary: bin,
			Err:    err,
		}
	}
	return nil
}

// SigninViewInfo holds connection details for the user to view/interact with the sign-in window.
type SigninViewInfo struct {
	Port     int    `json:"port"`
	Password string `json:"password"`
}

// WindowRunner abstracts display server, VNC server, browser, and staging profile path.
// This interface allows complete testing without real display servers or binaries.
type WindowRunner interface {
	StartXvfb(ctx context.Context) (display string, err error)
	StopXvfb() error

	StartX11VNC(ctx context.Context, display string) (port int, password string, err error)
	StopX11VNC() error

	StartChromium(ctx context.Context, display string, profileDir string, targetURL string) error
	StopChromium() error

	ProfileDir() string
}

// SigninManager owns the guided sign-in window lifecycle for one origin.
type SigninManager struct {
	mu      sync.Mutex
	wallet  *Wallet
	runner  WindowRunner
	timeout time.Duration

	state  SigninState
	origin string
	info   SigninViewInfo
	timer  *time.Timer

	xvfbStarted     bool
	vncStarted      bool
	chromiumStarted bool

	teardownOnce sync.Once
}

// SigninManagerOption configures a SigninManager.
type SigninManagerOption func(*SigninManager)

// WithTimeout sets a custom session timeout duration.
func WithTimeout(d time.Duration) SigninManagerOption {
	return func(m *SigninManager) {
		if d > 0 {
			m.timeout = d
		}
	}
}

// NewSigninManager creates a new sign-in window manager.
// If runner is nil and wallet is non-nil, NewRealRunner is used.
func NewSigninManager(wallet *Wallet, runner WindowRunner, opts ...SigninManagerOption) *SigninManager {
	if runner == nil && wallet != nil {
		runner = NewRealRunner(wallet.Home())
	}
	m := &SigninManager{
		wallet:  wallet,
		runner:  runner,
		timeout: 10 * time.Minute,
		state:   SigninIdle,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// State returns the current lifecycle state of the manager.
func (m *SigninManager) State() SigninState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

var legalTransitions = map[SigninState]map[SigninState]bool{
	SigninIdle: {
		SigninOpening: true,
	},
	SigninOpening: {
		SigninLive:     true,
		SigninFailed:   true,
		SigninCanceled: true,
		SigninTimeout:  true,
	},
	SigninLive: {
		SigninSealing:  true,
		SigninCanceled: true,
		SigninTimeout:  true,
		SigninFailed:   true,
	},
	SigninSealing: {
		SigninDone:   true,
		SigninFailed: true,
	},
}

func (m *SigninManager) canTransitionLocked(to SigninState) bool {
	targets, ok := legalTransitions[m.state]
	if !ok {
		return false
	}
	return targets[to]
}

func (m *SigninManager) transitionLocked(to SigninState) error {
	if !m.canTransitionLocked(to) {
		return fmt.Errorf("%w: cannot transition from %s to %s", ErrSigninIllegalTransition, m.state, to)
	}
	m.state = to
	return nil
}

// Start launches the sign-in window and returns connection info for viewing.
// A second Start call on a non-Idle manager returns ErrSigninBusy.
func (m *SigninManager) Start(ctx context.Context, origin string) (SigninViewInfo, error) {
	if _, err := OriginKey(origin); err != nil {
		return SigninViewInfo{}, fmt.Errorf("invalid origin: %w", err)
	}

	m.mu.Lock()
	if m.state != SigninIdle {
		st := m.state
		m.mu.Unlock()
		return SigninViewInfo{}, fmt.Errorf("%w: cannot start window in state %s: %w", ErrSigninIllegalTransition, st, ErrSigninBusy)
	}

	if err := m.transitionLocked(SigninOpening); err != nil {
		m.mu.Unlock()
		return SigninViewInfo{}, err
	}
	m.origin = origin
	m.mu.Unlock()

	cleanupOnFail := func(err error) (SigninViewInfo, error) {
		m.mu.Lock()
		_ = m.transitionLocked(SigninFailed)
		m.teardownLocked(false)
		m.mu.Unlock()
		return SigninViewInfo{}, err
	}

	if m.runner == nil {
		return cleanupOnFail(errors.New("no window runner configured"))
	}

	// 1. Get staging profile dir
	profileDir := m.runner.ProfileDir()
	if profileDir == "" {
		return cleanupOnFail(errors.New("empty staging profile directory"))
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return cleanupOnFail(fmt.Errorf("create staging profile directory: %w", err))
	}

	if ctx.Err() != nil {
		return cleanupOnFail(ctx.Err())
	}

	// 2. Start Xvfb display
	display, err := m.runner.StartXvfb(ctx)
	if err != nil {
		return cleanupOnFail(fmt.Errorf("start Xvfb: %w", err))
	}
	m.mu.Lock()
	m.xvfbStarted = true
	m.mu.Unlock()

	if ctx.Err() != nil {
		return cleanupOnFail(ctx.Err())
	}

	// 3. Start x11vnc server
	port, password, err := m.runner.StartX11VNC(ctx, display)
	if err != nil {
		return cleanupOnFail(fmt.Errorf("start x11vnc: %w", err))
	}
	m.mu.Lock()
	m.vncStarted = true
	m.mu.Unlock()

	if ctx.Err() != nil {
		return cleanupOnFail(ctx.Err())
	}

	// 4. Start Chromium window
	targetURL := origin
	if !strings.Contains(targetURL, "://") {
		targetURL = "https://" + targetURL
	}
	if err := m.runner.StartChromium(ctx, display, profileDir, targetURL); err != nil {
		return cleanupOnFail(fmt.Errorf("start chromium: %w", err))
	}
	m.mu.Lock()
	m.chromiumStarted = true

	// Check if canceled or state changed while opening
	if m.state != SigninOpening {
		st := m.state
		m.teardownLocked(false)
		m.mu.Unlock()
		return SigninViewInfo{}, fmt.Errorf("window %s during start", strings.ToLower(string(st)))
	}

	if err := m.transitionLocked(SigninLive); err != nil {
		m.teardownLocked(false)
		m.mu.Unlock()
		return SigninViewInfo{}, err
	}

	m.info = SigninViewInfo{
		Port:     port,
		Password: password,
	}
	info := m.info

	// Schedule session timeout
	if m.timeout > 0 {
		m.timer = time.AfterFunc(m.timeout, func() {
			m.handleTimeout()
		})
	}
	m.mu.Unlock()

	return info, nil
}

func (m *SigninManager) handleTimeout() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state != SigninOpening && m.state != SigninLive {
		return
	}
	_ = m.transitionLocked(SigninTimeout)
	m.teardownLocked(false)
}

// Cancel terminates the sign-in session and cleans up resources without saving.
func (m *SigninManager) Cancel() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state != SigninOpening && m.state != SigninLive {
		st := m.state
		return fmt.Errorf("%w: cannot cancel in state %s", ErrSigninIllegalTransition, st)
	}

	if m.timer != nil {
		m.timer.Stop()
	}

	if err := m.transitionLocked(SigninCanceled); err != nil {
		return err
	}

	m.teardownLocked(false)
	return nil
}

// Complete seals the staged profile directory into the wallet and tears down the window.
func (m *SigninManager) Complete() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state != SigninLive {
		st := m.state
		return fmt.Errorf("%w: cannot complete in state %s", ErrSigninIllegalTransition, st)
	}

	if m.timer != nil {
		m.timer.Stop()
	}

	if err := m.transitionLocked(SigninSealing); err != nil {
		return err
	}

	// Teardown processes gracefully while sealing (profile flushes to stagingDir)
	m.teardownLocked(true)

	stagingDir := m.runner.ProfileDir()
	origin := m.origin
	wallet := m.wallet

	if wallet == nil {
		_ = os.RemoveAll(stagingDir)
		_ = m.transitionLocked(SigninFailed)
		return errors.New("no wallet configured for manager")
	}

	if err := wallet.Store(origin, stagingDir); err != nil {
		_ = os.RemoveAll(stagingDir)
		_ = m.transitionLocked(SigninFailed)
		return fmt.Errorf("seal wallet: %w", err)
	}

	// Staging dir is now sealed into the wallet; clean it up
	_ = os.RemoveAll(stagingDir)

	return m.transitionLocked(SigninDone)
}

func (m *SigninManager) teardownLocked(isSealing bool) {
	m.teardownOnce.Do(func() {
		startedChromium := m.chromiumStarted
		startedVNC := m.vncStarted
		startedXvfb := m.xvfbStarted
		m.chromiumStarted = false
		m.vncStarted = false
		m.xvfbStarted = false
		runner := m.runner

		if runner == nil {
			return
		}

		// Teardown ALWAYS and in order on every exit path:
		// 1. chromium graceful-exit first (profile must flush)
		if startedChromium {
			_ = runner.StopChromium()
		}
		// 2. then vnc
		if startedVNC {
			_ = runner.StopX11VNC()
		}
		// 3. then xvfb
		if startedXvfb {
			_ = runner.StopXvfb()
		}
		// 4. remove staging dir only when NOT sealing
		if !isSealing {
			if dir := runner.ProfileDir(); dir != "" {
				_ = os.RemoveAll(dir)
			}
		}
	})
}

// RealRunner implements WindowRunner using real system binaries.
// Processes are spawned with context.WithoutCancel: their lifetime belongs to
// the sign-in session, never to whichever request happened to open the window.
// Teardown is guaranteed by the SigninManager state machine on every exit path.
type RealRunner struct {
	mu sync.Mutex

	walletHome string
	profileDir string

	xvfbCmd     *exec.Cmd
	xvfbDisplay string

	vncCmd     *exec.Cmd
	vncPort    int
	vncPwd     string
	vncPwdFile string

	chromiumCmd *exec.Cmd
}

// NewRealRunner creates a RealRunner rooted under walletHome.
func NewRealRunner(walletHome string) *RealRunner {
	randBytes := make([]byte, 16)
	if _, err := rand.Read(randBytes); err != nil {
		randBytes = []byte(fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	stagingDir := filepath.Join(walletHome, "signin-staging", hex.EncodeToString(randBytes))

	return &RealRunner{
		walletHome: walletHome,
		profileDir: stagingDir,
	}
}

// ProfileDir returns the staging profile directory path.
func (r *RealRunner) ProfileDir() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.profileDir
}

// StartXvfb picks a free display >= 99 and launches Xvfb.
func (r *RealRunner) StartXvfb(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := CheckPrereqBinary("Xvfb"); err != nil {
		return "", err
	}

	display, err := findFreeDisplay(context.WithoutCancel(ctx))
	if err != nil {
		return "", fmt.Errorf("find free display: %w", err)
	}

	cmd := exec.CommandContext(context.WithoutCancel(ctx), "Xvfb", display, "-screen", "0", "1280x900x24", "-nolisten", "tcp")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start Xvfb %s: %w", display, err)
	}

	r.xvfbCmd = cmd
	r.xvfbDisplay = display

	dispNum := strings.TrimPrefix(display, ":")
	socketPath := fmt.Sprintf("/tmp/.X11-unix/X%s", dispNum)
	deadline := time.Now().Add(3 * time.Second)
	started := false
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			started = true
			break
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return "", fmt.Errorf("xvfb exited prematurely: %v", cmd.ProcessState)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !started {
		_ = cmd.Process.Kill()
		return "", fmt.Errorf("timeout waiting for Xvfb display socket %s", socketPath)
	}

	return display, nil
}

// StopXvfb gracefully stops the Xvfb display process.
func (r *RealRunner) StopXvfb() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.xvfbCmd == nil || r.xvfbCmd.Process == nil {
		return nil
	}

	_ = r.xvfbCmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() {
		done <- r.xvfbCmd.Wait()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = r.xvfbCmd.Process.Kill()
		<-done
	}

	if r.xvfbDisplay != "" {
		dispNum := strings.TrimPrefix(r.xvfbDisplay, ":")
		_ = os.Remove(fmt.Sprintf("/tmp/.X11-unix/X%s", dispNum))
		_ = os.Remove(fmt.Sprintf("/tmp/.X%s-lock", dispNum))
	}

	r.xvfbCmd = nil
	r.xvfbDisplay = ""
	return nil
}

// StartX11VNC starts x11vnc on localhost with an ephemeral random password.
func (r *RealRunner) StartX11VNC(ctx context.Context, display string) (int, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := CheckPrereqBinary("x11vnc"); err != nil {
		return 0, "", err
	}

	port, err := findFreePort(context.WithoutCancel(ctx))
	if err != nil {
		return 0, "", fmt.Errorf("find free port: %w", err)
	}

	pwdBytes := make([]byte, 16)
	if _, err := rand.Read(pwdBytes); err != nil {
		return 0, "", fmt.Errorf("generate vnc password: %w", err)
	}
	pwd := hex.EncodeToString(pwdBytes)

	// Password goes through a 0600 file, never the command line (ps-visible).
	stagingRoot := filepath.Dir(r.profileDir)
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		return 0, "", fmt.Errorf("create staging root: %w", err)
	}
	pwdFile := filepath.Join(stagingRoot, ".vnc-"+hex.EncodeToString(pwdBytes[:4])+".pwd")
	if err := os.WriteFile(pwdFile, []byte(pwd+"\n"), 0o600); err != nil {
		return 0, "", fmt.Errorf("write vnc passwd file: %w", err)
	}
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "x11vnc",
		"-display", display,
		"-localhost",
		"-rfbport", strconv.Itoa(port),
		"-passwdfile", pwdFile,
		"-forever",
		"-shared",
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		_ = os.Remove(pwdFile)
		return 0, "", fmt.Errorf("start x11vnc: %w", err)
	}

	r.vncCmd = cmd
	r.vncPort = port
	r.vncPwd = pwd
	r.vncPwdFile = pwdFile

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(3 * time.Second)
	started := false
	dialer := net.Dialer{Timeout: 50 * time.Millisecond}
	dialCtx := context.WithoutCancel(ctx)
	for time.Now().Before(deadline) {
		conn, err := dialer.DialContext(dialCtx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			started = true
			break
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return 0, "", fmt.Errorf("x11vnc exited prematurely: %v", cmd.ProcessState)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !started {
		_ = cmd.Process.Kill()
		return 0, "", fmt.Errorf("timeout waiting for x11vnc on port %d", port)
	}

	return port, pwd, nil
}

// StopX11VNC terminates the x11vnc server process.
func (r *RealRunner) StopX11VNC() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.vncCmd == nil || r.vncCmd.Process == nil {
		return nil
	}

	_ = r.vncCmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() {
		done <- r.vncCmd.Wait()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = r.vncCmd.Process.Kill()
		<-done
	}

	r.vncCmd = nil
	r.vncPort = 0
	r.vncPwd = ""
	if r.vncPwdFile != "" {
		_ = os.Remove(r.vncPwdFile)
		r.vncPwdFile = ""
	}
	return nil
}

// StopVNC aliases StopX11VNC.
func (r *RealRunner) StopVNC() error {
	return r.StopX11VNC()
}

// StartChromium launches headed chromium with isolation flags in the staging profile.
func (r *RealRunner) StartChromium(ctx context.Context, display, profileDir, targetURL string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := CheckPrereqBinary("chromium"); err != nil {
		return err
	}

	args := []string{
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--user-data-dir=" + profileDir,
		"--no-first-run",
		"--window-size=1280,900",
		targetURL,
	}

	cmd := exec.CommandContext(context.WithoutCancel(ctx), "chromium", args...)
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start chromium: %w", err)
	}

	r.chromiumCmd = cmd
	return nil
}

// StopChromium stops chromium gracefully so profile data flushes to disk.
func (r *RealRunner) StopChromium() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.chromiumCmd == nil || r.chromiumCmd.Process == nil {
		return nil
	}

	_ = r.chromiumCmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() {
		done <- r.chromiumCmd.Wait()
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = r.chromiumCmd.Process.Kill()
		<-done
	}

	r.chromiumCmd = nil
	return nil
}

func findFreeDisplay(ctx context.Context) (string, error) {
	for disp := 99; disp < 1000; disp++ {
		sock := fmt.Sprintf("/tmp/.X11-unix/X%d", disp)
		if _, err := os.Stat(sock); err == nil {
			continue
		}
		lock := fmt.Sprintf("/tmp/.X%d-lock", disp)
		if _, err := os.Stat(lock); err == nil {
			continue
		}
		lc := net.ListenConfig{}
		ln, err := lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", 6000+disp))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return fmt.Sprintf(":%d", disp), nil
	}
	return "", errors.New("no free display >= :99 found")
}

func findFreePort(ctx context.Context) (int, error) {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("free port: unexpected address type %T", ln.Addr())
	}
	return tcpAddr.Port, nil
}
