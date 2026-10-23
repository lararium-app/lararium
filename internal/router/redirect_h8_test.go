package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

// M11: router clients enforce ResponseHeaderTimeout 120s + DialContext timeout 10s
// while keeping H8 CheckRedirect intact; injected clients are wrapped copies.
func TestM11_RouterClientTimeouts(t *testing.T) {
	// Default client:
	if providerClient.CheckRedirect == nil {
		t.Fatal("providerClient missing CheckRedirect")
	}
	tr, ok := providerClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("providerClient Transport is %T, want *http.Transport", providerClient.Transport)
	}
	if tr.ResponseHeaderTimeout != 120*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v, want 120s", tr.ResponseHeaderTimeout)
	}
	if tr.DialContext == nil {
		t.Fatal("tr.DialContext is nil")
	}

	// Injected custom client wrapped:
	injected := &http.Client{
		Transport: &http.Transport{},
	}
	wrapped := WrapClient(injected)
	if wrapped == injected {
		t.Fatal("WrapClient must return a copy of injected client")
	}
	if wrapped.CheckRedirect == nil {
		t.Fatal("wrapped client missing CheckRedirect")
	}
	wtr, ok := wrapped.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("wrapped Transport is %T, want *http.Transport", wrapped.Transport)
	}
	if wtr.ResponseHeaderTimeout != 120*time.Second {
		t.Fatalf("wrapped ResponseHeaderTimeout = %v, want 120s", wtr.ResponseHeaderTimeout)
	}
	if wtr.DialContext == nil {
		t.Fatal("wrapped tr.DialContext is nil")
	}
}
