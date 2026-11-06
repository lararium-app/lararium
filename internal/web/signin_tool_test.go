package web

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// passLoginGuard stubs the SSRF guard so tool tests need no network/DNS.
func passLoginGuard(t *testing.T) {
	t.Helper()
	orig := signinValidateURL
	t.Cleanup(func() { signinValidateURL = orig })
	signinValidateURL = func(_ context.Context, _ string) (*url.URL, []net.IP, error) {
		return nil, nil, nil
	}
}

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
	passLoginGuard(t)
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

	tool := SessionLoginTool(w.Home(), cfg,
		WithLoginManager(mgr),
		WithLoginWallet(w),
		WithLoginApprover(approver),
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

	// 3. The approval card itself carries the view link: the token was
	// pre-minted before the card was raised, so the link ships immediately
	// (it 404s until approval starts the window, then goes live).
	if !strings.Contains(raisedSummary, "/signin/v/") {
		t.Errorf("approval summary = %q, want the /signin/v/<token> link", raisedSummary)
	}
	if tok := mgr.Token(); tok == "" || !strings.Contains(raisedSummary, tok) {
		t.Errorf("approval summary = %q, want it to embed the pre-minted token %q", raisedSummary, mgr.Token())
	}
	// Done/Cancel on the viewer resolves the approval via this binding.
	if mgr.CardID() != "a_test_approval_001" {
		t.Errorf("CardID = %q, want the approval id bound for Done resolution", mgr.CardID())
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
	passLoginGuard(t)
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
	passLoginGuard(t)
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
	if tok := mgr.Token(); tok != "" {
		t.Errorf("pre-minted token = %q, want cleared on denial (no live link may remain)", tok)
	}
}

func TestSessionLoginTool_ForceOpensWindowEvenIfSessionPresent(t *testing.T) {
	origLookPath := execLookPath
	defer func() { execLookPath = origLookPath }()
	execLookPath = func(file string) (string, error) {
		return "/usr/bin/" + file, nil
	}

	w, mgr, _ := setupTestToolEnv(t)
	passLoginGuard(t)
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

func TestSessionLoginTool_ProviderWinsOverBuildTimeManager(t *testing.T) {
	origLookPath := execLookPath
	defer func() { execLookPath = origLookPath }()
	execLookPath = func(file string) (string, error) {
		return "/usr/bin/" + file, nil
	}

	w, mgrA, _ := setupTestToolEnv(t)
	passLoginGuard(t)
	_, mgrB, _ := setupTestToolEnv(t)
	cfg := DefaultConfig()
	cfg.Signin.NoVNCDir = t.TempDir()

	approver := func(name, argsSummary string) (bool, string) {
		return true, "a_provider_test"
	}

	tool := SessionLoginTool(w.Home(), cfg,
		WithLoginManager(mgrA), // build-time manager — must lose to the provider
		WithLoginWallet(w),
		WithLoginApprover(approver),
		WithLoginManagerProvider(func() *SigninManager { return mgrB }),
	)

	res, err := tool.Run(context.Background(), `{"origin": "https://carrier.example.com"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res, "window is open") {
		t.Errorf("result = %q, want 'window is open'", res)
	}
	if mgrB.State() != SigninLive {
		t.Errorf("provider manager state = %s, want Live", mgrB.State())
	}
	if mgrA.State() != SigninIdle {
		t.Errorf("build-time manager state = %s, want Idle (shared instance must win)", mgrA.State())
	}
}

func TestSessionLoginTool_DenylistRefusesBeforeApproval(t *testing.T) {
	origLookPath := execLookPath
	defer func() { execLookPath = origLookPath }()
	execLookPath = func(file string) (string, error) {
		return "/usr/bin/" + file, nil
	}

	origGuard := signinValidateURL
	defer func() { signinValidateURL = origGuard }()
	blockedErr := false
	signinValidateURL = func(_ context.Context, raw string) (*url.URL, []net.IP, error) {
		blockedErr = true
		return nil, nil, errors.New("refused: private address")
	}

	w, mgr, _ := setupTestToolEnv(t)
	cfg := DefaultConfig()
	cfg.Signin.NoVNCDir = t.TempDir()

	approverCalled := false
	approver := func(name, argsSummary string) (bool, string) {
		approverCalled = true
		return true, "a_should_never_raise"
	}

	tool := SessionLoginTool(w.Home(), cfg,
		WithLoginManager(mgr),
		WithLoginWallet(w),
		WithLoginApprover(approver),
	)

	res, err := tool.Run(context.Background(), `{"origin": "http://192.168.1.1/admin"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !blockedErr {
		t.Fatal("SSRF guard was not consulted")
	}
	if approverCalled {
		t.Error("approval card raised for a denylisted host; guard must run first")
	}
	if !strings.Contains(res, "refused") {
		t.Errorf("result = %q, want an honest refusal message", res)
	}
	if mgr.State() != SigninIdle {
		t.Errorf("mgr state = %s, want Idle", mgr.State())
	}
}

func TestSessionLoginTool_StaleEntryProceedsToApprovalCard(t *testing.T) {
	origLookPath := execLookPath
	defer func() { execLookPath = origLookPath }()
	execLookPath = func(file string) (string, error) {
		return "/usr/bin/" + file, nil
	}

	w, mgr, runner := setupTestToolEnv(t)
	passLoginGuard(t)
	cfg := DefaultConfig()
	novncDir := t.TempDir()
	cfg.Signin.NoVNCDir = novncDir

	origin := "https://carrier.example.com"
	stagingDir := filepath.Join(t.TempDir(), "prestored")
	_ = os.MkdirAll(stagingDir, 0o700)
	_ = os.WriteFile(filepath.Join(stagingDir, "token.txt"), []byte("pre-existing"), 0o600)
	if err := w.Store(origin, stagingDir); err != nil {
		t.Fatalf("wallet.Store: %v", err)
	}

	idx := NewWalletIndex(w.Home())
	if err := idx.MarkStale(origin); err != nil {
		t.Fatalf("MarkStale failed: %v", err)
	}

	var raisedTitle string
	approver := func(name, argsSummary string) (bool, string) {
		raisedTitle = name
		return true, "a_test_approval_002"
	}

	tool := SessionLoginTool(w.Home(), cfg,
		WithLoginManager(mgr),
		WithLoginWallet(w),
		WithLoginIndex(idx),
		WithLoginApprover(approver),
	)

	res, err := tool.Run(context.Background(), `{"origin": "https://carrier.example.com"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Stale entry proceeds to approval card flow and title says "Re-sign"
	wantTitle := "Re-sign me in to carrier.example.com"
	if raisedTitle != wantTitle {
		t.Errorf("raised card title = %q, want %q", raisedTitle, wantTitle)
	}
	if mgr.State() != SigninLive {
		t.Fatalf("manager state = %s, want Live", mgr.State())
	}
	if runner.startChromiumCalled != 1 {
		t.Fatalf("startChromiumCalled = %d, want 1", runner.startChromiumCalled)
	}
	if !strings.Contains(res, "The sign-in window is open") {
		t.Errorf("unexpected tool result: %s", res)
	}

	// Verify ReRaiseDate was stamped to today
	entry, ok := idx.Get(origin)
	if !ok {
		t.Fatalf("expected entry in index")
	}
	today := time.Now().UTC().Format("2006-01-02")
	if entry.ReRaiseDate != today {
		t.Errorf("ReRaiseDate = %q, want %q", entry.ReRaiseDate, today)
	}
}

func TestSessionLoginTool_StaleEntryRateLimitedToday(t *testing.T) {
	origLookPath := execLookPath
	defer func() { execLookPath = origLookPath }()
	execLookPath = func(file string) (string, error) {
		return "/usr/bin/" + file, nil
	}

	w, mgr, _ := setupTestToolEnv(t)
	passLoginGuard(t)
	cfg := DefaultConfig()
	cfg.Signin.NoVNCDir = t.TempDir()

	origin := "https://carrier.example.com"
	stagingDir := filepath.Join(t.TempDir(), "prestored")
	_ = os.MkdirAll(stagingDir, 0o700)
	_ = os.WriteFile(filepath.Join(stagingDir, "token.txt"), []byte("pre-existing"), 0o600)
	if err := w.Store(origin, stagingDir); err != nil {
		t.Fatalf("wallet.Store: %v", err)
	}

	today := time.Now().UTC().Format("2006-01-02")
	idx := NewWalletIndex(w.Home())
	if err := idx.MarkStale(origin); err != nil {
		t.Fatalf("MarkStale failed: %v", err)
	}
	if err := idx.StampReRaise(origin, today); err != nil {
		t.Fatalf("StampReRaise failed: %v", err)
	}

	approverCalled := false
	approver := func(name, argsSummary string) (bool, string) {
		approverCalled = true
		return true, "a_test_approval_003"
	}

	tool := SessionLoginTool(w.Home(), cfg,
		WithLoginManager(mgr),
		WithLoginWallet(w),
		WithLoginIndex(idx),
		WithLoginApprover(approver),
	)

	// ReRaiseDate == today -> refusal, no card
	res, err := tool.Run(context.Background(), `{"origin": "https://carrier.example.com"}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	wantRes := "a refresh sign-in for carrier.example.com was raised earlier today; approve it or pass force:true"
	if !strings.Contains(res, wantRes) {
		t.Errorf("result = %q, want containing %q", res, wantRes)
	}
	if approverCalled {
		t.Errorf("approver should NOT be called when rate-limited")
	}
	if mgr.State() != SigninIdle {
		t.Errorf("manager state = %s, want Idle (no window)", mgr.State())
	}
}

func TestSessionLoginTool_StaleEntryForceBypassesRateLimit(t *testing.T) {
	origLookPath := execLookPath
	defer func() { execLookPath = origLookPath }()
	execLookPath = func(file string) (string, error) {
		return "/usr/bin/" + file, nil
	}

	w, mgr, runner := setupTestToolEnv(t)
	passLoginGuard(t)
	cfg := DefaultConfig()
	cfg.Signin.NoVNCDir = t.TempDir()

	origin := "https://carrier.example.com"
	stagingDir := filepath.Join(t.TempDir(), "prestored")
	_ = os.MkdirAll(stagingDir, 0o700)
	_ = os.WriteFile(filepath.Join(stagingDir, "token.txt"), []byte("pre-existing"), 0o600)
	if err := w.Store(origin, stagingDir); err != nil {
		t.Fatalf("wallet.Store: %v", err)
	}

	today := time.Now().UTC().Format("2006-01-02")
	idx := NewWalletIndex(w.Home())
	if err := idx.MarkStale(origin); err != nil {
		t.Fatalf("MarkStale failed: %v", err)
	}
	if err := idx.StampReRaise(origin, today); err != nil {
		t.Fatalf("StampReRaise failed: %v", err)
	}

	var raisedTitle string
	approver := func(name, argsSummary string) (bool, string) {
		raisedTitle = name
		return true, "a_test_approval_004"
	}

	tool := SessionLoginTool(w.Home(), cfg,
		WithLoginManager(mgr),
		WithLoginWallet(w),
		WithLoginIndex(idx),
		WithLoginApprover(approver),
	)

	// force: true bypasses rate limit and stale check; proceeds to approval card
	res, err := tool.Run(context.Background(), `{"origin": "https://carrier.example.com", "force": true}`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	wantTitle := "Re-sign me in to carrier.example.com"
	if raisedTitle != wantTitle {
		t.Errorf("raised card title = %q, want %q", raisedTitle, wantTitle)
	}
	if mgr.State() != SigninLive {
		t.Fatalf("manager state = %s, want Live", mgr.State())
	}
	if runner.startChromiumCalled != 1 {
		t.Fatalf("startChromiumCalled = %d, want 1", runner.startChromiumCalled)
	}
	if !strings.Contains(res, "The sign-in window is open") {
		t.Errorf("unexpected tool result: %s", res)
	}
}
