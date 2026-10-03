package surface

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewServer(t *testing.T) {
	store, _ := OpenTokenStore(t.TempDir() + "/tokens.json")
	srv := &Server{Store: store, Sessions: &fakeSessionSource{}, Hub: &fakeHub{}, Cfg: ServeConfig{AllowedHosts: []string{"example.com"}}}

	mux := NewServer(srv)

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	req.Host = "localhost:7717"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("health status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestServerListenAndServeNormalizeError(t *testing.T) {
	store, _ := OpenTokenStore(t.TempDir() + "/tokens.json")
	srv := &Server{
		Store:    store,
		Sessions: &fakeSessionSource{},
		Hub:      &fakeHub{},
		Cfg:      ServeConfig{Listen: "0.0.0.0:7717"}, // non-loopback, no allowed_hosts
	}

	err := srv.ListenAndServe()
	if err == nil {
		t.Errorf("expected error for non-loopback without allowed_hosts, got nil")
	}
}

// Regression: an unmatched path must 404 through the full chain — the
// first wiring registered the mux inside itself and stack-overflowed.
func TestUnknownRouteJSON404(t *testing.T) {
	store, _ := OpenTokenStore(t.TempDir() + "/tokens.json")
	srv := &Server{Store: store, Sessions: &fakeSessionSource{}, Hub: &fakeHub{}}
	h := NewServer(srv)
	for _, path := range []string{"/nope", "/v2/things", "/app.txt"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "127.0.0.1:7717"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s content-type = %q, want application/json", path, ct)
		}
	}
}

// Regression: unknown-but-well-formed session must 404, not 200-empty.
func TestEventsUnknownSession404(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenTokenStore(dir + "/tokens.json")
	src := NewPenatusSource(dir, &fakeHub{})
	srv := &Server{Store: store, Sessions: src, Hub: &fakeHub{}}
	h := NewServer(srv)

	// mint a token so auth passes
	tok, err := store.Create("test")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/s_AAAAAAAAAAAAAAAAAAAAAAAAAA/events", nil)
	req.Host = "127.0.0.1:7717"
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown session events = %d, want 404 (body %s)", w.Code, w.Body.String())
	}
}
