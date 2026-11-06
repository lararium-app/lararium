package surface

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

	"github.com/lararium-app/lararium/internal/web"
)

type surfaceTestRunner struct {
	profileDir string
	assetsDir  string
}

func (r *surfaceTestRunner) StartXvfb(ctx context.Context) (string, error) { return ":99", nil }
func (r *surfaceTestRunner) StopXvfb() error                               { return nil }
func (r *surfaceTestRunner) StartX11VNC(ctx context.Context, display string) (int, string, error) {
	return 5900, "ephemeral-vnc-pwd", nil
}
func (r *surfaceTestRunner) StopX11VNC() error { return nil }
func (r *surfaceTestRunner) StartChromium(ctx context.Context, display, profileDir, targetURL string) error {
	return nil
}
func (r *surfaceTestRunner) StopChromium() error { return nil }
func (r *surfaceTestRunner) StartWebsockify(ctx context.Context, rfbPort int, bindAddr string) (int, error) {
	return 6080, nil
}
func (r *surfaceTestRunner) StopWebsockify() error { return nil }
func (r *surfaceTestRunner) ProfileDir() string    { return r.profileDir }
func (r *surfaceTestRunner) AssetsDir() string     { return r.assetsDir }

type signinServerEnv struct {
	srv     *Server
	handler http.Handler
	mgr     *web.SigninManager
	token   string
	ap      *ApprovalHub
}

func setupSigninServerEnv(t *testing.T) *signinServerEnv {
	t.Helper()
	timeout := 10 * time.Minute
	home := t.TempDir()

	store, err := OpenTokenStore(filepath.Join(home, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}

	wallet := web.NewWallet(home, nil)
	index := web.NewWalletIndex(home)
	act := web.NewActivityLogger(home)

	profileDir := filepath.Join(t.TempDir(), "profile")
	_ = os.MkdirAll(profileDir, 0o700)
	_ = os.WriteFile(filepath.Join(profileDir, "cookies.db"), []byte("auth=ok"), 0o600)

	assetsDir := filepath.Join(t.TempDir(), "assets")
	_ = os.MkdirAll(assetsDir, 0o700)
	indexContent := "<!DOCTYPE html><html><body>Sign In Viewer</body></html>"
	_ = os.WriteFile(filepath.Join(assetsDir, "index.html"), []byte(indexContent), 0o644)

	runner := &surfaceTestRunner{
		profileDir: profileDir,
		assetsDir:  assetsDir,
	}

	mgr := web.NewSigninManager(wallet, runner, web.WithTimeout(timeout))
	_, err = mgr.Start(context.Background(), "https://carrier.example.com")
	if err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}

	ap := NewApprovalHub(5 * time.Minute)

	srv := &Server{
		Store:     store,
		Sessions:  &fakeSessionSource{},
		Hub:       &fakeHub{},
		Cfg:       ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
		approvals: ap,
	}
	srv.SetWebWallet(wallet, index, act)
	srv.SetWebSignin(mgr)

	h := NewServer(srv)

	return &signinServerEnv{
		srv:     srv,
		handler: h,
		mgr:     mgr,
		token:   mgr.Token(),
		ap:      ap,
	}
}

func TestSigninRoutes_AuthExemptAndServe(t *testing.T) {
	e := setupSigninServerEnv(t)

	// 1. GET /signin/v/<token> works WITHOUT Bearer Authorization header
	req := httptest.NewRequest(http.MethodGet, "/signin/v/"+e.token, nil)
	req.Host = "127.0.0.1:7717"
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /signin/v/%s code = %d, want 200", e.token, w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want 'text/html; charset=utf-8'", ct)
	}
	if !strings.Contains(w.Body.String(), "Sign In Viewer") {
		t.Errorf("unexpected body content: %s", w.Body.String())
	}

	// 2. GET /signin/api/<token>/status works without Bearer token
	reqStatus := httptest.NewRequest(http.MethodGet, "/signin/api/"+e.token+"/status", nil)
	reqStatus.Host = "127.0.0.1:7717"
	wStatus := httptest.NewRecorder()
	e.handler.ServeHTTP(wStatus, reqStatus)

	if wStatus.Code != http.StatusOK {
		t.Fatalf("GET /signin/api/%s/status code = %d, want 200", e.token, wStatus.Code)
	}
	var st map[string]string
	if err := json.Unmarshal(wStatus.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal status JSON: %v", err)
	}
	if st["state"] != "Live" {
		t.Errorf("state = %q, want 'Live'", st["state"])
	}

	// 3. Bad token gives 404 with no info leak
	badReq := httptest.NewRequest(http.MethodGet, "/signin/v/not-a-valid-token", nil)
	badReq.Host = "127.0.0.1:7717"
	wBad := httptest.NewRecorder()
	e.handler.ServeHTTP(wBad, badReq)
	if wBad.Code != http.StatusNotFound {
		t.Errorf("bad token code = %d, want 404", wBad.Code)
	}
}

func TestSigninRoutes_HostGateApplies(t *testing.T) {
	e := setupSigninServerEnv(t)

	// Disallowed Host header must be rejected by HostGate (403 Forbidden)
	req := httptest.NewRequest(http.MethodGet, "/signin/v/"+e.token, nil)
	req.Host = "attacker.example.com"
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("disallowed host code = %d, want 403 Forbidden", w.Code)
	}
}

func TestSigninRoutes_DoneResolvesCustodyCard(t *testing.T) {
	e := setupSigninServerEnv(t)

	// Register a custody card on ApprovalHub
	cardID, decisionCh := e.ap.RegisterCustosNamed("", "", "Sign me in to carrier.example.com", "carrier.example.com", "/signin/v/"+e.token, 5*time.Minute, nil)
	e.mgr.SetCardID(cardID)

	// Call POST /signin/api/<token>/done
	req := httptest.NewRequest(http.MethodPost, "/signin/api/"+e.token+"/done", nil)
	req.Host = "127.0.0.1:7717"
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST /signin/api/%s/done code = %d, want 200 (body: %s)", e.token, w.Code, w.Body.String())
	}
	if e.mgr.State() != web.SigninDone {
		t.Errorf("mgr state = %s, want Done", e.mgr.State())
	}

	// Custody card decision channel must receive true (approved)
	select {
	case decision := <-decisionCh:
		if !decision {
			t.Fatal("expected custody card decision to be true (approved), got false")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for custody card decision")
	}
}

func TestSigninRoutes_CancelResolvesCustodyCardDenied(t *testing.T) {
	e := setupSigninServerEnv(t)

	// Register a custody card on ApprovalHub
	cardID, decisionCh := e.ap.RegisterCustosNamed("", "", "Sign me in to carrier.example.com", "carrier.example.com", "/signin/v/"+e.token, 5*time.Minute, nil)
	e.mgr.SetCardID(cardID)

	// Call POST /signin/api/<token>/cancel
	req := httptest.NewRequest(http.MethodPost, "/signin/api/"+e.token+"/cancel", nil)
	req.Host = "127.0.0.1:7717"
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST /signin/api/%s/cancel code = %d, want 200", e.token, w.Code)
	}
	if e.mgr.State() != web.SigninCanceled {
		t.Errorf("mgr state = %s, want Canceled", e.mgr.State())
	}

	// Custody card decision channel must receive false (denied)
	select {
	case decision := <-decisionCh:
		if decision {
			t.Fatal("expected custody card decision to be false (denied), got true")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for custody card decision")
	}
}
