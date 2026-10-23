package custos

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestH8_TokenForwardRefusesRedirect: a 307 from the token endpoint must
// not be followed (client_secret + refresh_token ride in the POST body).
func TestH8_TokenForwardRefusesRedirect(t *testing.T) {
	var targetHits int
	var leakedForm string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		_ = r.ParseForm()
		leakedForm = r.Form.Encode()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"AT","expires_in":3600}`))
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/token", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	res, err := DefaultTokenForward(context.Background(), redirector.URL+"/token",
		url.Values{"client_secret": {"s3cret"}, "refresh_token": {"rt"}})
	if err == nil {
		t.Fatalf("expected failure on redirect, got result %+v", res)
	}
	var tse *tokenStatusError
	if !errors.As(err, &tse) || tse.status != http.StatusTemporaryRedirect {
		t.Fatalf("expected terminal 307 status error, got %v", err)
	}
	if targetHits != 0 {
		t.Fatalf("redirect target was reached %d time(s); form leaked: %q", targetHits, leakedForm)
	}
}

// TestH8_GmailBearerStopsAtRedirect: the refresh/API client must not
// replay Authorization to a redirect target — even when the client was
// injected through the test seam.
func TestH8_GmailBearerStopsAtRedirect(t *testing.T) {
	var targetAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/exfil", http.StatusFound)
	}))
	defer redirector.Close()

	g := NewGmailConnector(nil)
	g.SetBaseURLOverride(redirector.URL)
	// Deliberately inject a redirect-FOLLOWING client; do() must override it.
	g.SetHTTPClient(&http.Client{})

	req, err := http.NewRequest(http.MethodGet, redirector.URL+"/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer ya29.token")
	_, status, err := g.do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if status != http.StatusFound {
		t.Fatalf("expected terminal 302 surfaced, got %d", status)
	}
	if targetAuth != "" {
		t.Fatalf("Authorization replayed to redirect target: %q", targetAuth)
	}
}

// TestH8_MCPClientRefusesRedirect: the StreamableHTTP client carrying the
// connector bearer must be built with the refuse-redirect policy.
func TestH8_MCPHTTPRefusesRedirect(t *testing.T) {
	// Directly assert the policy function is the package law.
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	if err := refuseRedirect(req, []*http.Request{req}); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("refuseRedirect must return ErrUseLastResponse, got %v", err)
	}
	// And that an MCP HTTP connector's transport refuses a 301 mid-handshake.
	var targetAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusMovedPermanently)
	}))
	defer redirector.Close()

	rt := &mcpHTTPRoundTripper{base: http.DefaultTransport, bearer: "mcp-bearer"}
	client := &http.Client{Transport: rt, CheckRedirect: refuseRedirect}
	resp, err := client.Get(redirector.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("expected terminal 301, got %d", resp.StatusCode)
	}
	if targetAuth != "" {
		t.Fatalf("bearer replayed to redirect target: %q", targetAuth)
	}
}
