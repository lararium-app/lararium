package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupTestSigninWindow(t *testing.T, timeout time.Duration) (*SigninManager, string, func()) {
	t.Helper()
	home := t.TempDir()
	w := NewWallet(home, nil)
	stagingDir := filepath.Join(t.TempDir(), "staging-profile")
	_ = os.MkdirAll(stagingDir, 0o700)
	_ = os.WriteFile(filepath.Join(stagingDir, "cookies.db"), []byte("auth=1"), 0o600)

	assetsDir := filepath.Join(t.TempDir(), "assets")
	_ = os.MkdirAll(assetsDir, 0o700)
	indexHTML := "<!DOCTYPE html><html><head><meta http-equiv=\"refresh\" content=\"0; url=vnc.html\"></head><body>Redirecting</body></html>"
	_ = os.WriteFile(filepath.Join(assetsDir, "index.html"), []byte(indexHTML), 0o644)

	runner := newFakeRunner(stagingDir)
	runner.assetsDir = assetsDir

	mgr := NewSigninManager(w, runner, WithTimeout(timeout))
	info, err := mgr.Start(context.Background(), "https://carrier.example.com")
	if err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	token := mgr.Token()
	if token == "" {
		t.Fatal("expected non-empty token from Start")
	}
	if info.ViewURL == "" {
		t.Fatal("expected non-empty ViewURL in info")
	}

	return mgr, token, func() {
		_ = mgr.Cancel()
	}
}

func TestSigninHTTP_BadToken_404(t *testing.T) {
	mgr, _, cleanup := setupTestSigninWindow(t, 10*time.Minute)
	defer cleanup()

	handler := NewSigninHTTPHandler(mgr, nil)

	badTokens := []string{
		"invalid-token",
		"0000000000000000000000000000000000000000000000000000000000000000",
		"../escape",
		"",
	}

	for _, token := range badTokens {
		// 1. GET /signin/v/<token>
		req := httptest.NewRequest(http.MethodGet, "/signin/v/"+token, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /signin/v/%s code = %d, want 404", token, rec.Code)
		}

		// 2. POST /signin/api/<token>/done
		req = httptest.NewRequest(http.MethodPost, "/signin/api/"+token+"/done", nil)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST /signin/api/%s/done code = %d, want 404", token, rec.Code)
		}

		// 3. POST /signin/api/<token>/cancel
		req = httptest.NewRequest(http.MethodPost, "/signin/api/"+token+"/cancel", nil)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST /signin/api/%s/cancel code = %d, want 404", token, rec.Code)
		}

		// 4. GET /signin/api/<token>/status
		req = httptest.NewRequest(http.MethodGet, "/signin/api/"+token+"/status", nil)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /signin/api/%s/status code = %d, want 404", token, rec.Code)
		}
	}
}

func TestSigninHTTP_ViewPage(t *testing.T) {
	mgr, token, cleanup := setupTestSigninWindow(t, 10*time.Minute)
	defer cleanup()

	handler := NewSigninHTTPHandler(mgr, nil)

	req := httptest.NewRequest(http.MethodGet, "/signin/v/"+token, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /signin/v/%s code = %d, want 200", token, rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("view page Cache-Control = %q, want no-store (page embeds the RFB password)", cc)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want 'text/html; charset=utf-8'", ct)
	}
	if !strings.Contains(rec.Body.String(), "Redirecting") {
		t.Errorf("body does not contain index.html content: %s", rec.Body.String())
	}
}

func TestSigninHTTP_DoneCallsCompleteOnce(t *testing.T) {
	mgr, token, cleanup := setupTestSigninWindow(t, 10*time.Minute)
	defer cleanup()

	var cardResolved bool
	var cardAllow bool
	var cardIDSeen string

	resolver := func(cardID string, allow bool) error {
		cardResolved = true
		cardAllow = allow
		cardIDSeen = cardID
		return nil
	}
	mgr.SetCardID("card_signin_test_123")
	handler := NewSigninHTTPHandler(mgr, resolver)

	// 1. First Done call succeeds
	req := httptest.NewRequest(http.MethodPost, "/signin/api/"+token+"/done", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /signin/api/%s/done code = %d, want 200 (body: %s)", token, rec.Code, rec.Body.String())
	}
	if mgr.State() != SigninDone {
		t.Fatalf("mgr.State() = %s, want Done", mgr.State())
	}
	if !cardResolved || !cardAllow || cardIDSeen != "card_signin_test_123" {
		t.Fatalf("card resolved mismatch: resolved=%v allow=%v id=%s", cardResolved, cardAllow, cardIDSeen)
	}

	// 2. Second Done call returns 409 Conflict because state is now Done (not Live/Sealing)
	req2 := httptest.NewRequest(http.MethodPost, "/signin/api/"+token+"/done", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusConflict {
		t.Fatalf("second POST /done code = %d, want 409", rec2.Code)
	}
}

func TestSigninHTTP_CancelPath(t *testing.T) {
	mgr, token, cleanup := setupTestSigninWindow(t, 10*time.Minute)
	defer cleanup()

	var cardResolved bool
	var cardAllow bool
	var cardIDSeen string

	resolver := func(cardID string, allow bool) error {
		cardResolved = true
		cardAllow = allow
		cardIDSeen = cardID
		return nil
	}
	mgr.SetCardID("card_cancel_123")
	handler := NewSigninHTTPHandler(mgr, resolver)

	req := httptest.NewRequest(http.MethodPost, "/signin/api/"+token+"/cancel", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /signin/api/%s/cancel code = %d, want 200", token, rec.Code)
	}
	if mgr.State() != SigninCanceled {
		t.Fatalf("mgr.State() = %s, want Canceled", mgr.State())
	}
	if !cardResolved || cardAllow || cardIDSeen != "card_cancel_123" {
		t.Fatalf("card cancel mismatch: resolved=%v allow=%v id=%s", cardResolved, cardAllow, cardIDSeen)
	}
}

func TestSigninHTTP_StatusShape(t *testing.T) {
	mgr, token, cleanup := setupTestSigninWindow(t, 10*time.Minute)
	defer cleanup()

	handler := NewSigninHTTPHandler(mgr, nil)

	req := httptest.NewRequest(http.MethodGet, "/signin/api/"+token+"/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /signin/api/%s/status code = %d, want 200", token, rec.Code)
	}

	var parsed map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("parse status JSON: %v", err)
	}

	// Status must contain ONLY state — never port, password, or paths
	if len(parsed) != 1 {
		t.Fatalf("status JSON has %d fields, want exactly 1: %+v", len(parsed), parsed)
	}
	stateVal, ok := parsed["state"]
	if !ok || stateVal != "Live" {
		t.Fatalf("parsed['state'] = %v, want 'Live'", stateVal)
	}
	for _, forbidden := range []string{"port", "password", "bridge_port", "assets_dir", "view_url"} {
		if _, exists := parsed[forbidden]; exists {
			t.Fatalf("forbidden field %q leaked in status JSON", forbidden)
		}
	}
}

func TestSigninHTTP_TTLExpiry(t *testing.T) {
	// Window with very short TTL
	mgr, token, cleanup := setupTestSigninWindow(t, 50*time.Millisecond)
	defer cleanup()

	handler := NewSigninHTTPHandler(mgr, nil)

	// Wait for TTL to expire
	time.Sleep(75 * time.Millisecond)

	// Now all requests must 404
	req := httptest.NewRequest(http.MethodGet, "/signin/v/"+token, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET after TTL code = %d, want 404", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/signin/api/"+token+"/status", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET status after TTL code = %d, want 404", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/signin/api/"+token+"/done", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST done after TTL code = %d, want 404", rec.Code)
	}
}
