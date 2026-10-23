package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestH8_ProviderClientRefusesRedirect: the shared credential-bearing
// client must terminate at a 3xx, never replaying Authorization onward.
func TestH8_ProviderClientRefusesRedirect(t *testing.T) {
	var targetAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	req, err := http.NewRequest(http.MethodPost, redirector.URL+"/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-secret")
	resp, err := providerClient.Do(req)
	if err != nil {
		t.Fatalf("expected terminal 307, got transport error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("expected terminal 307, got %d", resp.StatusCode)
	}
	if targetAuth != "" {
		t.Fatalf("provider key replayed to redirect target: %q", targetAuth)
	}
}
