package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func skipWithoutChrome(t *testing.T) string {
	t.Helper()
	// Browser tests are opt-in via LARARIUM_CHROME (house convention):
	// CI runners ship a Chrome whose auto-detection would turn every
	// shared runner into a browser farm — the headless instances starve
	// timing-sensitive suites (V7) that run alongside this package.
	// Dev boxes set the variable explicitly; nothing else changes.
	chrome := strings.TrimSpace(os.Getenv("LARARIUM_CHROME"))
	if chrome == "" {
		t.Skip("skipping browser-dependent test; LARARIUM_CHROME not set")
	}
	return chrome
}

func TestWebBrowse_ArgValidation(t *testing.T) {
	cfg := DefaultConfig()
	tool := BrowseTool(t.TempDir(), cfg)

	tests := []struct {
		name     string
		argsJSON string
		wantErr  string
	}{
		{
			name:     "invalid JSON",
			argsJSON: `{"url":`,
			wantErr:  "bad args:",
		},
		{
			name:     "missing url",
			argsJSON: `{"goal":"find docs"}`,
			wantErr:  "url is required",
		},
		{
			name:     "empty url",
			argsJSON: `{"url":"   "}`,
			wantErr:  "url is required",
		},
		{
			name:     "unsupported scheme",
			argsJSON: `{"url":"ftp://example.com/test"}`,
			wantErr:  "unsupported URL scheme",
		},
		{
			name:     "ssrf loopback blocked",
			argsJSON: `{"url":"http://127.0.0.1/test"}`,
			wantErr:  "ssrf: blocked destination IP",
		},
		{
			name:     "ssrf localhost blocked",
			argsJSON: `{"url":"http://localhost:8080/test"}`,
			wantErr:  "ssrf:",
		},
		{
			name:     "ssrf cloud metadata blocked",
			argsJSON: `{"url":"http://169.254.169.254/latest"}`,
			wantErr:  "ssrf:",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Run(context.Background(), tc.argsJSON)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestWebBrowse_BusyMutex(t *testing.T) {
	cfg := DefaultConfig()
	dataDir := t.TempDir()
	browser := NewBrowser(dataDir, cfg)

	// Hold mutex to simulate concurrent browse in flight
	browser.mu.Lock()

	res, err := browser.Browse(context.Background(), "https://example.com", "goal")
	browser.mu.Unlock()

	if err != nil {
		t.Fatalf("expected nil error on busy, got: %v", err)
	}
	if res != ErrBrowserBusy {
		t.Fatalf("res = %q, want %q", res, ErrBrowserBusy)
	}
}

func TestWebBrowse_UnavailableWhenNoBrowserInstalled(t *testing.T) {
	// Set explicit nonexistent chrome path
	cfg := Config{
		Enabled:        true,
		BrowseTimeoutS: 30,
		MaxTextBytes:   DefaultMaxTextBytes,
		ChromePath:     "/nonexistent/browser/binary",
	}

	browser := NewBrowser(t.TempDir(), cfg)
	res, err := browser.Browse(context.Background(), "https://example.com", "inspect")
	if err != nil {
		t.Fatalf("expected nil error on unavailable browser, got: %v", err)
	}
	if res != ErrBrowserUnavailable {
		t.Fatalf("res = %q, want %q", res, ErrBrowserUnavailable)
	}
}

func TestWebBrowse_RealBrowser(t *testing.T) {
	chrome := skipWithoutChrome(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!DOCTYPE html>
<html>
<head><title>Browser Dynamic Test</title></head>
<body>
<h1>Dynamic Header</h1>
<div id="content">Rendered content</div>
<a href="/sub">Sublink</a>
</body>
</html>`))
	}))
	defer ts.Close()

	cfg := Config{
		Enabled:        true,
		BrowseTimeoutS: 15,
		MaxTextBytes:   50000,
		ChromePath:     chrome,
	}

	dataDir := t.TempDir()
	browser := NewBrowser(dataDir, cfg, WithAllowedTestHosts(ts.URL))

	res, err := browser.Browse(context.Background(), ts.URL, "read page")
	if err != nil {
		t.Fatalf("real browser failed: %v", err)
	}
	if !strings.Contains(res, "Browser Dynamic Test") {
		t.Errorf("missing title in browser output: %s", res)
	}
	if !strings.Contains(res, "Rendered content") {
		t.Errorf("missing rendered text in browser output: %s", res)
	}
}

func TestPolicy_AlwaysAskCards(t *testing.T) {
	home := t.TempDir()
	var approverCalled bool
	var approverTool, approverSummary string

	approver := func(name, summary string) (bool, string) {
		approverCalled = true
		approverTool = name
		approverSummary = summary
		return true, "approved:card1"
	}

	cfg := DefaultConfig()
	tool := BrowseTool(home, cfg, WithApprover(approver))

	// By default, policy is always_ask
	argsJSON := `{
		"url": "https://example.com/checkout",
		"goal": "place order",
		"acts": [
			{"kind": "click", "selector": "#confirm-order", "intent": "click confirm order button"}
		]
	}`

	_, _ = tool.Run(context.Background(), argsJSON)
	if !approverCalled {
		t.Fatal("expected approval card to be invoked for acts on always_ask policy")
	}
	if approverTool != "web_browse" {
		t.Errorf("approver tool = %q, want web_browse", approverTool)
	}
	if !strings.Contains(approverSummary, "click confirm order button") {
		t.Errorf("approver summary = %q, want it to contain act intent", approverSummary)
	}
}

func TestPolicy_DailyAllowOncePerDay(t *testing.T) {
	home := t.TempDir()
	index := NewWalletIndex(home)
	today := time.Now().UTC().Format("2006-01-02")

	// Set daily_allow policy
	err := index.Set(WalletEntry{
		Origin: "example.com",
		Policy: PolicyDailyAllow,
	})
	if err != nil {
		t.Fatalf("set index entry: %v", err)
	}

	approverCalls := 0
	approver := func(name, summary string) (bool, string) {
		approverCalls++
		return true, "approved:test"
	}

	cfg := DefaultConfig()
	tool := BrowseTool(home, cfg, WithApprover(approver))

	argsJSON := `{
		"url": "https://example.com/task",
		"goal": "daily check",
		"acts": [
			{"kind": "click", "selector": "#refresh", "intent": "refresh dashboard"}
		]
	}`

	// Spec §10.1: first act of the day asks (one card, approved here),
	// stamps the day, and every later act the same day runs free.
	_, _ = tool.Run(context.Background(), argsJSON)
	if approverCalls != 1 {
		t.Fatalf("first call of daily_allow MUST request approval card once; calls = %d", approverCalls)
	}

	entry, ok := index.Get("example.com")
	if !ok || entry.DailyAllowDate != today {
		t.Fatalf("expected daily_allow_date to be stamped with %s, got: %+v", today, entry)
	}

	// Call 2 (same day): already stamped -> free, no new card.
	_, _ = tool.Run(context.Background(), argsJSON)
	if approverCalls != 1 {
		t.Fatalf("second call on same day must NOT request a new card; calls = %d", approverCalls)
	}
}

func TestPolicy_DenySkipsAct(t *testing.T) {
	home := t.TempDir()
	approver := func(name, summary string) (bool, string) {
		return false, "denied"
	}

	cfg := DefaultConfig()
	tool := BrowseTool(home, cfg, WithApprover(approver))

	argsJSON := `{
		"url": "https://example.com/delete",
		"goal": "delete item",
		"acts": [
			{"kind": "click", "selector": "#delete-btn", "intent": "delete record permanently"}
		]
	}`

	obs, err := tool.Run(context.Background(), argsJSON)
	if err != nil {
		t.Fatalf("expected nil error on denied tool call, got: %v", err)
	}
	if !strings.Contains(obs, "Blocked by the approval policy: the user denied tool web_browse on its approval card.") {
		t.Fatalf("expected frozen denial guidance text, got: %q", obs)
	}

	// Verify activity record is logged as denied
	records, err := QueryActivity(home, 10, "example.com")
	if err != nil {
		t.Fatalf("query activity: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("expected activity record for denied act")
	}
	if records[0].Verdict != VerdictDenied {
		t.Fatalf("verdict = %q, want denied", records[0].Verdict)
	}
}

func TestActs_IntentRequired(t *testing.T) {
	cfg := DefaultConfig()
	tool := BrowseTool(t.TempDir(), cfg)

	tests := []struct {
		name     string
		argsJSON string
		wantErr  string
	}{
		{
			name: "missing intent",
			argsJSON: `{
				"url": "https://example.com",
				"acts": [{"kind": "click", "selector": "#btn"}]
			}`,
			wantErr: "act intent is required",
		},
		{
			name: "empty intent",
			argsJSON: `{
				"url": "https://example.com",
				"acts": [{"kind": "click", "selector": "#btn", "intent": "   "}]
			}`,
			wantErr: "act intent is required",
		},
		{
			name: "multiline intent",
			argsJSON: `{
				"url": "https://example.com",
				"acts": [{"kind": "click", "selector": "#btn", "intent": "line1\nline2"}]
			}`,
			wantErr: "act intent must be a single line",
		},
		{
			name: "missing selector",
			argsJSON: `{
				"url": "https://example.com",
				"acts": [{"kind": "click", "selector": "", "intent": "click button"}]
			}`,
			wantErr: "act selector is required",
		},
		{
			name: "unsupported kind",
			argsJSON: `{
				"url": "https://example.com",
				"acts": [{"kind": "hover", "selector": "#btn", "intent": "hover over button"}]
			}`,
			wantErr: "unsupported act kind",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Run(context.Background(), tc.argsJSON)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestActs_PasswordFieldRefused(t *testing.T) {
	chrome := skipWithoutChrome(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!DOCTYPE html>
<html>
<head><title>Password Form</title></head>
<body>
<form>
  <input type="text" id="username" value="user1">
  <input type="password" id="secret_pwd">
</form>
</body>
</html>`))
	}))
	defer ts.Close()

	home := t.TempDir()
	approver := func(name, summary string) (bool, string) {
		return true, "approved"
	}

	cfg := Config{
		Enabled:        true,
		BrowseTimeoutS: 15,
		MaxTextBytes:   DefaultMaxTextBytes,
		ChromePath:     chrome,
	}

	browser := NewBrowser(home, cfg, WithAllowedTestHosts(ts.URL), WithApprover(approver))

	act := Act{
		Kind:     "fill",
		Selector: "#secret_pwd",
		Text:     "mysecretpassword",
		Intent:   "fill password field",
	}

	obs, err := browser.Browse(context.Background(), ts.URL, "fill login", act)
	if err != nil {
		t.Fatalf("browse failed: %v", err)
	}

	if !strings.Contains(obs, "refused: fill act on password field #secret_pwd is blocked") {
		t.Fatalf("expected password refusal explanation, got: %s", obs)
	}
	if !strings.Contains(obs, "credentials are never handled by the agent") {
		t.Fatalf("expected credentials law note in observation, got: %s", obs)
	}

	// Verify activity record recorded blocked verdict
	u, _ := url.Parse(ts.URL)
	records, err := QueryActivity(home, 10, u.Hostname())
	if err != nil {
		t.Fatalf("query activity: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("expected activity record")
	}
	if records[0].Verdict != VerdictBlocked {
		t.Fatalf("expected blocked verdict, got: %q", records[0].Verdict)
	}
}

func TestBrowse_SessionLoadedForOrigin(t *testing.T) {
	chrome := skipWithoutChrome(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		cookie, _ := r.Cookie("session_user")
		user := "anonymous"
		if cookie != nil {
			user = cookie.Value
		}
		//nolint:gosec // G705: test fixture HTML
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Session Page</title></head>
<body>
<div id="greeting">Hello %s</div>
</body>
</html>`, user)
	}))
	defer ts.Close()

	home := t.TempDir()
	u, _ := url.Parse(ts.URL)
	origin := u.Hostname()

	w := NewWallet(home, nil)
	// Seed a session file for this origin
	activeDir, release, err := w.Load(origin)
	if err != nil {
		t.Fatalf("seed Load failed: %v", err)
	}
	_ = os.WriteFile(filepath.Join(activeDir, "dummy_profile_marker"), []byte("active"), 0o600)
	if err := release(); err != nil {
		t.Fatalf("seed release failed: %v", err)
	}

	cfg := Config{
		Enabled:        true,
		BrowseTimeoutS: 15,
		MaxTextBytes:   DefaultMaxTextBytes,
		ChromePath:     chrome,
	}

	browser := NewBrowser(home, cfg, WithAllowedTestHosts(ts.URL), WithWallet(w))

	// Browse origin: session is loaded and released
	res, err := browser.Browse(context.Background(), ts.URL, "view session page")
	if err != nil {
		t.Fatalf("browse failed: %v", err)
	}

	if !strings.Contains(res, "Hello anonymous") {
		t.Errorf("unexpected output: %s", res)
	}

	// Verify session was released properly (active dir removed, lrws updated)
	originKey, _ := OriginKey(origin)
	activeDirCheck := filepath.Join(home, "sessions", ".active", originKey)
	if _, err := os.Stat(activeDirCheck); !os.IsNotExist(err) {
		t.Fatalf("active dir should be removed on release, got: %v", err)
	}
}

func TestBrowse_OffOriginRefusedWithSession(t *testing.T) {
	chrome := skipWithoutChrome(t)

	// ts is listening on 127.0.0.1:<port>
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Offer link to localhost (different origin by host aliasing)
		parts := strings.Split(r.Host, ":")
		port := parts[len(parts)-1]
		localhostURL := fmt.Sprintf("http://localhost:%s/secret", port)
		//nolint:gosec // G705: test fixture HTML
		fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head><title>Origin A</title></head>
<body>
<a id="cross-origin" href="%s">Cross Link</a>
</body>
</html>`, localhostURL)
	}))
	defer ts.Close()

	home := t.TempDir()
	u, _ := url.Parse(ts.URL)
	origin := u.Hostname() // 127.0.0.1

	wallet := NewWallet(home, nil)
	// Create entry for 127.0.0.1
	dir, rel, err := wallet.Load(origin)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "marker"), []byte("ok"), 0o600)
	_ = rel()

	cfg := Config{
		Enabled:        true,
		BrowseTimeoutS: 15,
		MaxTextBytes:   DefaultMaxTextBytes,
		ChromePath:     chrome,
	}

	approver := func(name, summary string) (bool, string) {
		return true, "approved"
	}

	browser := NewBrowser(home, cfg,
		WithAllowedTestHosts(ts.URL, "localhost"),
		WithWallet(wallet),
		WithApprover(approver),
	)

	// Act clicking off-origin link
	act := Act{
		Kind:     "click",
		Selector: "#cross-origin",
		Intent:   "click cross origin link",
	}

	obs, err := browser.Browse(context.Background(), ts.URL, "click off origin", act)
	if err != nil {
		t.Fatalf("browse failed: %v", err)
	}

	if !strings.Contains(obs, "refused: off-origin navigation to") {
		t.Fatalf("expected off-origin refusal observation, got: %s", obs)
	}
	if !strings.Contains(obs, "session is locked to 127.0.0.1") {
		t.Fatalf("expected origin lock in refusal text, got: %s", obs)
	}
}

func TestBrowse_LoginWallMarker(t *testing.T) {
	chrome := skipWithoutChrome(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!DOCTYPE html>
<html>
<head><title>Sign In</title></head>
<body>
<h1>Please Sign In</h1>
<form action="/login" method="POST">
  <input type="text" name="user">
  <input type="password" name="pass">
  <button type="submit">Sign In</button>
</form>
</body>
</html>`))
	}))
	defer ts.Close()

	home := t.TempDir()
	cfg := Config{
		Enabled:        true,
		BrowseTimeoutS: 15,
		MaxTextBytes:   DefaultMaxTextBytes,
		ChromePath:     chrome,
	}

	browser := NewBrowser(home, cfg, WithAllowedTestHosts(ts.URL))

	res, err := browser.Browse(context.Background(), ts.URL, "read page")
	if err != nil {
		t.Fatalf("browse failed: %v", err)
	}

	u, _ := url.Parse(ts.URL)
	expectedPrefix := fmt.Sprintf("[login-wall origin=%s] Propose sign-in to the user rather than retry.", u.Hostname())
	if !strings.Contains(res, expectedPrefix) {
		t.Fatalf("expected login-wall marker %q, got: %s", expectedPrefix, res)
	}
}

func TestBrowse_NoMarkerOnReadPage(t *testing.T) {
	chrome := skipWithoutChrome(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!DOCTYPE html>
<html>
<head><title>Public Documentation</title></head>
<body>
<h1>Docs</h1>
<p>Public info page without authentication</p>
</body>
</html>`))
	}))
	defer ts.Close()

	home := t.TempDir()
	cfg := Config{
		Enabled:        true,
		BrowseTimeoutS: 15,
		MaxTextBytes:   DefaultMaxTextBytes,
		ChromePath:     chrome,
	}

	browser := NewBrowser(home, cfg, WithAllowedTestHosts(ts.URL))

	res, err := browser.Browse(context.Background(), ts.URL+"/docs", "read docs")
	if err != nil {
		t.Fatalf("browse failed: %v", err)
	}

	if strings.Contains(res, "[login-wall") {
		t.Fatalf("expected no login-wall marker on public read page, got: %s", res)
	}
}

func TestWebBrowse_BotWallBlocked(t *testing.T) {
	chrome := skipWithoutChrome(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`<!DOCTYPE html><html><head><title>Just a moment...</title></head><body>Just a moment... Enable JavaScript and cookies to continue</body></html>`))
	}))
	defer ts.Close()

	cfg := Config{
		Enabled:        true,
		BrowseTimeoutS: 15,
		MaxTextBytes:   50000,
		ChromePath:     chrome,
	}

	dataDir := t.TempDir()
	browser := NewBrowser(dataDir, cfg, WithAllowedTestHosts(ts.URL))

	res, err := browser.Browse(context.Background(), ts.URL, "read page")
	if err != nil {
		t.Fatalf("browse failed: %v", err)
	}
	if !strings.HasPrefix(res, "[blocked] This site refused automated access.") {
		t.Fatalf("expected result to start with '[blocked] This site refused automated access.', got: %s", res)
	}
	if !strings.Contains(res, "Just a moment...") {
		t.Fatalf("expected result to include body snippet, got: %s", res)
	}
}

func TestWebBrowse_StealthFlags(t *testing.T) {
	chrome := skipWithoutChrome(t)

	var receivedUA string
	var receivedAcceptLang string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUA = r.Header.Get("User-Agent")
		receivedAcceptLang = r.Header.Get("Accept-Language")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!DOCTYPE html><html><head><title>Stealth Test</title></head><body>Loaded</body></html>`))
	}))
	defer ts.Close()

	cfg := Config{
		Enabled:        true,
		BrowseTimeoutS: 15,
		MaxTextBytes:   50000,
		ChromePath:     chrome,
	}

	dataDir := t.TempDir()
	browser := NewBrowser(dataDir, cfg, WithAllowedTestHosts(ts.URL))

	_, err := browser.Browse(context.Background(), ts.URL, "test flags")
	if err != nil {
		t.Fatalf("browse failed: %v", err)
	}

	if receivedUA != BrowserUserAgent {
		t.Errorf("User-Agent = %q, want %q", receivedUA, BrowserUserAgent)
	}
	if !strings.Contains(receivedAcceptLang, "en-US") {
		t.Errorf("Accept-Language = %q, want it to contain en-US", receivedAcceptLang)
	}
}

func TestWebBrowse_WriteThrough_DirtyDetection(t *testing.T) {
	origFP := profileFingerprint
	defer func() { profileFingerprint = origFP }()

	home := t.TempDir()
	w := NewWallet(home, nil)
	origin := "carrier.example.com"

	// Seed session in wallet
	stagingDir := filepath.Join(t.TempDir(), "seed-profile")
	_ = os.MkdirAll(filepath.Join(stagingDir, "Default"), 0o700)
	_ = os.WriteFile(filepath.Join(stagingDir, "Default", "Cookies"), []byte("seed-cookie"), 0o600)
	if err := w.Store(origin, stagingDir); err != nil {
		t.Fatalf("seed Store: %v", err)
	}

	cfg := DefaultConfig()
	browser := NewBrowser(home, cfg, WithWallet(w))

	// Setup seam to detect dirty change: fp1 on load, fp2 on release
	calls := 0
	profileFingerprint = func(dir string) (string, error) {
		calls++
		if calls == 1 {
			return "fp-seed", nil
		}
		// Simulate browser modifying the cookie DB before release
		_ = os.WriteFile(filepath.Join(dir, "Default", "Cookies"), []byte("updated-cookie-db"), 0o600)
		return "fp-updated", nil
	}

	sessionActive, releaseFunc, activeDir, err := browser.prepareSession(origin)
	if err != nil {
		t.Fatalf("prepareSession failed: %v", err)
	}
	if !sessionActive {
		t.Fatalf("expected sessionActive=true")
	}
	if releaseFunc == nil {
		t.Fatalf("expected non-nil releaseFunc")
	}

	// Release without browse error -> should write through to wallet before wiping activeDir
	releaseFunc(nil)

	// Verify activeDir was removed on release
	if _, err := os.Stat(activeDir); !os.IsNotExist(err) {
		t.Fatalf("activeDir should be wiped after release, got err: %v", err)
	}

	// Verify the wallet now contains the updated cookie DB (Store was called before rel)
	checkDir, checkRel, err := w.Load(origin)
	if err != nil {
		t.Fatalf("Load after write-through: %v", err)
	}
	defer func() { _ = checkRel() }()

	updatedData, err := os.ReadFile(filepath.Join(checkDir, "Default", "Cookies"))
	if err != nil {
		t.Fatalf("read updated cookie DB: %v", err)
	}
	if string(updatedData) != "updated-cookie-db" {
		t.Errorf("cookie DB data = %q, want %q", string(updatedData), "updated-cookie-db")
	}
}

func TestWebBrowse_WriteThrough_UnchangedNoStore(t *testing.T) {
	origFP := profileFingerprint
	defer func() { profileFingerprint = origFP }()

	home := t.TempDir()
	w := NewWallet(home, nil)
	origin := "carrier.example.com"

	stagingDir := filepath.Join(t.TempDir(), "seed-profile")
	_ = os.MkdirAll(filepath.Join(stagingDir, "Default"), 0o700)
	_ = os.WriteFile(filepath.Join(stagingDir, "Default", "Cookies"), []byte("seed-cookie"), 0o600)
	if err := w.Store(origin, stagingDir); err != nil {
		t.Fatalf("seed Store: %v", err)
	}

	cfg := DefaultConfig()
	browser := NewBrowser(home, cfg, WithWallet(w))

	// Return identical fingerprint on both calls
	profileFingerprint = func(dir string) (string, error) {
		return "fp-identical", nil
	}

	sessionActive, releaseFunc, _, err := browser.prepareSession(origin)
	if err != nil {
		t.Fatalf("prepareSession failed: %v", err)
	}
	if !sessionActive {
		t.Fatalf("expected sessionActive=true")
	}

	// Release without browse error
	releaseFunc(nil)

	// Verify wallet still has original seed cookie
	checkDir, checkRel, err := w.Load(origin)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer func() { _ = checkRel() }()

	data, err := os.ReadFile(filepath.Join(checkDir, "Default", "Cookies"))
	if err != nil {
		t.Fatalf("read cookie DB: %v", err)
	}
	if string(data) != "seed-cookie" {
		t.Errorf("cookie DB data = %q, want %q", string(data), "seed-cookie")
	}
}

func TestWebBrowse_WriteThrough_BrowseErrorNoStore(t *testing.T) {
	origFP := profileFingerprint
	defer func() { profileFingerprint = origFP }()

	home := t.TempDir()
	w := NewWallet(home, nil)
	origin := "carrier.example.com"

	stagingDir := filepath.Join(t.TempDir(), "seed-profile")
	_ = os.MkdirAll(filepath.Join(stagingDir, "Default"), 0o700)
	_ = os.WriteFile(filepath.Join(stagingDir, "Default", "Cookies"), []byte("original-cookie"), 0o600)
	if err := w.Store(origin, stagingDir); err != nil {
		t.Fatalf("seed Store: %v", err)
	}

	cfg := DefaultConfig()
	browser := NewBrowser(home, cfg, WithWallet(w))

	calls := 0
	profileFingerprint = func(dir string) (string, error) {
		calls++
		if calls == 1 {
			return "fp1", nil
		}
		_ = os.WriteFile(filepath.Join(dir, "Default", "Cookies"), []byte("error-attempt-cookie"), 0o600)
		return "fp2", nil
	}

	sessionActive, releaseFunc, _, err := browser.prepareSession(origin)
	if err != nil {
		t.Fatalf("prepareSession failed: %v", err)
	}
	if !sessionActive {
		t.Fatalf("expected sessionActive=true")
	}

	// Release WITH browse error -> must NOT store to wallet
	releaseFunc(errors.New("navigation failed: context deadline exceeded"))

	// Verify wallet was NOT overwritten with error-attempt-cookie
	checkDir, checkRel, err := w.Load(origin)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer func() { _ = checkRel() }()

	data, err := os.ReadFile(filepath.Join(checkDir, "Default", "Cookies"))
	if err != nil {
		t.Fatalf("read cookie DB: %v", err)
	}
	if string(data) != "original-cookie" {
		t.Errorf("cookie DB data = %q, want original-cookie (must not store on error)", string(data))
	}
}

func TestWebBrowse_WriteThrough_AnonymousNoStore(t *testing.T) {
	home := t.TempDir()
	w := NewWallet(home, nil)
	cfg := DefaultConfig()
	browser := NewBrowser(home, cfg, WithWallet(w))

	sessionActive, releaseFunc, userDataDir, err := browser.prepareSession("anonymous.example.com")
	if err != nil {
		t.Fatalf("prepareSession failed: %v", err)
	}
	if sessionActive {
		t.Errorf("expected sessionActive=false for anonymous origin")
	}
	if releaseFunc != nil {
		t.Errorf("expected nil releaseFunc for anonymous origin")
	}
	if userDataDir != "" {
		t.Errorf("expected empty userDataDir for anonymous origin")
	}
}

func TestProfileFingerprint_Default(t *testing.T) {
	dir := t.TempDir()

	// 1. Missing cookie DB -> returns ("", nil)
	fp, err := defaultProfileFingerprint(dir)
	if err != nil {
		t.Fatalf("unexpected error for missing cookie DB: %v", err)
	}
	if fp != "" {
		t.Errorf("expected empty fingerprint for missing file, got: %q", fp)
	}

	// 2. Present cookie DB -> returns sha256 hex
	cookiePath := filepath.Join(dir, "Default", "Cookies")
	_ = os.MkdirAll(filepath.Dir(cookiePath), 0o700)
	cookieData := []byte("sqlite-format-cookie-data")
	if err := os.WriteFile(cookiePath, cookieData, 0o600); err != nil {
		t.Fatalf("write cookie: %v", err)
	}

	fp, err = defaultProfileFingerprint(dir)
	if err != nil {
		t.Fatalf("unexpected error for present cookie DB: %v", err)
	}
	if len(fp) != 64 {
		t.Errorf("expected 64-char sha256 hex string, got %q (len %d)", fp, len(fp))
	}

	// 3. Changed cookie DB -> returns different sha256 hex
	if err := os.WriteFile(cookiePath, []byte("sqlite-format-cookie-data-changed"), 0o600); err != nil {
		t.Fatalf("write changed cookie: %v", err)
	}
	fp2, err := defaultProfileFingerprint(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fp2 == fp {
		t.Errorf("fingerprint did not change when file contents changed")
	}
}

func TestBrowse_LoginWall_MarksStaleWithWallet(t *testing.T) {
	chrome := skipWithoutChrome(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!DOCTYPE html>
<html>
<head><title>Sign In</title></head>
<body>
<h1>Please Sign In</h1>
<form action="/login" method="POST">
  <input type="text" name="user">
  <input type="password" name="pass">
  <button type="submit">Sign In</button>
</form>
</body>
</html>`))
	}))
	defer ts.Close()

	home := t.TempDir()
	u, _ := url.Parse(ts.URL)
	origin := u.Hostname()

	w := NewWallet(home, nil)
	// Seed a session file for this origin
	activeDir, release, err := w.Load(origin)
	if err != nil {
		t.Fatalf("seed Load failed: %v", err)
	}
	_ = os.WriteFile(filepath.Join(activeDir, "dummy_profile_marker"), []byte("active"), 0o600)
	if err := release(); err != nil {
		t.Fatalf("seed release failed: %v", err)
	}

	cfg := Config{
		Enabled:        true,
		BrowseTimeoutS: 15,
		MaxTextBytes:   DefaultMaxTextBytes,
		ChromePath:     chrome,
	}

	browser := NewBrowser(home, cfg, WithAllowedTestHosts(ts.URL), WithWallet(w))

	res, err := browser.Browse(context.Background(), ts.URL, "read page")
	if err != nil {
		t.Fatalf("browse failed: %v", err)
	}

	// 1. Guidance line appended to tool result
	expectedGuidance := fmt.Sprintf("The saved session for %s appears expired — suggest calling session_login %s to refresh it (an approval card reaches the owner).", origin, origin)
	if !strings.Contains(res, expectedGuidance) {
		t.Errorf("expected guidance %q in result, got: %s", expectedGuidance, res)
	}

	// 2. MarkStale was called on the index
	entry, ok := browser.Index().Get(origin)
	if !ok {
		t.Fatalf("expected origin in index")
	}
	if !entry.Stale {
		t.Errorf("expected index entry to be marked Stale=true")
	}
}

func TestWebBrowse_WriteThrough_RealBrowser(t *testing.T) {
	chrome := skipWithoutChrome(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: test fixture cookie
			Name:     "test_cookie",
			Value:    "logged_in_value",
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!DOCTYPE html><html><head><title>Cookie Page</title></head><body>Cookie set</body></html>`))
	}))
	defer ts.Close()

	home := t.TempDir()
	u, _ := url.Parse(ts.URL)
	origin := u.Hostname()

	w := NewWallet(home, nil)
	// Seed a session file for this origin
	activeDir, release, err := w.Load(origin)
	if err != nil {
		t.Fatalf("seed Load failed: %v", err)
	}
	_ = os.WriteFile(filepath.Join(activeDir, "dummy_profile_marker"), []byte("active"), 0o600)
	if err := release(); err != nil {
		t.Fatalf("seed release failed: %v", err)
	}

	cfg := Config{
		Enabled:        true,
		BrowseTimeoutS: 15,
		MaxTextBytes:   DefaultMaxTextBytes,
		ChromePath:     chrome,
	}

	browser := NewBrowser(home, cfg, WithAllowedTestHosts(ts.URL), WithWallet(w))

	res, err := browser.Browse(context.Background(), ts.URL, "view page")
	if err != nil {
		t.Fatalf("browse failed: %v", err)
	}
	if !strings.Contains(res, "Cookie set") {
		t.Fatalf("unexpected output: %s", res)
	}

	// Verify the session in wallet has the Cookies file written through
	checkDir, checkRel, err := w.Load(origin)
	if err != nil {
		t.Fatalf("Load after real browse: %v", err)
	}
	defer func() { _ = checkRel() }()

	cookieFile := filepath.Join(checkDir, "Default", "Cookies")
	if _, err := os.Stat(cookieFile); err != nil {
		t.Fatalf("expected Default/Cookies to be written through to wallet, got err: %v", err)
	}
}
