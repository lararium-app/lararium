package surface

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lararium-app/lararium/internal/web"
)

type webSessionsTestEnv struct {
	srv     *Server
	wallet  *web.Wallet
	index   *web.WalletIndex
	act     *web.ActivityLogger
	tok     string
	handler http.Handler
	home    string
}

func setupWebSessionsEnv(t *testing.T) *webSessionsTestEnv {
	t.Helper()
	home := t.TempDir()

	store, err := OpenTokenStore(filepath.Join(home, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := store.Create("web-sessions-unit-test")
	if err != nil {
		t.Fatal(err)
	}

	wallet := web.NewWallet(home, nil)
	index := web.NewWalletIndex(home)
	act := web.NewActivityLogger(home)

	srv := &Server{
		Store:    store,
		Sessions: &fakeSessionSource{},
		Hub:      &fakeHub{},
		Cfg:      ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}
	srv.SetWebWallet(wallet, index, act)
	h := NewServer(srv)

	return &webSessionsTestEnv{
		srv:     srv,
		wallet:  wallet,
		index:   index,
		act:     act,
		tok:     tok,
		handler: h,
		home:    home,
	}
}

func (e *webSessionsTestEnv) do(method, path, body string) *httptest.ResponseRecorder {
	return credDo(e.handler, method, path, e.tok, body)
}

func TestWebSessionsCRUD(t *testing.T) {
	e := setupWebSessionsEnv(t)

	// 1. Initial list: empty
	w := e.do(http.MethodGet, "/v1/web/sessions", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/web/sessions status = %d, want 200", w.Code)
	}
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("expected [], got: %s", w.Body.String())
	}

	// 2. Seed a profile and index entry
	const canaryCookie = "CANARY_SECRET_COOKIE_DATA_xyz789"
	dir, rel, err := e.wallet.Load("ups.com")
	if err != nil {
		t.Fatalf("seed Load failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cookies.sqlite"), []byte(canaryCookie), 0o600); err != nil {
		t.Fatalf("write canary cookie: %v", err)
	}
	if err := rel(); err != nil {
		t.Fatalf("seed release failed: %v", err)
	}

	if err := e.index.Set(web.WalletEntry{
		Origin:     "ups.com",
		Policy:     "always_ask",
		CreatedAt:  "2026-11-05T01:00:00Z",
		LastUsedAt: "2026-11-05T01:15:00Z",
		LastGoal:   "track my package",
	}); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	// 3. GET /v1/web/sessions returns the row, never cookies
	w = e.do(http.MethodGet, "/v1/web/sessions", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/web/sessions status = %d, want 200", w.Code)
	}

	rawBody := w.Body.String()
	if strings.Contains(rawBody, canaryCookie) {
		t.Fatalf("CRITICAL SECURITY LEAK: cookie contents found in GET /v1/web/sessions response: %s", rawBody)
	}

	var rows []webSessionRow
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].Origin != "ups.com" || rows[0].Policy != "always_ask" || rows[0].LastGoal != "track my package" {
		t.Fatalf("unexpected row content: %+v", rows[0])
	}

	// 4. PATCH /v1/web/sessions/ups.com updates policy
	w = e.do(http.MethodPatch, "/v1/web/sessions/ups.com", `{"policy":"daily_allow"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, body = %s", w.Code, w.Body.String())
	}
	var updated webSessionRow
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("unmarshal patch response: %v", err)
	}
	if updated.Policy != "daily_allow" {
		t.Fatalf("updated policy = %q, want daily_allow", updated.Policy)
	}

	// Verify GET shows the updated policy
	w = e.do(http.MethodGet, "/v1/web/sessions", "")
	var rows2 []webSessionRow
	_ = json.Unmarshal(w.Body.Bytes(), &rows2)
	if len(rows2) != 1 || rows2[0].Policy != "daily_allow" {
		t.Fatalf("expected updated policy daily_allow, got %+v", rows2)
	}

	// 5. DELETE /v1/web/sessions/ups.com wipes profile, index, and logs sign_out
	w = e.do(http.MethodDelete, "/v1/web/sessions/ups.com", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", w.Code)
	}

	// Verify wallet profile wiped
	if e.wallet.Has("ups.com") {
		t.Fatal("expected wallet entry for ups.com to be deleted")
	}

	// Verify index wiped
	w = e.do(http.MethodGet, "/v1/web/sessions", "")
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("expected empty list after delete, got: %s", w.Body.String())
	}

	// Verify activity log recorded sign_out
	records, err := e.act.Query(10, "ups.com")
	if err != nil {
		t.Fatalf("query activity: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("expected activity record after delete")
	}
	if records[0].Kind != web.KindSignOut || records[0].Verdict != web.VerdictOK {
		t.Fatalf("expected sign_out ok record, got %+v", records[0])
	}

	// 6. DELETE non-existent session yields 404
	w = e.do(http.MethodDelete, "/v1/web/sessions/ups.com", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("DELETE non-existent session status = %d, want 404", w.Code)
	}
}

func TestWebSessionsHostGate(t *testing.T) {
	e := setupWebSessionsEnv(t)

	// 1. Disallowed Host header -> 403 Forbidden
	req1 := httptest.NewRequest(http.MethodGet, "/v1/web/sessions", nil)
	req1.Host = "evil.example.com"
	req1.Header.Set("Authorization", "Bearer "+e.tok)
	w1 := httptest.NewRecorder()
	e.handler.ServeHTTP(w1, req1)
	if w1.Code != http.StatusForbidden {
		t.Fatalf("disallowed Host status = %d, want 403", w1.Code)
	}
	if strings.TrimSpace(w1.Body.String()) != `{"error":"forbidden"}` {
		t.Fatalf("disallowed Host body = %q, want forbidden json", w1.Body.String())
	}

	// 2. Missing authorization token -> 401 Unauthorized
	req2 := httptest.NewRequest(http.MethodGet, "/v1/web/sessions", nil)
	req2.Host = "127.0.0.1:7717"
	w2 := httptest.NewRecorder()
	e.handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d, want 401", w2.Code)
	}

	// 3. Invalid token -> 401 Unauthorized
	req3 := httptest.NewRequest(http.MethodGet, "/v1/web/sessions", nil)
	req3.Host = "127.0.0.1:7717"
	req3.Header.Set("Authorization", "Bearer invalid-token-xyz")
	w3 := httptest.NewRecorder()
	e.handler.ServeHTTP(w3, req3)
	if w3.Code != http.StatusUnauthorized {
		t.Fatalf("invalid auth status = %d, want 401", w3.Code)
	}

	// 4. Valid host and token -> 200 OK
	req4 := httptest.NewRequest(http.MethodGet, "/v1/web/sessions", nil)
	req4.Host = "127.0.0.1:7717"
	req4.Header.Set("Authorization", "Bearer "+e.tok)
	w4 := httptest.NewRecorder()
	e.handler.ServeHTTP(w4, req4)
	if w4.Code != http.StatusOK {
		t.Fatalf("valid request status = %d, want 200", w4.Code)
	}
}

func TestWebSessionsPolicyEnumRejected(t *testing.T) {
	e := setupWebSessionsEnv(t)

	_ = e.index.Set(web.WalletEntry{
		Origin: "example.com",
		Policy: "always_ask",
	})

	// Invalid policy string
	w := e.do(http.MethodPatch, "/v1/web/sessions/example.com", `{"policy":"never_ask"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid policy status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid_policy") {
		t.Fatalf("expected invalid_policy reason, got: %s", w.Body.String())
	}

	// Empty policy
	w = e.do(http.MethodPatch, "/v1/web/sessions/example.com", `{"policy":""}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty policy status = %d, want 400", w.Code)
	}

	// Malformed JSON
	w = e.do(http.MethodPatch, "/v1/web/sessions/example.com", `{"policy":`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON status = %d, want 400", w.Code)
	}

	// Non-existent origin
	w = e.do(http.MethodPatch, "/v1/web/sessions/nonexistent.com", `{"policy":"daily_allow"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-existent origin status = %d, want 404", w.Code)
	}
}
