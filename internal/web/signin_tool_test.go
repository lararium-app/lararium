package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupTestToolEnv(t *testing.T) (*Wallet, *SigninManager, *fakeRunner) {
	t.Helper()
	home := t.TempDir()
	w := NewWallet(home, nil)
	stagingDir := filepath.Join(t.TempDir(), "staging-tool")
	_ = os.MkdirAll(stagingDir, 0o700)
	_ = os.WriteFile(filepath.Join(stagingDir, "cookies.db"), []byte("auth=1"), 0o600)

	runner := newFakeRunner(stagingDir)
	mgr := NewSigninManager(w, runner, WithTimeout(10*time.Minute))
	return w, mgr, runner
}

func TestSessionLoginTool_BadOrigin(t *testing.T) {
	w, mgr, _ := setupTestToolEnv(t)
	cfg := DefaultConfig()
	tool := SessionLoginTool(w.Home(), cfg, WithLoginManager(mgr), WithLoginWallet(w))

	tests := []struct {
		name    string
		args    string
		wantStr string
	}{
		{
			name:    "empty origin",
			args:    `{"origin": ""}`,
			wantStr: "invalid origin",
		},
		{
			name:    "empty hostname in URL",
			args:    `{"origin": "https://"}`,
			wantStr: "invalid origin",
		},
		{
			name:    "dot-dot origin traversal attempt",
			args:    `{"origin": ".."}`,
			wantStr: "invalid origin",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tool.Run(context.Background(), tc.args)
			if err != nil {
				t.Fatalf("unexpected Run error: %v", err)
			}
			if !strings.Contains(res, tc.wantStr) {
				t.Errorf("result = %q, want containing %q", res, tc.wantStr)
			}
			// Verify NO window was opened
			if mgr.State() != SigninIdle {
				t.Errorf("manager state = %s, want Idle", mgr.State())
			}
		})
	}
}

func TestSessionLoginTool_ExistingSessionWithoutForce(t *testing.T) {
	w, mgr, _ := setupTestToolEnv(t)
	origin := "https://carrier.example.com"

	// Pre-store a session in the wallet
	stagingDir := filepath.Join(t.TempDir(), "prestored")
	_ = os.MkdirAll(stagingDir, 0o700)
	_ = os.WriteFile(filepath.Join(stagingDir, "token.txt"), []byte("pre-existing"), 0o600)
	if err := w.Store(origin, stagingDir); err != nil {
		t.Fatalf("wallet.Store: %v", err)
	}

	cfg := DefaultConfig()
	tool := SessionLoginTool(w.Home(), cfg, WithLoginManager(mgr), WithLoginWallet(w))

	// 1. Without force -> plain tool result, NO window opens
	args := `{"origin": "https://carrier.example.com"}`
	res, err := tool.Run(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res, "existing saved session is present") || !strings.Contains(res, "force: true") {
		t.Errorf("result = %q, want message mentioning existing session and force: true", res)
	}
	if mgr.State() != SigninIdle {
		t.Errorf("manager state = %s, want Idle (NO window opens)", mgr.State())
	}
}

func TestSessionLoginTool_PrereqsMissingHonestExplanation(t *testing.T) {
	origLookPath := execLookPath
	defer func() { execLookPath = origLookPath }()

	// Simulate x11vnc missing from PATH
	execLookPath = func(file string) (string, error) {
		if file == "x11vnc" {
			return "", errors.New("mock missing x11vnc")
		}
		return "/usr/bin/" + file, nil
	}

	w, mgr, _ := setupTestToolEnv(t)
	cfg := DefaultConfig()
	tool := SessionLoginTool(w.Home(), cfg, WithLoginManager(mgr), WithLoginWallet(w))

	res, err := tool.Run(context.Background(), `{"origin": "https://example.com"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := "this host cannot open a sign-in window: x11vnc not installed"
	if !strings.Contains(res, want) {
		t.Errorf("result = %q, want containing %q", res, want)
	}
	if mgr.State() != SigninIdle {
		t.Errorf("manager state = %s, want Idle", mgr.State())
	}
}

func TestSessionLoginTool_HappyPathAndCustodyCard(t *testing.T) {
	origLookPath := execLookPath
	defer func() { execLookPath = origLookPath }()
	execLookPath = func(file string) (string, error) {
		return "/usr/bin/" + file, nil
	}

	w, mgr, runner := setupTestToolEnv(t)
	cfg := DefaultConfig()
	// Point to temp dir for novnc assets so test host assets are not required
	novncDir := t.TempDir()
	cfg.Signin.NoVNCDir = novncDir

	var raisedTitle, raisedSummary string
	approver := func(name, argsSummary string) (bool, string) {
		raisedTitle = name
		raisedSummary = argsSummary
		return true, "a_test_approval_001"
	}

	var registeredCardTitle, registeredDest, registeredURL string
	registrar := func(cardTitle, cardDest, summary string) (string, error) {
		registeredCardTitle = cardTitle
		registeredDest = cardDest
		registeredURL = summary
		return "card_custos_001", nil
	}

	tool := SessionLoginTool(w.Home(), cfg,
		WithLoginManager(mgr),
		WithLoginWallet(w),
		WithLoginApprover(approver),
		WithLoginCardRegistrar(registrar),
	)

	// Call session_login
	args := `{"origin": "https://carrier.example.com/login"}`
	res, err := tool.Run(context.Background(), args)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 1. Card title must be "Sign me in to <host>" (host only — NEVER credential text)
	wantTitle := "Sign me in to carrier.example.com"
	if raisedTitle != wantTitle {
		t.Errorf("raised card title = %q, want %q", raisedTitle, wantTitle)
	}
	if !strings.Contains(raisedSummary, "Open sign-in window") {
		t.Errorf("raised card summary = %q, want containing 'Open sign-in window'", raisedSummary)
	}

	// 2. On approve: SigninManager was started and is now Live
	if mgr.State() != SigninLive {
		t.Fatalf("manager state = %s, want Live", mgr.State())
	}
	if runner.startWebsockifyCalled != 1 {
		t.Fatalf("startWebsockifyCalled = %d, want 1", runner.startWebsockifyCalled)
	}
	if runner.startChromiumCalled != 1 {
		t.Fatalf("startChromiumCalled = %d, want 1", runner.startChromiumCalled)
	}

	// 3. Card payload carries View URL
	if registeredCardTitle != wantTitle {
		t.Errorf("registered card title = %q, want %q", registeredCardTitle, wantTitle)
	}
	if registeredDest != "carrier.example.com" {
		t.Errorf("registered card dest = %q, want 'carrier.example.com'", registeredDest)
	}
	if !strings.Contains(registeredURL, "/signin/v/") {
		t.Errorf("registered card summary = %q, want containing ViewURL '/signin/v/'", registeredURL)
	}

	// 4. Tool result says window is open, model must NOT wait/poll, returns immediately
	if !strings.Contains(res, "window is open") || !strings.Contains(res, "Do not wait or poll") {
		t.Errorf("tool result = %q, want message saying window is open and do not wait/poll", res)
	}
}

func TestSessionLoginTool_EngineCtxApproverInjection(t *testing.T) {
	// Test lesson from PR #251: integration tests must exercise the engineCtx injection path
	origLookPath := execLookPath
	defer func() { execLookPath = origLookPath }()
	execLookPath = func(file string) (string, error) {
		return "/usr/bin/" + file, nil
	}

	w, mgr, _ := setupTestToolEnv(t)
	cfg := DefaultConfig()
	cfg.Signin.NoVNCDir = t.TempDir()

	approverCalled := false
	approver := func(name, argsSummary string) (bool, string) {
		approverCalled = true
		return true, "a_engine_ctx_001"
	}

	// Construct tool without WithLoginApprover
	tool := SessionLoginTool(w.Home(), cfg,
		WithLoginManager(mgr),
		WithLoginWallet(w),
	)

	// Inject approver through engineCtx via ContextWithApprover
	engineCtx := ContextWithApprover(context.Background(), approver)

	res, err := tool.Run(engineCtx, `{"origin": "https://carrier.example.com"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !approverCalled {
		t.Fatal("expected approver from engineCtx to be called, was not")
	}
	if mgr.State() != SigninLive {
		t.Fatalf("mgr state = %s, want Live", mgr.State())
	}
	if !strings.Contains(res, "window is open") {
		t.Errorf("result = %q, want containing 'window is open'", res)
	}
}

func TestSessionLoginTool_UserDeniesApproval(t *testing.T) {
	origLookPath := execLookPath
	defer func() { execLookPath = origLookPath }()
	execLookPath = func(file string) (string, error) {
		return "/usr/bin/" + file, nil
	}

	w, mgr, _ := setupTestToolEnv(t)
	cfg := DefaultConfig()
	cfg.Signin.NoVNCDir = t.TempDir()

	denyingApprover := func(name, argsSummary string) (bool, string) {
		return false, "denied:user"
	}

	tool := SessionLoginTool(w.Home(), cfg,
		WithLoginManager(mgr),
		WithLoginWallet(w),
		WithLoginApprover(denyingApprover),
	)

	res, err := tool.Run(context.Background(), `{"origin": "https://carrier.example.com"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(res, "denied") {
		t.Errorf("result = %q, want containing 'denied'", res)
	}
	if mgr.State() != SigninIdle {
		t.Fatalf("mgr state = %s, want Idle (window never opened on denial)", mgr.State())
	}
}

func TestSessionLoginTool_ForceOpensWindowEvenIfSessionPresent(t *testing.T) {
	origLookPath := execLookPath
	defer func() { execLookPath = origLookPath }()
	execLookPath = func(file string) (string, error) {
		return "/usr/bin/" + file, nil
	}

	w, mgr, _ := setupTestToolEnv(t)
	origin := "https://carrier.example.com"

	// Pre-store session in wallet
	stagingDir := filepath.Join(t.TempDir(), "prestored-force")
	_ = os.MkdirAll(stagingDir, 0o700)
	_ = os.WriteFile(filepath.Join(stagingDir, "token.txt"), []byte("pre-existing"), 0o600)
	if err := w.Store(origin, stagingDir); err != nil {
		t.Fatalf("wallet.Store: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Signin.NoVNCDir = t.TempDir()

	approver := func(name, argsSummary string) (bool, string) {
		return true, "a_force_allow"
	}

	tool := SessionLoginTool(w.Home(), cfg,
		WithLoginManager(mgr),
		WithLoginWallet(w),
		WithLoginApprover(approver),
	)

	// Call with force: true
	res, err := tool.Run(context.Background(), `{"origin": "https://carrier.example.com", "force": true}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(res, "window is open") {
		t.Errorf("result = %q, want containing 'window is open'", res)
	}
	if mgr.State() != SigninLive {
		t.Fatalf("mgr state = %s, want Live on force:true", mgr.State())
	}
}
