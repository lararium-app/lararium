package surface

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakePolicySource is an in-memory implementation of PolicySource for tests.
// It has zero imports of internal/custos per the frozen import direction.
type fakePolicySource struct {
	mu           sync.Mutex
	rules        []PolicyRule
	egressStrict bool
	listErr      error
	addNotes     []string
	addErr       error
	removeOK     bool
	removeErr    error
	lastLane     string
	lastPattern  string
	lastVerdict  string
}

func (f *fakePolicySource) ListRules() ([]PolicyRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rules, nil
}

func (f *fakePolicySource) EgressStrict() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.egressStrict
}

func (f *fakePolicySource) AddRule(lane, pattern, verdict string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastLane = lane
	f.lastPattern = pattern
	f.lastVerdict = verdict
	if f.addErr != nil {
		return nil, f.addErr
	}
	return f.addNotes, nil
}

func (f *fakePolicySource) RemoveRule(lane, pattern string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastLane = lane
	f.lastPattern = pattern
	if f.removeErr != nil {
		return false, f.removeErr
	}
	return f.removeOK, nil
}

func setupPolicyTestServer(t *testing.T, src PolicySource) (string, http.Handler) {
	t.Helper()
	store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	tokVal, err := store.Create("policy-unit-test")
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		Store:    store,
		Sessions: &fakeSessionSource{},
		Hub:      &fakeHub{},
		Cfg:      ServeConfig{AllowedHosts: []string{"localhost", "127.0.0.1"}},
	}
	if src != nil {
		srv.SetPolicySource(src)
	}
	return tokVal, NewServer(srv)
}

func TestPolicy_NilSource(t *testing.T) {
	tok, handler := setupPolicyTestServer(t, nil)

	// GET -> 423 locked
	{
		req := httptest.NewRequest(http.MethodGet, "/v1/policy/rules", nil)
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusLocked {
			t.Fatalf("GET status = %d, want %d", w.Code, http.StatusLocked)
		}
		if !strings.Contains(w.Body.String(), `"error":"locked"`) {
			t.Errorf("GET body = %q, want locked error", w.Body.String())
		}
	}

	// PUT -> 423 locked
	{
		body := `{"lane":"credential","pattern":"demo/echo","verdict":"ask"}`
		req := httptest.NewRequest(http.MethodPut, "/v1/policy/rules", strings.NewReader(body))
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusLocked {
			t.Fatalf("PUT status = %d, want %d", w.Code, http.StatusLocked)
		}
		if !strings.Contains(w.Body.String(), `"error":"locked"`) {
			t.Errorf("PUT body = %q, want locked error", w.Body.String())
		}
	}

	// DELETE -> 423 locked
	{
		req := httptest.NewRequest(http.MethodDelete, "/v1/policy/rules/demo%2fecho", nil)
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusLocked {
			t.Fatalf("DELETE status = %d, want %d", w.Code, http.StatusLocked)
		}
		if !strings.Contains(w.Body.String(), `"error":"locked"`) {
			t.Errorf("DELETE body = %q, want locked error", w.Body.String())
		}
	}
}

func TestPolicy_GET_HappyPath(t *testing.T) {
	src := &fakePolicySource{
		egressStrict: true,
		rules: []PolicyRule{
			{Lane: "credential", Pattern: "demo/echo", Verdict: "ask", Always: false, Source: "web"},
		},
	}
	tok, handler := setupPolicyTestServer(t, src)

	req := httptest.NewRequest(http.MethodGet, "/v1/policy/rules", nil)
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp struct {
		EgressStrict bool         `json:"egress_strict"`
		Rules        []PolicyRule `json:"rules"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !resp.EgressStrict {
		t.Errorf("egress_strict = false, want true")
	}
	if len(resp.Rules) != 1 || resp.Rules[0].Pattern != "demo/echo" {
		t.Errorf("rules mismatch: %+v", resp.Rules)
	}
}

func TestPolicy_GET_NullArrayGuard(t *testing.T) {
	src := &fakePolicySource{
		egressStrict: false,
		rules:        nil, // explicitly nil slice
	}
	tok, handler := setupPolicyTestServer(t, src)

	req := httptest.NewRequest(http.MethodGet, "/v1/policy/rules", nil)
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	body := w.Body.String()
	if strings.Contains(body, `"rules":null`) {
		t.Errorf("rules must not be null in body: %s", body)
	}
	if !strings.Contains(body, `"rules":[]`) {
		t.Errorf("rules must marshal as empty array []: %s", body)
	}
}

func TestPolicy_PUT_HappyPath(t *testing.T) {
	src := &fakePolicySource{
		addNotes: []string{"domination warning"},
	}
	tok, handler := setupPolicyTestServer(t, src)

	// Credential lane
	{
		body := `{"lane":"credential","pattern":"demo/echo","verdict":"ask"}`
		req := httptest.NewRequest(http.MethodPut, "/v1/policy/rules", strings.NewReader(body))
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
		}
		var resp struct {
			OK    bool     `json:"ok"`
			Notes []string `json:"notes"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if !resp.OK {
			t.Errorf("ok = false, want true")
		}
		if len(resp.Notes) != 1 || resp.Notes[0] != "domination warning" {
			t.Errorf("notes = %+v, want ['domination warning']", resp.Notes)
		}
		if src.lastLane != "credential" || src.lastPattern != "demo/echo" || src.lastVerdict != "ask" {
			t.Errorf("AddRule args = (%q, %q, %q)", src.lastLane, src.lastPattern, src.lastVerdict)
		}
	}

	// Egress lane with empty notes
	src.addNotes = nil
	{
		body := `{"lane":"egress","pattern":"*.proxy.test","verdict":"auto"}`
		req := httptest.NewRequest(http.MethodPut, "/v1/policy/rules", strings.NewReader(body))
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if strings.Contains(w.Body.String(), `"notes":null`) {
			t.Errorf("notes must not marshal as null: %s", w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"notes":[]`) {
			t.Errorf("notes must marshal as empty array: %s", w.Body.String())
		}
		if src.lastLane != "egress" {
			t.Errorf("lastLane = %q, want egress", src.lastLane)
		}
	}
}

func TestPolicy_PUT_DisallowUnknownFields(t *testing.T) {
	src := &fakePolicySource{}
	tok, handler := setupPolicyTestServer(t, src)

	// Extra field "always"
	body := `{"lane":"credential","pattern":"demo/echo","verdict":"ask","always":true}`
	req := httptest.NewRequest(http.MethodPut, "/v1/policy/rules", strings.NewReader(body))
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}

	// Malformed JSON
	reqBad := httptest.NewRequest(http.MethodPut, "/v1/policy/rules", strings.NewReader("bad-json"))
	reqBad.Host = "127.0.0.1:7717"
	reqBad.Header.Set("Authorization", "Bearer "+tok)
	wBad := httptest.NewRecorder()
	handler.ServeHTTP(wBad, reqBad)

	if wBad.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", wBad.Code)
	}
}

func TestPolicy_DELETE_HappyPath(t *testing.T) {
	src := &fakePolicySource{removeOK: true}
	tok, handler := setupPolicyTestServer(t, src)

	// 1. First remove -> removed: true
	req := httptest.NewRequest(http.MethodDelete, "/v1/policy/rules/demo%2fecho", nil)
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var resp struct {
		OK      bool `json:"ok"`
		Removed bool `json:"removed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !resp.OK || !resp.Removed {
		t.Errorf("resp = %+v, want ok:true removed:true", resp)
	}
	if src.lastPattern != "demo/echo" {
		t.Errorf("lastPattern = %q, want demo/echo", src.lastPattern)
	}

	// 2. Second remove -> removed: false
	src.removeOK = false
	req2 := httptest.NewRequest(http.MethodDelete, "/v1/policy/rules/demo%2fecho", nil)
	req2.Host = "127.0.0.1:7717"
	req2.Header.Set("Authorization", "Bearer "+tok)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w2.Code, http.StatusOK)
	}
	var resp2 struct {
		OK      bool `json:"ok"`
		Removed bool `json:"removed"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !resp2.OK || resp2.Removed {
		t.Errorf("resp2 = %+v, want ok:true removed:false", resp2)
	}
}

func TestPolicy_DELETE_LaneGate(t *testing.T) {
	src := &fakePolicySource{removeOK: true}
	tok, handler := setupPolicyTestServer(t, src)

	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantLane   string
	}{
		{"absent lane (dual-lane)", "", http.StatusOK, ""},
		{"credential lane", "?lane=credential", http.StatusOK, "credential"},
		{"egress lane", "?lane=egress", http.StatusOK, "egress"},
		{"invalid lane", "?lane=worker", http.StatusBadRequest, ""},
		{"empty query val", "?lane=bogus", http.StatusBadRequest, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodDelete, "/v1/policy/rules/demo%2fecho"+tc.query, nil)
			req.Host = "127.0.0.1:7717"
			req.Header.Set("Authorization", "Bearer "+tok)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK && src.lastLane != tc.wantLane {
				t.Errorf("lastLane = %q, want %q", src.lastLane, tc.wantLane)
			}
		})
	}
}

func TestPolicy_DELETE_PatternGate(t *testing.T) {
	src := &fakePolicySource{removeOK: true}
	tok, handler := setupPolicyTestServer(t, src)

	badPaths := []struct {
		name string
		path string
	}{
		{"null byte control char", "/v1/policy/rules/%00"},
		{"space character", "/v1/policy/rules/demo%20echo"},
		{"empty pattern trailing slash", "/v1/policy/rules/"},
		{"over 256 chars", "/v1/policy/rules/" + strings.Repeat("a", 257)},
		{"non-printable ascii tab", "/v1/policy/rules/%09"},
	}

	for _, tc := range badPaths {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodDelete, tc.path, nil)
			req.Host = "127.0.0.1:7717"
			req.Header.Set("Authorization", "Bearer "+tok)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusNotFound {
				t.Fatalf("path %s status = %d, want 404", tc.path, w.Code)
			}
			if !strings.Contains(w.Body.String(), `"error":"not found"`) {
				t.Errorf("body = %q, want not found", w.Body.String())
			}
		})
	}
}

func TestPolicy_ErrorMapping_LockedAndCorrupt(t *testing.T) {
	src := &fakePolicySource{}
	tok, handler := setupPolicyTestServer(t, src)

	// 1. GET locked -> 423
	src.listErr = ErrPolicyLocked
	{
		req := httptest.NewRequest(http.MethodGet, "/v1/policy/rules", nil)
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusLocked {
			t.Fatalf("GET locked status = %d, want 423", w.Code)
		}
	}

	// 2. GET corrupt -> 503
	src.listErr = ErrPolicyCorrupt
	{
		req := httptest.NewRequest(http.MethodGet, "/v1/policy/rules", nil)
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET corrupt status = %d, want 503", w.Code)
		}
		if !strings.Contains(w.Body.String(), `"error":"policy_corrupt"`) {
			t.Errorf("body = %q, want policy_corrupt", w.Body.String())
		}
	}
	src.listErr = nil

	// 3. PUT locked -> 423
	src.addErr = ErrPolicyLocked
	{
		req := httptest.NewRequest(http.MethodPut, "/v1/policy/rules", strings.NewReader(`{"lane":"credential","pattern":"x","verdict":"auto"}`))
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusLocked {
			t.Fatalf("PUT locked status = %d, want 423", w.Code)
		}
	}

	// 4. PUT corrupt -> 503
	src.addErr = ErrPolicyCorrupt
	{
		req := httptest.NewRequest(http.MethodPut, "/v1/policy/rules", strings.NewReader(`{"lane":"credential","pattern":"x","verdict":"auto"}`))
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("PUT corrupt status = %d, want 503", w.Code)
		}
	}

	// 5. PUT validation error -> 400
	src.addErr = errors.New("invalid pattern")
	{
		req := httptest.NewRequest(http.MethodPut, "/v1/policy/rules", strings.NewReader(`{"lane":"credential","pattern":"x","verdict":"auto"}`))
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PUT validation status = %d, want 400", w.Code)
		}
		if !strings.Contains(w.Body.String(), `"error":"invalid pattern"`) {
			t.Errorf("body = %q, want invalid pattern", w.Body.String())
		}
	}
	src.addErr = nil

	// 6. DELETE locked -> 423
	src.removeErr = ErrPolicyLocked
	{
		req := httptest.NewRequest(http.MethodDelete, "/v1/policy/rules/x", nil)
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusLocked {
			t.Fatalf("DELETE locked status = %d, want 423", w.Code)
		}
	}

	// 7. DELETE corrupt -> 503
	src.removeErr = ErrPolicyCorrupt
	{
		req := httptest.NewRequest(http.MethodDelete, "/v1/policy/rules/x", nil)
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("DELETE corrupt status = %d, want 503", w.Code)
		}
	}

	// 8. DELETE other error -> 400
	src.removeErr = errors.New("delete failed")
	{
		req := httptest.NewRequest(http.MethodDelete, "/v1/policy/rules/x", nil)
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("DELETE err status = %d, want 400", w.Code)
		}
		if !strings.Contains(w.Body.String(), `"error":"delete failed"`) {
			t.Errorf("body = %q, want delete failed", w.Body.String())
		}
	}
}

func TestPolicy_MethodNotAllowed(t *testing.T) {
	src := &fakePolicySource{}
	tok, handler := setupPolicyTestServer(t, src)

	// POST /v1/policy/rules -> 405
	{
		req := httptest.NewRequest(http.MethodPost, "/v1/policy/rules", nil)
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST /v1/policy/rules status = %d, want 405", w.Code)
		}
	}

	// DELETE /v1/policy/rules -> 405
	{
		req := httptest.NewRequest(http.MethodDelete, "/v1/policy/rules", nil)
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("DELETE /v1/policy/rules status = %d, want 405", w.Code)
		}
	}

	// GET /v1/policy/rules/demo%2fecho -> 405
	{
		req := httptest.NewRequest(http.MethodGet, "/v1/policy/rules/demo%2fecho", nil)
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET /v1/policy/rules/{pattern} status = %d, want 405", w.Code)
		}
	}

	// PUT /v1/policy/rules/demo%2fecho -> 405
	{
		req := httptest.NewRequest(http.MethodPut, "/v1/policy/rules/demo%2fecho", nil)
		req.Host = "127.0.0.1:7717"
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("PUT /v1/policy/rules/{pattern} status = %d, want 405", w.Code)
		}
	}
}

func TestPolicy_AuthRequired(t *testing.T) {
	src := &fakePolicySource{}
	_, handler := setupPolicyTestServer(t, src)

	req := httptest.NewRequest(http.MethodGet, "/v1/policy/rules", nil)
	req.Host = "127.0.0.1:7717"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}
