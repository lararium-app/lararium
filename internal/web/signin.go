package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
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

var (
	execLookPath = exec.LookPath
	osStat       = os.Stat
)

// CheckNoVNCAssets verifies that the noVNC static assets directory exists.
// If missing or not a directory, it returns a typed *ErrPrereqMissing naming "novnc assets".
func CheckNoVNCAssets(dir string) error {
	if dir == "" {
		dir = DefaultNoVNCDir
	}
	fi, err := osStat(dir)
	if err != nil || !fi.IsDir() {
		return &ErrPrereqMissing{
			Binary: "novnc assets",
			Err:    err,
		}
	}
	return nil
}

// CheckPrereqs verifies that Xvfb, x11vnc, chromium, and websockify are available in PATH,
// and that the noVNC assets directory exists.
// If any is missing, it returns a typed *ErrPrereqMissing.
func CheckPrereqs(novncDir ...string) error {
	for _, bin := range []string{"Xvfb", "x11vnc", "chromium", "websockify"} {
		if err := CheckPrereqBinary(bin); err != nil {
			return err
		}
	}
	dir := DefaultNoVNCDir
	if len(novncDir) > 0 && novncDir[0] != "" {
		dir = novncDir[0]
	}
	return CheckNoVNCAssets(dir)
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

// BuildViewURL constructs the view URL for a sign-in token given a base URL and listen address.
func BuildViewURL(viewBaseURL string, listenAddr string, token string) string {
	viewPath := "/signin/v/" + token
	viewBaseURL = strings.TrimSpace(viewBaseURL)
	if viewBaseURL != "" {
		return strings.TrimRight(viewBaseURL, "/") + viewPath
	}

	// Request-less fallback: http://<hostname>:<serve port>
	var port string
	if listenAddr != "" {
		if _, p, err := net.SplitHostPort(listenAddr); err == nil {
			port = p
		} else if idx := strings.LastIndex(listenAddr, ":"); idx >= 0 {
			port = listenAddr[idx+1:]
		}
	}

	hostname, err := os.Hostname()
	if err == nil && hostname != "" && port != "" {
		return fmt.Sprintf("http://%s%s", net.JoinHostPort(hostname, port), viewPath)
	}

	// If unresolvable, return relative URL and let the card render it.
	return viewPath
}

// SigninViewInfo holds connection details for the user to view/interact with the sign-in window.
type SigninViewInfo struct {
	Port       int    `json:"port"`
	Password   string `json:"password"`
	BridgePort int    `json:"bridge_port,omitempty"`
	AssetsDir  string `json:"assets_dir,omitempty"`
	ViewURL    string `json:"view_url,omitempty"`
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

	StartWebsockify(ctx context.Context, rfbPort int, bindAddr string) (port int, err error)
	StopWebsockify() error

	ProfileDir() string
}

// SigninManager owns the guided sign-in window lifecycle for one origin.
type SigninManager struct {
	mu      sync.Mutex
	wallet  *Wallet
	runner  WindowRunner
	timeout time.Duration

	bindAddr    string
	viewBaseURL string
	listenAddr  string
	novncDir    string

	token       string
	tokenExpiry time.Time
	cardID      string

	state  SigninState
	origin string
	info   SigninViewInfo
	timer  *time.Timer

	xvfbStarted       bool
	vncStarted        bool
	websockifyStarted bool
	chromiumStarted   bool
	assetsDir         string

	cardResolver func(cardID string, allow bool) error

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

// WithBindAddr sets the local bind address for the websockify bridge.
func WithBindAddr(addr string) SigninManagerOption {
	return func(m *SigninManager) {
		if addr != "" {
			m.bindAddr = addr
		}
	}
}

// WithViewBaseURL sets the external view base URL for sign-in links.
func WithViewBaseURL(url string) SigninManagerOption {
	return func(m *SigninManager) {
		m.viewBaseURL = url
	}
}

// WithListenAddr sets the daemon HTTP listen address for fallback view URL generation.
func WithListenAddr(addr string) SigninManagerOption {
	return func(m *SigninManager) {
		m.listenAddr = addr
	}
}

// WithNoVNCDir sets the noVNC static assets directory.
func WithNoVNCDir(dir string) SigninManagerOption {
	return func(m *SigninManager) {
		if dir != "" {
			m.novncDir = dir
		}
	}
}

// WithCardResolver sets the callback used to resolve the custody card on done/cancel.
func WithCardResolver(fn func(cardID string, allow bool) error) SigninManagerOption {
	return func(m *SigninManager) {
		m.cardResolver = fn
	}
}

// NewSigninManager creates a new sign-in window manager.
// If runner is nil and wallet is non-nil, NewRealRunner is used.
func NewSigninManager(wallet *Wallet, runner WindowRunner, opts ...SigninManagerOption) *SigninManager {
	if runner == nil && wallet != nil {
		runner = NewRealRunner(wallet.Home())
	}
	m := &SigninManager{
		wallet:   wallet,
		runner:   runner,
		timeout:  10 * time.Minute,
		bindAddr: "127.0.0.1",
		novncDir: DefaultNoVNCDir,
		state:    SigninIdle,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Token returns the active one-time token for the session.
func (m *SigninManager) Token() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.token
}

// ValidateToken checks if token matches the active session token and has not expired.
func (m *SigninManager) ValidateToken(token string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token == "" || token == "" {
		return false
	}
	if time.Now().After(m.tokenExpiry) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(m.token), []byte(token)) == 1
}

// AssetsDir returns the staging assets directory path.
func (m *SigninManager) AssetsDir() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.assetsDir
}

// Timeout returns the session timeout.
func (m *SigninManager) Timeout() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.timeout
}

// SetCardID stores the associated custody card ID.
func (m *SigninManager) SetCardID(cardID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cardID = cardID
}

// CardID returns the associated custody card ID.
func (m *SigninManager) CardID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cardID
}

// SetCardResolver sets the card resolver callback.
func (m *SigninManager) SetCardResolver(fn func(cardID string, allow bool) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cardResolver = fn
}

// ResolveCard resolves the pending custody card via the installed resolver.
func (m *SigninManager) ResolveCard(allow bool) error {
	m.mu.Lock()
	resolver := m.cardResolver
	cardID := m.cardID
	m.mu.Unlock()
	if resolver != nil && cardID != "" {
		return resolver(cardID, allow)
	}
	return nil
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

	// 4. Start websockify bridge
	bindAddr := m.bindAddr
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	wsPort, err := m.runner.StartWebsockify(ctx, port, bindAddr)
	if err != nil {
		return cleanupOnFail(fmt.Errorf("start websockify: %w", err))
	}
	m.mu.Lock()
	m.websockifyStarted = true
	m.mu.Unlock()

	if ctx.Err() != nil {
		return cleanupOnFail(ctx.Err())
	}

	// 5. Start Chromium window
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

	// Mint 256-bit one-time token
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		m.teardownLocked(false)
		m.mu.Unlock()
		return SigninViewInfo{}, fmt.Errorf("generate signin token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	viewURL := BuildViewURL(m.viewBaseURL, m.listenAddr, token)

	var assetsDir string
	if ar, ok := m.runner.(interface{ AssetsDir() string }); ok {
		assetsDir = ar.AssetsDir()
	}

	m.token = token
	m.tokenExpiry = time.Now().Add(m.timeout)
	m.assetsDir = assetsDir

	m.info = SigninViewInfo{
		Port:       port,
		Password:   password,
		BridgePort: wsPort,
		AssetsDir:  assetsDir,
		ViewURL:    viewURL,
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
		startedWebsockify := m.websockifyStarted
		startedChromium := m.chromiumStarted
		startedVNC := m.vncStarted
		startedXvfb := m.xvfbStarted
		m.websockifyStarted = false
		m.chromiumStarted = false
		m.vncStarted = false
		m.xvfbStarted = false
		runner := m.runner

		if runner == nil {
			return
		}

		// Teardown ALWAYS and in order on every exit path:
		// 1. websockify stops at the front
		if startedWebsockify {
			_ = runner.StopWebsockify()
		}
		// 2. chromium graceful-exit first (profile must flush)
		if startedChromium {
			_ = runner.StopChromium()
		}
		// 3. then vnc
		if startedVNC {
			_ = runner.StopX11VNC()
		}
		// 4. then xvfb
		if startedXvfb {
			_ = runner.StopXvfb()
		}
		// 5. remove staging dir only when NOT sealing
		if !isSealing {
			if dir := runner.ProfileDir(); dir != "" {
				_ = os.RemoveAll(dir)
			}
		}
		if m.assetsDir != "" {
			_ = os.RemoveAll(m.assetsDir)
			m.assetsDir = ""
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
	novncDir   string

	xvfbCmd     *exec.Cmd
	xvfbDisplay string

	vncCmd     *exec.Cmd
	vncPort    int
	vncPwd     string
	vncPwdFile string

	wsCmd     *exec.Cmd
	wsPort    int
	assetsDir string

	chromiumCmd *exec.Cmd
}

// RealRunnerOption configures a RealRunner.
type RealRunnerOption func(*RealRunner)

// WithRunnerNoVNCDir sets the noVNC assets directory for RealRunner.
func WithRunnerNoVNCDir(dir string) RealRunnerOption {
	return func(r *RealRunner) {
		if dir != "" {
			r.novncDir = dir
		}
	}
}

// NewRealRunner creates a RealRunner rooted under walletHome.
func NewRealRunner(walletHome string, opts ...RealRunnerOption) *RealRunner {
	randBytes := make([]byte, 16)
	if _, err := rand.Read(randBytes); err != nil {
		randBytes = []byte(fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	stagingDir := filepath.Join(walletHome, "signin-staging", hex.EncodeToString(randBytes))

	r := &RealRunner{
		walletHome: walletHome,
		profileDir: stagingDir,
		novncDir:   DefaultNoVNCDir,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// AssetsDir returns the staging assets directory path.
func (r *RealRunner) AssetsDir() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.assetsDir
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

// StartWebsockify starts websockify pointing to rfbPort on localhost and serving noVNC assets on bindAddr.
func (r *RealRunner) StartWebsockify(ctx context.Context, rfbPort int, bindAddr string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := CheckPrereqBinary("websockify"); err != nil {
		return 0, err
	}
	novncDir := r.novncDir
	if novncDir == "" {
		novncDir = DefaultNoVNCDir
	}
	if err := CheckNoVNCAssets(novncDir); err != nil {
		return 0, err
	}

	port, err := findFreePort(context.WithoutCancel(ctx))
	if err != nil {
		return 0, fmt.Errorf("find free port: %w", err)
	}

	stagingRoot := filepath.Dir(r.profileDir)
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		return 0, fmt.Errorf("create staging root for websockify: %w", err)
	}
	assetsDir, err := os.MkdirTemp(stagingRoot, "novnc-staging-*")
	if err != nil {
		return 0, fmt.Errorf("create novnc assets staging dir: %w", err)
	}

	// Copy noVNC static assets to staging dir
	if err := copyDir(novncDir, assetsDir); err != nil {
		_ = os.RemoveAll(assetsDir)
		return 0, fmt.Errorf("copy novnc assets: %w", err)
	}

	// Write index.html with meta refresh redirect to vnc.html with connection query parameters
	q := url.Values{}
	q.Set("host", bindAddr)
	q.Set("port", strconv.Itoa(port))
	q.Set("path", "websockify")
	q.Set("password", r.vncPwd)
	q.Set("autoconnect", "1")
	q.Set("resize", "scale")
	redirectURL := "vnc.html?" + q.Encode()

	indexContent := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
  <meta http-equiv="refresh" content="0; url=%s">
  <title>Sign-in Window</title>
</head>
<body>
  <p>Connecting to sign-in window... If not redirected, <a href="%s">click here</a>.</p>
</body>
</html>
`, redirectURL, redirectURL)

	if err := os.WriteFile(filepath.Join(assetsDir, "index.html"), []byte(indexContent), 0o644); err != nil {
		_ = os.RemoveAll(assetsDir)
		return 0, fmt.Errorf("write index.html: %w", err)
	}

	// #nosec G204 -- bindAddr and port numbers are internally controlled
	cmd := exec.CommandContext(context.WithoutCancel(ctx), "websockify",
		"--web="+assetsDir,
		fmt.Sprintf("%s:%d", bindAddr, port),
		fmt.Sprintf("127.0.0.1:%d", rfbPort),
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(assetsDir)
		return 0, fmt.Errorf("start websockify: %w", err)
	}

	r.wsCmd = cmd
	r.wsPort = port
	r.assetsDir = assetsDir

	addr := fmt.Sprintf("%s:%d", bindAddr, port)
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
			_ = os.RemoveAll(assetsDir)
			return 0, fmt.Errorf("websockify exited prematurely: %v", cmd.ProcessState)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !started {
		_ = cmd.Process.Kill()
		_ = os.RemoveAll(assetsDir)
		return 0, fmt.Errorf("timeout waiting for websockify on %s", addr)
	}

	return port, nil
}

// StopWebsockify gracefully terminates the websockify process and removes the assets staging directory.
func (r *RealRunner) StopWebsockify() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.wsCmd != nil && r.wsCmd.Process != nil {
		_ = r.wsCmd.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() {
			done <- r.wsCmd.Wait()
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = r.wsCmd.Process.Kill()
			<-done
		}
	}

	r.wsCmd = nil
	r.wsPort = 0
	if r.assetsDir != "" {
		_ = os.RemoveAll(r.assetsDir)
		r.assetsDir = ""
	}
	return nil
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			// #nosec G122 -- staging directory is a freshly created isolated temp dir
			return os.Symlink(linkTarget, target)
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
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
