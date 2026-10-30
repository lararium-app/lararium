package surface

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// credTestValue is a distinctive fixture secret (V-T1b).
const credTestValue = "fx-DISTINCTIVE-fixture-7f3a9c1e5b2d"

const credTestHandle = "hdl_abcdefghijklmnopqrstuv"

// fakeCredSource is an in-memory CredentialSource with no custos import. It
// counts calls so tests can prove the gate ran before any vault access.
type fakeCredSource struct {
	mu      sync.Mutex
	calls   int
	rows    []CredentialInfo
	locked  bool
	lastReq CredentialCreate
}

func (f *fakeCredSource) List() ([]CredentialInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.locked {
		return nil, ErrLocked
	}
	return append([]CredentialInfo(nil), f.rows...), nil
}

func (f *fakeCredSource) Create(req CredentialCreate) (CredentialInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastReq = req
	if f.locked {
		return CredentialInfo{}, ErrLocked
	}
	if req.Scope != "" && !strings.HasPrefix(req.Scope, req.Name) {
		return CredentialInfo{}, &CredentialFieldError{Field: "scope"}
	}
	info := CredentialInfo{
		Handle: credTestHandle, Name: req.Name, Kind: "api_key",
		Last4:   req.Secret[len(req.Secret)-4:],
		Created: time.Now().UTC().Format(time.RFC3339),
	}
	if req.TTLSeconds > 0 {
		e := time.Now().UTC().Add(time.Duration(req.TTLSeconds) * time.Second).Format(time.RFC3339)
		info.Expires = &e
	}
	if req.Rate != nil {
		r := *req.Rate
		info.Rate = &r
	}
	f.rows = append(f.rows, info)
	return info, nil
}

func (f *fakeCredSource) Revoke(handle string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.locked {
		return ErrLocked
	}
	for i, r := range f.rows {
		if r.Handle == handle {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return nil
		}
	}
	return ErrCredentialNotFound
}

func setupCredServer(t *testing.T, src CredentialSource) (string, http.Handler) {
	t.Helper()
	store, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := store.Create("cred-unit-test")
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
		srv.SetCredentialSource(src)
	}
	return tok, NewServer(srv)
}

func credDo(h http.Handler, method, path, tok, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:7717"
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// C1: missing/invalid bearer token → the existing 401 shape on all three
// routes, with no vault access. (No scope model exists; see credentials.go.)
func TestCredentials_C1_BearerGate(t *testing.T) {
	src := &fakeCredSource{}
	_, h := setupCredServer(t, src)
	routes := []struct{ method, path, body string }{
		{http.MethodGet, "/v1/credentials", ""},
		{http.MethodPost, "/v1/credentials", `{"name":"svc","secret":"` + credTestValue + `"}`},
		{http.MethodDelete, "/v1/credentials/" + credTestHandle, ""},
	}
	for _, rt := range routes {
		for _, tok := range []string{"", "not-a-real-token"} {
			w := credDo(h, rt.method, rt.path, tok, rt.body)
			if w.Code != http.StatusUnauthorized || strings.TrimSpace(w.Body.String()) != `{"error":"unauthorized"}` {
				t.Fatalf("%s %s tok=%q: %d %q", rt.method, rt.path, tok, w.Code, w.Body.String())
			}
		}
	}
	if src.calls != 0 {
		t.Fatalf("vault accessed %d times before auth", src.calls)
	}
}

// C2 + V-T1b: the create response carries handle + last4 and never the
// secret; nothing logged by the server contains it either.
func TestCredentials_C2_WriteOnly(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(log.Writer())
	src := &fakeCredSource{}
	tok, h := setupCredServer(t, src)
	w := credDo(h, http.MethodPost, "/v1/credentials", tok,
		`{"name":"svc","kind":"api_key","secret":"`+credTestValue+`","ttl_seconds":3600,"rate":{"n":5,"window_seconds":60}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), credTestValue) {
		t.Fatal("response leaks secret")
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["handle"] != credTestHandle || got["last4"] != "5b2d" || got["name"] != "svc" || len(got) != 7 {
		t.Fatalf("unexpected body %v", got)
	}
	if strings.Contains(logBuf.String(), credTestValue) {
		t.Fatal("log leaks secret")
	}
}

// C3: list fields; expires/rate null when unset, populated when set.
func TestCredentials_C3_List(t *testing.T) {
	src := &fakeCredSource{}
	tok, h := setupCredServer(t, src)
	credDo(h, http.MethodPost, "/v1/credentials", tok,
		`{"name":"svc","secret":"`+credTestValue+`","ttl_seconds":60,"rate":{"n":2,"window_seconds":30}}`)
	src.rows = append(src.rows, CredentialInfo{Handle: "hdl_zzzzzzzzzzzzzzzzzzzzzz", Name: "plain", Kind: "api_key", Last4: "abcd", Created: "2026-01-01T00:00:00Z"})
	w := credDo(h, http.MethodGet, "/v1/credentials", tok, "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), credTestValue) {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 2 {
		t.Fatalf("rows %v %v", rows, err)
	}
	if rows[0]["expires"] == nil || rows[0]["rate"] == nil || rows[0]["last4"] != "5b2d" {
		t.Fatalf("populated row wrong: %v", rows[0])
	}
	if v, ok := rows[1]["expires"]; !ok || v != nil {
		t.Fatalf("expires must be null: %v", rows[1])
	}
	if v, ok := rows[1]["rate"]; !ok || v != nil {
		t.Fatalf("rate must be null: %v", rows[1])
	}
	empty := &fakeCredSource{}
	tok2, h2 := setupCredServer(t, empty)
	if w := credDo(h2, http.MethodGet, "/v1/credentials", tok2, ""); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("empty list should be []: %s", w.Body.String())
	}
}

// C4: DELETE revokes; list omits it; second DELETE → not found.
func TestCredentials_C4_Revoke(t *testing.T) {
	src := &fakeCredSource{}
	tok, h := setupCredServer(t, src)
	credDo(h, http.MethodPost, "/v1/credentials", tok, `{"name":"svc","secret":"`+credTestValue+`"}`)
	path := "/v1/credentials/" + credTestHandle
	if w := credDo(h, http.MethodDelete, path, tok, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := credDo(h, http.MethodGet, "/v1/credentials", tok, ""); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("list after revoke: %s", w.Body.String())
	}
	w := credDo(h, http.MethodDelete, path, tok, "")
	if w.Code != http.StatusNotFound || strings.TrimSpace(w.Body.String()) != `{"error":"not found"}` {
		t.Fatalf("second delete: %d %s", w.Code, w.Body.String())
	}
	if w := credDo(h, http.MethodDelete, "/v1/credentials/bogus", tok, ""); w.Code != http.StatusNotFound {
		t.Fatalf("malformed handle: %d", w.Code)
	}
}

// C5: locked vault → frozen 423 {"error":"locked"} on all routes (nil source
// and locked source), no leak.
func TestCredentials_C5_Locked(t *testing.T) {
	for name, src := range map[string]CredentialSource{"nil": nil, "locked": &fakeCredSource{locked: true}} {
		tok, h := setupCredServer(t, src)
		for _, rt := range []struct{ method, path, body string }{
			{http.MethodGet, "/v1/credentials", ""},
			{http.MethodPost, "/v1/credentials", `{"name":"svc","secret":"` + credTestValue + `"}`},
			{http.MethodDelete, "/v1/credentials/" + credTestHandle, ""},
		} {
			w := credDo(h, rt.method, rt.path, tok, rt.body)
			if w.Code != http.StatusLocked || strings.TrimSpace(w.Body.String()) != `{"error":"locked"}` {
				t.Fatalf("%s %s %s: %d %s", name, rt.method, rt.path, w.Code, w.Body.String())
			}
		}
	}
}

// C6: malformed bodies → 4xx frozen shape naming fields, never values.
func TestCredentials_C6_Malformed(t *testing.T) {
	src := &fakeCredSource{}
	tok, h := setupCredServer(t, src)
	cases := map[string]string{
		`{"name":"svc"}`:                     "invalid field: secret",
		`{"secret":"` + credTestValue + `"}`: "invalid field: name",
		`{"name":"svc","secret":"` + credTestValue + `","scope":"other/x"}`:                 "invalid field: scope",
		`{"name":"svc","secret":"` + credTestValue + `","rate":{"n":0,"window_seconds":1}}`: "invalid field: rate",
		`{"name":"svc","secret":"` + credTestValue + `","ttl_seconds":-1}`:                  "invalid field: ttl_seconds",
		`{"name":"svc","secret":"` + credTestValue + `","kind":"oauth2"}`:                   "invalid field: kind",
		`{"name":"svc","secret":` + credTestValue + `}`:                                     "invalid request body",
	}
	for body, want := range cases {
		w := credDo(h, http.MethodPost, "/v1/credentials", tok, body)
		if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != `{"error":"`+want+`"}` {
			t.Fatalf("%s → %d %s (want %s)", body, w.Code, w.Body.String(), want)
		}
		if strings.Contains(w.Body.String(), credTestValue) {
			t.Fatal("error echoes secret")
		}
	}
	if len(src.rows) != 0 {
		t.Fatal("malformed create stored a row")
	}
}

func TestCredentials_ErrorMapping(t *testing.T) {
	s := &Server{}
	for err, code := range map[error]int{
		ErrCredentialExists: http.StatusConflict,
		ErrCredentialFull:   http.StatusConflict,
		errors.New("boom"):  http.StatusInternalServerError,
	} {
		w := httptest.NewRecorder()
		s.credentialsError(w, "create", err)
		if w.Code != code {
			t.Fatalf("%v → %d", err, w.Code)
		}
	}
}
