package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRunner struct {
	mu sync.Mutex

	profileDir string

	startXvfbErr     error
	startX11VNCErr   error
	startChromiumErr error

	startXvfbCalled     int
	stopXvfbCalled      int
	startX11VNCCalled   int
	stopX11VNCCalled    int
	startChromiumCalled int
	stopChromiumCalled  int

	teardownOrder []string

	display  string
	vncPort  int
	password string
}

func newFakeRunner(profileDir string) *fakeRunner {
	return &fakeRunner{
		profileDir: profileDir,
		display:    ":99",
		vncPort:    5900,
		password:   "ephemeral123",
	}
}

func (f *fakeRunner) StartXvfb(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startXvfbCalled++
	if f.startXvfbErr != nil {
		return "", f.startXvfbErr
	}
	return f.display, nil
}

func (f *fakeRunner) StopXvfb() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopXvfbCalled++
	f.teardownOrder = append(f.teardownOrder, "xvfb")
	return nil
}

func (f *fakeRunner) StartX11VNC(ctx context.Context, display string) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startX11VNCCalled++
	if f.startX11VNCErr != nil {
		return 0, "", f.startX11VNCErr
	}
	return f.vncPort, f.password, nil
}

func (f *fakeRunner) StopX11VNC() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopX11VNCCalled++
	f.teardownOrder = append(f.teardownOrder, "vnc")
	return nil
}

func (f *fakeRunner) StopVNC() error {
	return f.StopX11VNC()
}

func (f *fakeRunner) StartChromium(ctx context.Context, display, profileDir, targetURL string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startChromiumCalled++
	if f.startChromiumErr != nil {
		return f.startChromiumErr
	}
	return nil
}

func (f *fakeRunner) StopChromium() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopChromiumCalled++
	f.teardownOrder = append(f.teardownOrder, "chromium")
	return nil
}

func (f *fakeRunner) ProfileDir() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.profileDir
}

func (f *fakeRunner) getTeardownOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	order := make([]string, len(f.teardownOrder))
	copy(order, f.teardownOrder)
	return order
}

func TestSigninManager_HappyPath(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)
	origin := "https://carrier.example.com"

	stagingDir := filepath.Join(t.TempDir(), "staging-profile")
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		t.Fatalf("mkdir stagingDir: %v", err)
	}
	cookiePath := filepath.Join(stagingDir, "cookies.db")
	if err := os.WriteFile(cookiePath, []byte("auth_token=super_secret_cookie"), 0o600); err != nil {
		t.Fatalf("write cookie: %v", err)
	}

	runner := newFakeRunner(stagingDir)
	mgr := NewManager(w, runner)

	if mgr.State() != StateIdle {
		t.Fatalf("expected initial StateIdle, got %s", mgr.State())
	}

	info, err := mgr.Start(context.Background(), origin)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if info.Port != 5900 {
		t.Fatalf("info.Port = %d, want 5900", info.Port)
	}
	if info.Password != "ephemeral123" {
		t.Fatalf("info.Password = %q, want ephemeral123", info.Password)
	}
	if mgr.State() != StateLive {
		t.Fatalf("state after Start = %s, want StateLive", mgr.State())
	}

	// Complete seals profile and tears down
	if err := mgr.Complete(); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	if mgr.State() != StateDone {
		t.Fatalf("state after Complete = %s, want StateDone", mgr.State())
	}

	// Staging dir should be cleaned up
	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Fatalf("expected stagingDir to be removed after Complete, got: %v", err)
	}

	// Teardown must be strictly in order: chromium first (profile flush), then vnc, then xvfb
	expectedOrder := []string{"chromium", "vnc", "xvfb"}
	if !reflect.DeepEqual(runner.getTeardownOrder(), expectedOrder) {
		t.Fatalf("teardown order mismatch: got %v, want %v", runner.getTeardownOrder(), expectedOrder)
	}

	// Wallet must now have the sealed entry
	if !w.Has(origin) {
		t.Fatalf("wallet.Has(%q) = false, want true", origin)
	}

	// Round-trip verification from wallet
	activeDir, release, err := w.Load(origin)
	if err != nil {
		t.Fatalf("wallet.Load failed: %v", err)
	}
	defer release()

	data, err := os.ReadFile(filepath.Join(activeDir, "cookies.db"))
	if err != nil {
		t.Fatalf("read restored cookie: %v", err)
	}
	if string(data) != "auth_token=super_secret_cookie" {
		t.Fatalf("restored cookie mismatch: got %q", string(data))
	}
}

func TestSigninManager_TimeoutPath(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)
	origin := "https://ups.com"

	stagingDir := filepath.Join(t.TempDir(), "staging-timeout")
	_ = os.MkdirAll(stagingDir, 0o700)
	_ = os.WriteFile(filepath.Join(stagingDir, "data.txt"), []byte("temporary"), 0o600)

	runner := newFakeRunner(stagingDir)
	mgr := NewManager(w, runner, WithTimeout(30*time.Millisecond))

	_, err := mgr.Start(context.Background(), origin)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if mgr.State() != StateLive {
		t.Fatalf("expected StateLive, got %s", mgr.State())
	}

	// Wait for timeout to trigger
	deadline := time.Now().Add(500 * time.Millisecond)
	timedOut := false
	for time.Now().Before(deadline) {
		if mgr.State() == StateTimeout {
			timedOut = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !timedOut {
		t.Fatalf("timed out waiting for manager to transition to StateTimeout, current: %s", mgr.State())
	}

	// Staging dir must be removed on timeout
	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Fatalf("staging dir should be removed on timeout: %v", err)
	}

	// Wallet must not have stored anything
	if w.Has(origin) {
		t.Fatalf("wallet should not have origin after timeout")
	}

	// Teardown order
	expectedOrder := []string{"chromium", "vnc", "xvfb"}
	if !reflect.DeepEqual(runner.getTeardownOrder(), expectedOrder) {
		t.Fatalf("teardown order mismatch: got %v, want %v", runner.getTeardownOrder(), expectedOrder)
	}
}

func TestSigninManager_CancelPath(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)
	origin := "https://portal.example.com"

	stagingDir := filepath.Join(t.TempDir(), "staging-cancel")
	_ = os.MkdirAll(stagingDir, 0o700)
	_ = os.WriteFile(filepath.Join(stagingDir, "token.txt"), []byte("cancel-me"), 0o600)

	runner := newFakeRunner(stagingDir)
	mgr := NewManager(w, runner)

	_, err := mgr.Start(context.Background(), origin)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if err := mgr.Cancel(); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}

	if mgr.State() != StateCanceled {
		t.Fatalf("state after Cancel = %s, want StateCanceled", mgr.State())
	}

	// Staging dir must be removed on cancel
	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Fatalf("staging dir should be removed on cancel: %v", err)
	}

	// Wallet must not have stored anything
	if w.Has(origin) {
		t.Fatalf("wallet should not store session on cancel")
	}

	expectedOrder := []string{"chromium", "vnc", "xvfb"}
	if !reflect.DeepEqual(runner.getTeardownOrder(), expectedOrder) {
		t.Fatalf("teardown order mismatch: got %v, want %v", runner.getTeardownOrder(), expectedOrder)
	}
}

func TestSigninManager_BusyError(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)

	stagingDir := filepath.Join(t.TempDir(), "staging-busy")
	runner := newFakeRunner(stagingDir)
	mgr := NewManager(w, runner)

	_, err := mgr.Start(context.Background(), "https://first.example.com")
	if err != nil {
		t.Fatalf("first Start failed: %v", err)
	}

	// Second Start while manager is Live must return ErrBusy
	_, err2 := mgr.Start(context.Background(), "https://second.example.com")
	if err2 == nil {
		t.Fatal("expected second Start to fail with busy error, got nil")
	}
	if !errors.Is(err2, ErrBusy) {
		t.Fatalf("expected errors.Is(err, ErrBusy), got: %v", err2)
	}

	// Cancel and cleanup
	if err := mgr.Cancel(); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}
}

func TestSigninManager_IllegalTransitions(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)

	stagingDir := filepath.Join(t.TempDir(), "staging-illegal")
	runner := newFakeRunner(stagingDir)
	mgr := NewManager(w, runner)

	// 1. From Idle: Complete and Cancel are illegal
	if err := mgr.Complete(); err == nil || !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Complete from Idle want ErrIllegalTransition, got %v", err)
	}
	if err := mgr.Cancel(); err == nil || !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Cancel from Idle want ErrIllegalTransition, got %v", err)
	}

	// Start -> Live
	if _, err := mgr.Start(context.Background(), "https://test.com"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// 2. From Live: second Start is illegal
	if _, err := mgr.Start(context.Background(), "https://test.com"); err == nil || !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("second Start want ErrIllegalTransition, got %v", err)
	}

	// Complete -> Done
	if err := mgr.Complete(); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	// 3. From Done: Start, Complete, Cancel are all illegal
	if _, err := mgr.Start(context.Background(), "https://test.com"); err == nil || !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Start from Done want ErrIllegalTransition, got %v", err)
	}
	if err := mgr.Complete(); err == nil || !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Complete from Done want ErrIllegalTransition, got %v", err)
	}
	if err := mgr.Cancel(); err == nil || !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Cancel from Done want ErrIllegalTransition, got %v", err)
	}
}

func TestSigninManager_FailureTeardownRanOnce_MidStartVNC(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)

	stagingDir := filepath.Join(t.TempDir(), "staging-fail-vnc")
	runner := newFakeRunner(stagingDir)
	runner.startX11VNCErr = errors.New("cannot bind vnc port")

	mgr := NewManager(w, runner)

	_, err := mgr.Start(context.Background(), "https://failure.example.com")
	if err == nil {
		t.Fatal("expected Start to fail when x11vnc fails, got nil")
	}

	if mgr.State() != StateFailed {
		t.Fatalf("state after failure = %s, want StateFailed", mgr.State())
	}

	// Teardown must tear down what was started (Xvfb) exactly once
	if runner.startXvfbCalled != 1 {
		t.Fatalf("startXvfbCalled = %d, want 1", runner.startXvfbCalled)
	}
	if runner.stopXvfbCalled != 1 {
		t.Fatalf("stopXvfbCalled = %d, want 1 (torn down what started exactly once)", runner.stopXvfbCalled)
	}

	// Chromium was never started, so stopChromium should not have been called
	if runner.stopChromiumCalled != 0 {
		t.Fatalf("stopChromiumCalled = %d, want 0", runner.stopChromiumCalled)
	}

	// Staging dir must be removed on failure
	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Fatalf("staging dir should be removed on failure: %v", err)
	}

	// Additional calls must not re-trigger teardown
	_ = mgr.Cancel()
	if runner.stopXvfbCalled != 1 {
		t.Fatalf("stopXvfbCalled after Cancel on failed manager = %d, want 1", runner.stopXvfbCalled)
	}
}

func TestSigninManager_FailureTeardownRanOnce_MidStartChromium(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)

	stagingDir := filepath.Join(t.TempDir(), "staging-fail-chrome")
	runner := newFakeRunner(stagingDir)
	runner.startChromiumErr = errors.New("chromium failed to launch")

	mgr := NewManager(w, runner)

	_, err := mgr.Start(context.Background(), "https://failure.example.com")
	if err == nil {
		t.Fatal("expected Start to fail when chromium fails, got nil")
	}

	if mgr.State() != StateFailed {
		t.Fatalf("state after failure = %s, want StateFailed", mgr.State())
	}

	// Xvfb and VNC were started, so both must be stopped
	if runner.stopXvfbCalled != 1 {
		t.Fatalf("stopXvfbCalled = %d, want 1", runner.stopXvfbCalled)
	}
	if runner.stopX11VNCCalled != 1 {
		t.Fatalf("stopX11VNCCalled = %d, want 1", runner.stopX11VNCCalled)
	}

	// Teardown order for what was started: vnc then xvfb
	expectedOrder := []string{"vnc", "xvfb"}
	if !reflect.DeepEqual(runner.getTeardownOrder(), expectedOrder) {
		t.Fatalf("teardown order mismatch: got %v, want %v", runner.getTeardownOrder(), expectedOrder)
	}

	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Fatalf("staging dir should be removed on failure: %v", err)
	}
}

func TestPrereqs_MissingBinaryToleratesAbsence(t *testing.T) {
	// 1. Single binary check for a nonexistent binary
	nonexistent := "nonexistent-binary-lararium-12345"
	err := CheckPrereqBinary(nonexistent)
	if err == nil {
		t.Fatalf("expected error for nonexistent binary %s, got nil", nonexistent)
	}

	var missing *ErrPrereqMissing
	if !errors.As(err, &missing) {
		t.Fatalf("expected error to be *ErrPrereqMissing, got: %T (%v)", err, err)
	}
	if missing.Binary != nonexistent {
		t.Fatalf("missing.Binary = %q, want %q", missing.Binary, nonexistent)
	}
	if missing.Unwrap() == nil {
		t.Fatal("missing.Unwrap() returned nil, want underlying error")
	}

	// 2. CheckPrereqs with mocked LookPath returning error for Xvfb
	origLookPath := execLookPath
	defer func() { execLookPath = origLookPath }()

	execLookPath = func(file string) (string, error) {
		if file == "x11vnc" {
			return "", errors.New("mocked missing x11vnc")
		}
		return "/usr/bin/" + file, nil
	}

	err = CheckPrereqs()
	if err == nil {
		t.Fatal("expected CheckPrereqs to return error for missing x11vnc, got nil")
	}
	var prereqErr *ErrPrereqMissing
	if !errors.As(err, &prereqErr) {
		t.Fatalf("expected *ErrPrereqMissing, got: %T (%v)", err, err)
	}
	if prereqErr.Binary != "x11vnc" {
		t.Fatalf("prereqErr.Binary = %q, want x11vnc", prereqErr.Binary)
	}
}

func TestSigninManager_ConcurrentCallsSafe(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)

	stagingDir := filepath.Join(t.TempDir(), "staging-concurrent")
	runner := newFakeRunner(stagingDir)
	mgr := NewManager(w, runner)

	var wg sync.WaitGroup
	const routines = 15

	// Concurrently attempt Start, State, Cancel
	for i := 0; i < routines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			switch idx % 3 {
			case 0:
				_, _ = mgr.Start(context.Background(), fmt.Sprintf("https://test-%d.example.com", idx))
			case 1:
				_ = mgr.State()
			case 2:
				_ = mgr.Cancel()
			}
		}(i)
	}

	wg.Wait()
}

func TestRealRunner_Helpers(t *testing.T) {
	home := t.TempDir()
	runner := NewRealRunner(home)
	if runner == nil {
		t.Fatal("NewRealRunner returned nil")
	}
	pDir := runner.ProfileDir()
	if !strings.HasPrefix(pDir, filepath.Join(home, "signin-staging")) {
		t.Fatalf("expected profileDir under %s, got %s", filepath.Join(home, "signin-staging"), pDir)
	}

	disp, err := findFreeDisplay()
	if err != nil {
		t.Fatalf("findFreeDisplay failed: %v", err)
	}
	if !strings.HasPrefix(disp, ":") {
		t.Fatalf("expected display to start with ':', got %s", disp)
	}

	port, err := findFreePort()
	if err != nil {
		t.Fatalf("findFreePort failed: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("unexpected port: %d", port)
	}
}
