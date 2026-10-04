package surface

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lararium-app/lararium/internal/keystore"
)

// KEYS-SPEC K4: /v1/keys routes. Tests drive s.dispatch directly —
// bearer + Host gates are covered by the existing surface tests and
// apply unchanged (dispatch sits behind BearerAuth).

type keysFixture struct {
	srv      *Server
	store    *keystore.Store
	reg      *keystore.Registry
	reloaded int
}

func newKeysFixture(t *testing.T, provs []keystore.ProviderInfo) *keysFixture {
	t.Helper()
	kx := &keysFixture{store: keystore.New(t.TempDir())}
	kx.reg = keystore.NewRegistry(nil)
	kx.srv = &Server{Store: mustTokens(t), Sessions: &fakeSessionSource{}, Hub: &fakeHub{}}
	kx.srv.AttachKeys(&KeysDeps{
		Store:     kx.store,
		Registry:  kx.reg,
		Providers: func() []keystore.ProviderInfo { return provs },
		Reload: func() error {
			m, err := kx.store.Read()
			if err != nil {
				return err
			}
			kx.reg.Swap(m)
			kx.reloaded++
			return nil
		},
	})
	return kx
}

func mustTokens(t *testing.T) *TokenStore {
	t.Helper()
	s, err := OpenTokenStore(t.TempDir() + "/tokens.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (kx *keysFixture) do(method, target, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	kx.srv.dispatch(w, r)
	return w
}

func TestKeysRoutesAbsentIs404(t *testing.T) {
	srv := &Server{Store: mustTokens(t), Sessions: &fakeSessionSource{}, Hub: &fakeHub{}}
	w := httptest.NewRecorder()
	srv.dispatch(w, httptest.NewRequest(http.MethodGet, "/v1/keys", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("unattached GET /v1/keys = %d, want 404", w.Code)
	}
}

func TestKeysGetEmptyList(t *testing.T) {
	kx := newKeysFixture(t, nil)
	w := kx.do(http.MethodGet, "/v1/keys", "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var rows []keyRow
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %v, want empty", rows)
	}
}

func TestKeysGetStatusAndFingerprint(t *testing.T) {
	t.Setenv("LARARIUM_TEST_ENV_KEY", "envelope-secret")
	provs := []keystore.ProviderInfo{
		{Name: "envprov", EnvVar: "LARARIUM_TEST_ENV_KEY"},
		{Name: "cfgprov", Literal: "literal-secret"},
		{Name: "emptyprov"},
	}
	kx := newKeysFixture(t, provs)
	if err := kx.store.Set("stored", "sk-stored-value"); err != nil {
		t.Fatal(err)
	}

	w := kx.do(http.MethodGet, "/v1/keys", "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body)
	}
	var rows []keyRow
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	byName := map[string]keyRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d (%v), want 4 (config ∪ keys.json)", len(rows), rows)
	}
	if got := byName["envprov"].Status; got != "set (env)" {
		t.Errorf("envprov status = %q", got)
	}
	if got := byName["cfgprov"].Status; got != "set (config)" {
		t.Errorf("cfgprov status = %q", got)
	}
	if got := byName["stored"].Status; got != "set (keys.json)" {
		t.Errorf("stored status = %q", got)
	}
	if got := byName["emptyprov"].Status; got != "missing" {
		t.Errorf("emptyprov status = %q", got)
	}
	// Fingerprint: first 8 hex of sha256(value), display-only.
	sum := sha256.Sum256([]byte("sk-stored-value"))
	if got := byName["stored"].SHA256; got != hex.EncodeToString(sum[:4]) {
		t.Errorf("stored sha256_8 = %q, want %s", got, hex.EncodeToString(sum[:4]))
	}
	if byName["emptyprov"].SHA256 != "" {
		t.Errorf("missing key has fingerprint %q", byName["emptyprov"].SHA256)
	}
	// Never a value.
	if strings.Contains(w.Body.String(), "sk-stored-value") ||
		strings.Contains(w.Body.String(), "literal-secret") ||
		strings.Contains(w.Body.String(), "envelope-secret") {
		t.Errorf("GET leaked key material: %s", w.Body)
	}
}

func TestKeysPutUnknownProvider(t *testing.T) {
	kx := newKeysFixture(t, []keystore.ProviderInfo{{Name: "known"}})

	w := kx.do(http.MethodPut, "/v1/keys/novel", `{"key":"sk-x"}`)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "unknown provider") {
		t.Fatalf("unknown provider: %d %s", w.Code, w.Body)
	}
	// force=true admits a never-seen name.
	w = kx.do(http.MethodPut, "/v1/keys/novel?force=true", `{"key":"sk-x"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("force PUT: %d %s", w.Code, w.Body)
	}
	// Once in keys.json, plain PUT (drawer re-save) works without force.
	w = kx.do(http.MethodPut, "/v1/keys/novel", `{"key":"sk-y"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("re-save PUT: %d %s", w.Code, w.Body)
	}
	// Config provider: plain PUT.
	w = kx.do(http.MethodPut, "/v1/keys/known", `{"key":"sk-z"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("config PUT: %d %s", w.Code, w.Body)
	}
}

func TestKeysPutValidationMatrix(t *testing.T) {
	kx := newKeysFixture(t, []keystore.ProviderInfo{{Name: "known"}})

	cases := []struct {
		name, method, target, body string
		want                       int
		wantMsg                    string
	}{
		{"bad name", http.MethodPut, "/v1/keys/Bad%20Name", `{"key":"sk-x"}`, http.StatusNotFound, "invalid provider name"},
		{"dotdot", http.MethodPut, "/v1/keys/a%2eb", `{"key":"sk-x"}`, 0, ""}, // mux-level: any 4xx/3xx
		{"empty key", http.MethodPut, "/v1/keys/known", `{"key":"   "}`, http.StatusBadRequest, "empty key"},
		{"no key field", http.MethodPut, "/v1/keys/known", `{"nope":"x"}`, http.StatusBadRequest, "body must be"},
		{"not json", http.MethodPut, "/v1/keys/known", `sk-raw`, http.StatusBadRequest, "body must be"},
		{"too large", http.MethodPut, "/v1/keys/known", `{"key":"` + strings.Repeat("x", 5000) + `"}`, http.StatusRequestEntityTooLarge, "key too large"},
	}
	for _, tc := range cases {
		w := kx.do(tc.method, tc.target, tc.body)
		if tc.want == 0 {
			if w.Code < 400 {
				t.Errorf("%s: code = %d, want any 4xx/3xx", tc.name, w.Code)
			}
			continue
		}
		if w.Code != tc.want || !strings.Contains(w.Body.String(), tc.wantMsg) {
			t.Errorf("%s: %d %q, want %d containing %q", tc.name, w.Code, w.Body.String(), tc.want, tc.wantMsg)
		}
	}
	// Bad name must not write, regardless of body.
	if m, _ := kx.store.Read(); len(m) != 0 {
		t.Errorf("rejected PUTs wrote to the store: %v", m)
	}
}

func TestKeysPutCap(t *testing.T) {
	kx := newKeysFixture(t, nil)
	for i := range keystore.MaxKeys {
		if err := kx.store.Set(fmt.Sprintf("k%02d", i), "sk-v"); err != nil {
			t.Fatal(err)
		}
	}
	w := kx.do(http.MethodPut, "/v1/keys/one-more?force=true", `{"key":"sk-x"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "key store full") {
		t.Fatalf("cap: %d %s", w.Code, w.Body)
	}
	// Overwrite at cap is always allowed.
	w = kx.do(http.MethodPut, "/v1/keys/k00", `{"key":"sk-new"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("overwrite at cap: %d %s", w.Code, w.Body)
	}
}

func TestKeysPutReloadsInProcess(t *testing.T) {
	kx := newKeysFixture(t, []keystore.ProviderInfo{{Name: "known"}})
	before := kx.reloaded
	if w := kx.do(http.MethodPut, "/v1/keys/known", `{"key":"sk-live"}`); w.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d", w.Code)
	}
	if kx.reloaded != before+1 {
		t.Errorf("registry reload count = %d, want %d", kx.reloaded, before+1)
	}
	if kx.reg.Key("known") != "sk-live" {
		t.Errorf("registry not swapped: %q", kx.reg.Key("known"))
	}
}

func TestKeysDeleteLifecycle(t *testing.T) {
	// Config provider: delete leaves the row, status flips to missing.
	kx := newKeysFixture(t, []keystore.ProviderInfo{{Name: "cfgprov", Literal: ""}})
	if err := kx.store.Set("cfgprov", "sk-v"); err != nil {
		t.Fatal(err)
	}
	w := kx.do(http.MethodDelete, "/v1/keys/cfgprov", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d", w.Code)
	}
	rows := kx.srv.keyRows()
	if len(rows) != 1 || rows[0].Status != "missing" {
		t.Errorf("after delete: %+v, want one missing row", rows)
	}

	// Force-only key: delete removes it from the list entirely.
	kx2 := newKeysFixture(t, nil)
	if err := kx2.store.Set("ghost", "sk-v"); err != nil {
		t.Fatal(err)
	}
	if w := kx2.do(http.MethodDelete, "/v1/keys/ghost", ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE ghost: %d", w.Code)
	}
	if rows := kx2.srv.keyRows(); len(rows) != 0 {
		t.Errorf("ghost survived delete: %+v", rows)
	}

	// Idempotent: absent name is still success.
	if w := kx2.do(http.MethodDelete, "/v1/keys/never", ""); w.Code != http.StatusNoContent {
		t.Errorf("DELETE absent: %d, want 204", w.Code)
	}
	// Bad name: 404 with the frozen message.
	w = kx2.do(http.MethodDelete, "/v1/keys/Bad%20Name", "")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "invalid provider name") {
		t.Errorf("DELETE bad name: %d %s", w.Code, w.Body)
	}
}

func TestKeysGetAfterEnvAndDelete(t *testing.T) {
	// V5: env present -> "set (env)" even after keys.json delete.
	t.Setenv("LARARIUM_TEST_V5", "env-wins")
	kx := newKeysFixture(t, []keystore.ProviderInfo{{Name: "vp", EnvVar: "LARARIUM_TEST_V5"}})
	if err := kx.store.Set("vp", "sk-v"); err != nil {
		t.Fatal(err)
	}
	if w := kx.do(http.MethodDelete, "/v1/keys/vp", ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d", w.Code)
	}
	rows := kx.srv.keyRows()
	if len(rows) != 1 || rows[0].Status != "set (env)" {
		t.Errorf("rows = %+v, want one 'set (env)'", rows)
	}
}

func TestKeysMethodNotAllowed(t *testing.T) {
	kx := newKeysFixture(t, nil)
	if w := kx.do(http.MethodPost, "/v1/keys", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/keys = %d", w.Code)
	}
	if w := kx.do(http.MethodGet, "/v1/keys/known", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/keys/x = %d", w.Code)
	}
}
