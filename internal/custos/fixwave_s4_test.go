package custos_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lararium-app/lararium/internal/custos"
	"github.com/lararium-app/lararium/internal/surface"
)

// ---------------------------------------------------------------------------
// S4-1: nil-vault doctrine branch must be reachable without panicking.
// ---------------------------------------------------------------------------

func TestS4_WorkerNilVaultNoPanic(t *testing.T) {
	cfg := &custos.Config{AskHoldTimeout: 5 * time.Second}
	cfg.Normalize()
	hub := surface.NewApprovalHub(cfg.AskHoldTimeout)
	ws := custos.NewWorkerServer(nil, hub, cfg)
	defer func() { _ = ws.Close() }()

	cw := custos.NewTestCellWorker("cell-nv", "nv-token-0123456789abcdef", "")

	// bad op → frozen bad_args frame, no panic.
	resp := ws.Dispatch(cw, &custos.WorkerRequest{Op: "nope", CellToken: "nv-token-0123456789abcdef"})
	if resp.ErrorCode != "bad_args" {
		t.Fatalf("nil-vault bad op: %+v", resp)
	}

	// wrong cell token → denied, no panic.
	resp = ws.Dispatch(cw, &custos.WorkerRequest{
		Op: "call", Connector: "gmail",
		Tool: "send", CellToken: "wrong-token-0000000000",
	})
	if resp.ErrorCode != "denied" {
		t.Fatalf("nil-vault wrong token: %+v", resp)
	}

	// valid-looking request → internal (nil vault is a wiring fault), no
	// panic: the doctrine branch after the identity gates stays reachable.
	resp = ws.Dispatch(cw, &custos.WorkerRequest{
		Op: "call", Connector: "gmail",
		Tool: "send", CellToken: "nv-token-0123456789abcdef",
		Args: json.RawMessage(`{"to":["x@example.com"],"subject":"s","body":"b"}`),
	})
	if resp.ErrorCode != "internal" {
		t.Fatalf("nil-vault valid-looking request: %+v", resp)
	}
}

// ---------------------------------------------------------------------------
// S4-2: refresh HTTP failures contribute their status class (§5.1a), not
// 'internal'. Drives the fake's refreshFail seam through a real worker send.
// ---------------------------------------------------------------------------

func s4StoreExpiringGmail(t *testing.T, h *workerHarness, tokenURI string) {
	t.Helper()
	cred := custos.Credential{ //nolint:gosec // G101: fake test fixture values, not real credentials
		Kind: "oauth2", ClientID: "cid", ClientSecret: v13ClientSecret,
		RefreshToken: v13RefreshCanary, AccessToken: "at-stale-token-aaaa",
		// Expired: worker send must refresh through the fake.
		AccessExpiry: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		TokenURI:     tokenURI,
	}
	if err := h.v.StoreOAuthGrant("", "gmail", cred, "cli"); err != nil {
		t.Fatalf("store expiring gmail grant: %v", err)
	}
}

func TestS4_RefreshFailureStatusClass(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		detail string
	}{
		{429, "rate_limited", "upstream rate limited"},
		{500, "upstream", "upstream returned 5xx"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			fake := &fakeOAuth{refreshFail: tc.status}
			h := setupWorkerHarness(t, fake)
			if _, err := h.v.Policy().AddCredentialRule("gmail/send", "auto", false, "cli", "cli"); err != nil {
				t.Fatalf("policy add: %v", err)
			}
			s4StoreExpiringGmail(t, h, h.fakeURL+"/token-refresh")
			resp := h.send(t, sendReq(t, v13CellToken,
				map[string]interface{}{"to": []string{"x@example.com"}, "subject": "s", "body": "b"}))
			if resp.ErrorCode != tc.code {
				t.Fatalf("refresh %d: want %s, got %+v", tc.status, tc.code, resp)
			}
			if resp.Detail != tc.detail {
				t.Fatalf("refresh %d: detail = %q, want %q", tc.status, resp.Detail, tc.detail)
			}
			// §5.1a: never the URL, never the upstream body.
			if strings.Contains(resp.Detail, "http") ||
				strings.Contains(resp.Detail, "leaky") ||
				strings.Contains(resp.Detail, "secret.example.com") {
				t.Fatalf("refresh %d: detail leaks URL/body: %q", tc.status, resp.Detail)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// S4-3: an oversized NDJSON request line gets the frozen bad_args frame and
// the connection closes; the server never buffers past the cap.
// ---------------------------------------------------------------------------

func TestS4_OversizedLineRefused(t *testing.T) {
	fake := &fakeOAuth{}
	h := setupWorkerHarness(t, fake)
	h.storeGmail(t)

	conn, err := net.Dial("unix", h.sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// One line over the 1 MiB cap, no newline yet. Written in a goroutine:
	// the server stops reading after the cap, so a blocking write must not
	// deadlock the test before the response is checked.
	go func() {
		chunk := []byte(strings.Repeat("A", 64<<10))
		for range (1 << 20 / (64 << 10)) + 2 {
			if _, werr := conn.Write(chunk); werr != nil {
				return // server closed: expected once the cap trips
			}
		}
	}()

	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, rerr := bufio.NewReader(conn).ReadBytes('\n')
	if rerr != nil {
		t.Fatalf("expected bad_args frame for oversized line, read err: %v", rerr)
	}
	var resp custos.WorkerResponse
	if jerr := json.Unmarshal(line, &resp); jerr != nil {
		t.Fatalf("oversized-line response not JSON: %q", line)
	}
	if resp.ErrorCode != "bad_args" {
		t.Fatalf("oversized line: %+v", resp)
	}

	// Frame delivered, connection is closed behind it: EOF from now on.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if extra, xerr := bufio.NewReader(conn).ReadBytes('\n'); xerr == nil {
		t.Fatalf("expected close after oversized frame, got %q", extra)
	}
}

// ---------------------------------------------------------------------------
// S4-4: CA-1(e) name grammar + 64-name vault-side cap on StoreOAuthGrant,
// and the ctl LOGIN-STORE path (which goes through StoreOAuthGrant).
// ---------------------------------------------------------------------------

func TestS4_StoreOAuthGrantNameGrammar(t *testing.T) {
	v, _, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	bad := []struct {
		name  string
		label string
	}{
		{"Gmail", "uppercase"},
		{"", "empty"},
		{strings.Repeat("a", 65), "too long"},
		{"-leading-dash", "leading dash"},
		{"has space", "space"},
	}
	for _, b := range bad {
		t.Run(b.label, func(t *testing.T) {
			err := v.StoreOAuthGrant("", b.name, custos.Credential{Kind: "oauth2", RefreshToken: "r"}, "cli")
			if !errors.Is(err, custos.ErrBadName) {
				t.Fatalf("store name %q: want ErrBadName, got %v", b.label, err)
			}
		})
	}
	// 64-char lowercase name is grammar-legal (upper bound of the regex).
	if err := v.StoreOAuthGrant("", strings.Repeat("a", 64), custos.Credential{Kind: "oauth2", RefreshToken: "r"}, "cli"); err != nil {
		t.Fatalf("64-char name must be accepted: %v", err)
	}
}

func TestS4_StoreOAuthGrantNameCap(t *testing.T) {
	v, _, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	for i := range 64 {
		name := "c" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		if err := v.StoreOAuthGrant("", name, custos.Credential{Kind: "oauth2", RefreshToken: "r"}, "cli"); err != nil {
			t.Fatalf("store %d (%s): %v", i, name, err)
		}
	}
	// 65th distinct name refused with the cap error.
	err := v.StoreOAuthGrant("", "zz9", custos.Credential{Kind: "oauth2", RefreshToken: "r"}, "cli")
	if !errors.Is(err, custos.ErrFull) {
		t.Fatalf("65th name: want ErrFull, got %v", err)
	}
	// Overwriting an existing name never trips the cap.
	if err := v.StoreOAuthGrant("", "caa", custos.Credential{Kind: "oauth2", RefreshToken: "r2"}, "cli"); err != nil {
		t.Fatalf("overwrite at cap must succeed: %v", err)
	}
}

func TestS4_CtlLoginStoreNameGrammar(t *testing.T) {
	v, stateDir, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	tok, terr := custos.EnsureCtlToken(stateDir)
	if terr != nil {
		t.Fatalf("ensure ctl token: %v", terr)
	}
	cs, err := custos.StartCtlServer(stateDir, v, nil)
	if err != nil {
		t.Fatalf("start ctl: %v", err)
	}
	defer func() { _ = cs.Close() }()

	sendCtl := func(t *testing.T, payload string) string {
		t.Helper()
		sockPath, _ := custos.CtlPaths(stateDir)
		conn, derr := net.Dial("unix", sockPath)
		if derr != nil {
			t.Fatalf("dial ctl.sock: %v", derr)
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("LOGIN-STORE " + tok + " " + payload + "\n"))
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, rerr := bufio.NewReader(conn).ReadString('\n')
		if rerr != nil {
			t.Fatalf("read ctl response: %v", rerr)
		}
		return strings.TrimSpace(line)
	}

	if got := sendCtl(t, `{"name":"Bad Name","cred":{"kind":"oauth2","refresh_token":"r"}}`); got != "ERR bad_name" {
		t.Fatalf("uppercase name: got %q, want ERR bad_name", got)
	}
	if got := sendCtl(t, `{"name":"gmail","cred":{"kind":"oauth2","refresh_token":"r"}}`); got != "OK credential_added" {
		t.Fatalf("valid first store: got %q", got)
	}
	// Overwrite of the same name is a rotation: distinct OK line + audit.
	if got := sendCtl(t, `{"name":"gmail","cred":{"kind":"oauth2","refresh_token":"r2"}}`); got != "OK credential_rotated" {
		t.Fatalf("overwrite store: got %q, want OK credential_rotated", got)
	}
	recs, aerr := v.Audit().ReadTailRecords(12)
	if aerr != nil {
		t.Fatalf("audit: %v", aerr)
	}
	rotated := false
	for _, r := range recs {
		if r.Kind == custos.AuditKindCredentialRotated && r.Cred == "gmail" {
			rotated = true
		}
	}
	if !rotated {
		t.Fatalf("credential_rotated audit missing for LOGIN-STORE overwrite")
	}
}

// ---------------------------------------------------------------------------
// S4-5: connection deadline arithmetic (hold + exec + margin) and the
// WorkerExecTimeout budget honored by Execute.
// ---------------------------------------------------------------------------

func TestS4_ConnDeadlineArithmetic(t *testing.T) {
	cfg := &custos.Config{AskHoldTimeout: 330 * time.Second}
	cfg.Normalize()
	if cfg.WorkerExecTimeout != custos.DefaultWorkerExecTimeout {
		t.Fatalf("default exec timeout = %v, want %v", cfg.WorkerExecTimeout, custos.DefaultWorkerExecTimeout)
	}
	hub := surface.NewApprovalHub(cfg.AskHoldTimeout)
	ws := custos.NewWorkerServer(nil, hub, cfg)
	defer func() { _ = ws.Close() }()

	if got, want := ws.ConnDeadlineForTest(), 330*time.Second+60*time.Second+30*time.Second; got != want {
		t.Fatalf("conn deadline = %v, want %v", got, want)
	}

	// An explicit exec budget is respected by the arithmetic too.
	cfg2 := &custos.Config{AskHoldTimeout: 3 * time.Second, WorkerExecTimeout: 2 * time.Second}
	cfg2.Normalize()
	ws2 := custos.NewWorkerServer(nil, surface.NewApprovalHub(cfg2.AskHoldTimeout), cfg2)
	defer func() { _ = ws2.Close() }()
	if got, want := ws2.ConnDeadlineForTest(), 5*time.Second+30*time.Second; got != want {
		t.Fatalf("explicit conn deadline = %v, want %v", got, want)
	}
}

type slowConnector struct{}

func (slowConnector) Name() string             { return "slow" }
func (slowConnector) Credential() string       { return "slow" }
func (slowConnector) Host() string             { return "slow.test" }
func (slowConnector) HasTool(tool string) bool { return tool == "nap" }
func (slowConnector) ValidateArgs(tool string, args json.RawMessage) error {
	if tool != "nap" {
		return errors.New("no such tool")
	}
	return nil
}
func (slowConnector) ReviewFields(tool string, args json.RawMessage) string { return "slow:nap" }
func (slowConnector) Execute(ctx context.Context, tool string, args json.RawMessage, cred custos.Credential) (json.RawMessage, error) {
	select {
	case <-time.After(30 * time.Second):
		return json.RawMessage(`{"slept":true}`), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestS4_ExecuteTimeoutBudget(t *testing.T) {
	fake := &fakeOAuth{}
	h := setupWorkerHarness(t, fake)
	// Explicit tiny execution budget (harness Normalize left the default).
	h.ws.SetExecTimeoutForTest(200 * time.Millisecond)
	if err := h.ws.RegisterConnector(slowConnector{}); err != nil {
		t.Fatalf("register slow: %v", err)
	}
	// Credential named "slow" so the connector's execution-time
	// revalidation finds it (kind is irrelevant to the stub).
	if err := h.v.StoreOAuthGrant("", "slow", custos.Credential{Kind: "oauth2", RefreshToken: "r"}, "cli"); err != nil {
		t.Fatalf("store slow cred: %v", err)
	}
	if _, err := h.v.Policy().AddCredentialRule("slow/nap", "auto", false, "cli", "cli"); err != nil {
		t.Fatalf("policy add: %v", err)
	}

	start := time.Now()
	resp := h.send(t, custos.WorkerRequest{Op: "call", Connector: "slow", Tool: "nap", CellToken: v13CellToken})
	elapsed := time.Since(start)
	if resp.ErrorCode != "timeout" {
		t.Fatalf("exec over budget: %+v", resp)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("exec timeout not honored: waited %v", elapsed)
	}
}

// ---------------------------------------------------------------------------
// S4-6: §4.5 manual paste-back — state validated for the URL form; a bare
// code carries no state (documented limit).
// ---------------------------------------------------------------------------

type devNullWriter struct{}

func (devNullWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestS4_ManualPastebackStateValidation(t *testing.T) {
	fake := &fakeOAuth{}
	h := setupWorkerHarness(t, fake)

	pasteLogin := func(pasted, state string) error {
		_, err := custos.OAuthLogin(context.Background(), &custos.LoginOptions{
			Connector:    "gmail",
			ClientID:     "cid",
			ClientSecret: v13ClientSecret,
			Manual:       true,
			State:        state,
			AuthURI:      h.fakeURL + "/auth",
			TokenURI:     h.fakeURL + "/token",
			Out:          devNullWriter{},
			PasteBack:    func() (string, error) { return pasted, nil },
		})
		return err
	}

	// URL form with the WRONG state → refused before any exchange.
	err := pasteLogin("http://127.0.0.1:1/?code=code-wrong-state&state=not-the-state", "real-state-xyz")
	if !errors.Is(err, custos.ErrLoginStateMismatch) {
		t.Fatalf("wrong-state URL paste-back: want ErrLoginStateMismatch, got %v", err)
	}
	// URL form with the CORRECT state → accepted (exchange proceeds).
	if err := pasteLogin("http://127.0.0.1:1/?code=code-good-state&state=real-state-xyz", "real-state-xyz"); err != nil {
		t.Fatalf("correct-state URL paste-back: %v", err)
	}
}

// ---------------------------------------------------------------------------
// S4-8: previously vacuous coverage — credential-gone and locked-gate worker
// denials actually exercised, with the locked-before-credential order.
// ---------------------------------------------------------------------------

func TestS4_WorkerCredentialGoneDenied(t *testing.T) {
	fake := &fakeOAuth{}
	h := setupWorkerHarness(t, fake)
	h.storeGmail(t)
	// Auto policy so the request reaches execution-time revalidation.
	if _, err := h.v.Policy().AddCredentialRule("gmail/send", "auto", false, "cli", "cli"); err != nil {
		t.Fatalf("policy add: %v", err)
	}
	// Credential gone before the send.
	if err := h.v.RevokeCredential("", "gmail", "cli"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	resp := h.send(t, sendReq(t, v13CellToken,
		map[string]interface{}{"to": []string{"x@example.com"}, "subject": "s", "body": "b"}))
	if resp.ErrorCode != "denied" {
		t.Fatalf("credential gone: %+v", resp)
	}
	// Frozen detail template for the credential-unavailable denial.
	if !strings.Contains(resp.Detail, "credential unavailable") {
		t.Fatalf("credential-gone detail shape: %q", resp.Detail)
	}
	recs, err := h.v.Audit().ReadTailRecords(12)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	gone := false
	for _, r := range recs {
		if r.Kind == custos.AuditKindWorkerCallDenied && r.Reason == "credential_gone" {
			gone = true
		}
	}
	if !gone {
		t.Fatalf("worker_call_denied reason credential_gone missing")
	}
	fake.mu.Lock()
	seen := fake.lastAuth
	fake.mu.Unlock()
	if seen != "" {
		t.Fatalf("upstream called with credential gone")
	}
}

func TestS4_WorkerLockedGate(t *testing.T) {
	fake := &fakeOAuth{}
	h := setupWorkerHarness(t, fake)
	h.storeGmail(t)
	if err := h.v.Lock(); err != nil {
		t.Fatalf("lock vault: %v", err)
	}

	resp := h.send(t, sendReq(t, v13CellToken,
		map[string]interface{}{"to": []string{"x@example.com"}, "subject": "s", "body": "b"}))
	if resp.ErrorCode != "locked" {
		t.Fatalf("locked vault: %+v", resp)
	}
	recs, err := h.v.Audit().ReadTailRecords(12)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	locked := false
	for _, r := range recs {
		if r.Kind == custos.AuditKindWorkerCallDenied && r.Reason == "locked" {
			locked = true
		}
	}
	if !locked {
		t.Fatalf("worker_call_denied reason locked missing")
	}
}

func TestS4_WorkerLockedBeforeCredentialGone(t *testing.T) {
	// Locked vault + credential that never existed: the locked gate must
	// win (spec order: locked → unknown_connector → ... → credential).
	fake := &fakeOAuth{}
	h := setupWorkerHarness(t, fake)
	// Deliberately never store the gmail credential.
	if err := h.v.Lock(); err != nil {
		t.Fatalf("lock vault: %v", err)
	}
	resp := h.send(t, sendReq(t, v13CellToken,
		map[string]interface{}{"to": []string{"x@example.com"}, "subject": "s", "body": "b"}))
	if resp.ErrorCode != "locked" {
		t.Fatalf("locked+missing credential: want locked, got %+v", resp)
	}
}

// ---------------------------------------------------------------------------
// S4-9: single-flight refresh must not die with the first caller's context.
// ---------------------------------------------------------------------------

func TestS4_RefreshSurvivesFirstCallerCancel(t *testing.T) {
	v, _, pass := setupTestVault(t)
	if err := v.Init(pass); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := v.Unlock(pass, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	release := make(chan struct{})
	v.SetTokenForward(func(ctx context.Context, tokenURI string, form url.Values) (*custos.TokenExchangeResult, error) {
		<-release
		return &custos.TokenExchangeResult{AccessToken: "at-coalesced-1", ExpiresIn: 3600}, nil //nolint:gosec // G101: fake fixture value, not a real credential
	})

	cred := custos.Credential{
		Kind: "oauth2", ClientID: "cid", ClientSecret: v13ClientSecret,
		RefreshToken: "refresh-canary-s4", AccessToken: "at-stale-aaaa",
		AccessExpiry: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
	}
	if err := v.StoreOAuthGrant("", "gmail", cred, "cli"); err != nil {
		t.Fatalf("store: %v", err)
	}

	// Caller A registers the in-flight refresh, then cancels immediately.
	ctxA, cancelA := context.WithCancel(context.Background())
	aCh := make(chan error, 1)
	go func() {
		_, err := v.AcquireTokenCtx(ctxA, "gmail")
		aCh <- err
	}()
	time.Sleep(150 * time.Millisecond) // let A register the single-flight
	cancelA()

	// Caller B coalesces onto the SAME refresh while it blocks.
	type rb struct {
		tok string
		err error
	}
	bCh := make(chan rb, 1)
	go func() {
		tok, err := v.AcquireTokenCtx(context.Background(), "gmail")
		bCh <- rb{tok, err}
	}()
	time.Sleep(150 * time.Millisecond) // let B coalesce

	close(release) // the shared forward may finish

	// The shared forward ran detached from A's context: B must get the
	// token regardless of what A observed.
	select {
	case got := <-bCh:
		if got.err != nil {
			t.Fatalf("coalesced caller failed after first-caller cancel: %v", got.err)
		}
		if got.tok != "at-coalesced-1" {
			t.Fatalf("coalesced caller token = %q", got.tok)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("coalesced caller never returned")
	}
	<-aCh
}
