package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestWebBrowse_ArgValidation(t *testing.T) {
	cfg := DefaultConfig()
	tool := WebBrowseTool(t.TempDir(), cfg)

	tests := []struct {
		name     string
		argsJSON string
		wantErr  string
	}{
		{
			name:     "invalid JSON",
			argsJSON: `{"url":`,
			wantErr:  "bad args:",
		},
		{
			name:     "missing url",
			argsJSON: `{"goal":"find docs"}`,
			wantErr:  "url is required",
		},
		{
			name:     "empty url",
			argsJSON: `{"url":"   "}`,
			wantErr:  "url is required",
		},
		{
			name:     "unsupported scheme",
			argsJSON: `{"url":"ftp://example.com/test"}`,
			wantErr:  "unsupported URL scheme",
		},
		{
			name:     "ssrf loopback blocked",
			argsJSON: `{"url":"http://127.0.0.1/test"}`,
			wantErr:  "ssrf: blocked destination IP",
		},
		{
			name:     "ssrf localhost blocked",
			argsJSON: `{"url":"http://localhost:8080/test"}`,
			wantErr:  "ssrf:",
		},
		{
			name:     "ssrf cloud metadata blocked",
			argsJSON: `{"url":"http://169.254.169.254/latest"}`,
			wantErr:  "ssrf:",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Run(context.Background(), tc.argsJSON)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestWebBrowse_BusyMutex(t *testing.T) {
	cfg := DefaultConfig()
	dataDir := t.TempDir()
	browser := NewBrowser(dataDir, cfg)

	// Hold mutex to simulate concurrent browse in flight
	browser.mu.Lock()

	res, err := browser.Browse(context.Background(), "https://example.com", "goal")
	browser.mu.Unlock()

	if err != nil {
		t.Fatalf("expected nil error on busy, got: %v", err)
	}
	if res != ErrBrowserBusy {
		t.Fatalf("res = %q, want %q", res, ErrBrowserBusy)
	}
}

func TestWebBrowse_UnavailableWhenNoBrowserInstalled(t *testing.T) {
	// Set explicit nonexistent chrome path
	cfg := Config{
		Enabled:        true,
		BrowseTimeoutS: 30,
		MaxTextBytes:   DefaultMaxTextBytes,
		ChromePath:     "/nonexistent/browser/binary",
	}

	browser := NewBrowser(t.TempDir(), cfg)
	res, err := browser.Browse(context.Background(), "https://example.com", "inspect")
	if err != nil {
		t.Fatalf("expected nil error on unavailable browser, got: %v", err)
	}
	if res != ErrBrowserUnavailable {
		t.Fatalf("res = %q, want %q", res, ErrBrowserUnavailable)
	}
}

func TestWebBrowse_RealBrowser(t *testing.T) {
	chrome := strings.TrimSpace(os.Getenv("LARARIUM_CHROME"))
	if chrome == "" {
		t.Skip("skipping real-browser test; LARARIUM_CHROME not set")
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!DOCTYPE html>
<html>
<head><title>Browser Dynamic Test</title></head>
<body>
<h1>Dynamic Header</h1>
<div id="content">Rendered content</div>
<a href="/sub">Sublink</a>
</body>
</html>`))
	}))
	defer ts.Close()

	cfg := Config{
		Enabled:        true,
		BrowseTimeoutS: 15,
		MaxTextBytes:   50000,
		ChromePath:     chrome,
	}

	dataDir := t.TempDir()
	browser := NewBrowser(dataDir, cfg)

	res, err := browser.Browse(context.Background(), ts.URL, "read page")
	if err != nil {
		t.Fatalf("real browser failed: %v", err)
	}
	if !strings.Contains(res, "Browser Dynamic Test") {
		t.Errorf("missing title in browser output: %s", res)
	}
	if !strings.Contains(res, "Rendered content") {
		t.Errorf("missing rendered text in browser output: %s", res)
	}
}
