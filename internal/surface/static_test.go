package surface

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestRegisterStatic(t *testing.T) {
	mockFS := fstest.MapFS{
		"index.html": &fstest.MapFile{
			Data: []byte("<!DOCTYPE html><html><body><h1>Lararium</h1></body></html>"),
		},
		"assets/bundle.js": &fstest.MapFile{
			Data: []byte("console.log('test bundle');"),
		},
		"assets/style.css": &fstest.MapFile{
			Data: []byte("body { color: red; }"),
		},
		"assets/logo.svg": &fstest.MapFile{
			Data: []byte("<svg></svg>"),
		},
	}

	mux := http.NewServeMux()
	RegisterStatic(mux, mockFS)

	tests := []struct {
		name         string
		method       string
		path         string
		wantStatus   int
		wantCSP      bool
		wantNosniff  bool
		wantCache    string
		wantType     string
		wantContains string
	}{
		{
			name:         "root serves index.html",
			method:       http.MethodGet,
			path:         "/",
			wantStatus:   http.StatusOK,
			wantCSP:      true,
			wantNosniff:  true,
			wantCache:    "",
			wantType:     "text/html; charset=utf-8",
			wantContains: "<h1>Lararium</h1>",
		},
		{
			name:         "explicit index.html",
			method:       http.MethodGet,
			path:         "/index.html",
			wantStatus:   http.StatusOK,
			wantCSP:      true,
			wantNosniff:  true,
			wantCache:    "",
			wantType:     "text/html; charset=utf-8",
			wantContains: "<h1>Lararium</h1>",
		},
		{
			name:         "nested asset js",
			method:       http.MethodGet,
			path:         "/assets/bundle.js",
			wantStatus:   http.StatusOK,
			wantCSP:      true,
			wantNosniff:  true,
			wantCache:    "no-cache",
			wantType:     "text/javascript; charset=utf-8",
			wantContains: "console.log('test bundle');",
		},
		{
			name:         "nested asset css",
			method:       http.MethodGet,
			path:         "/assets/style.css",
			wantStatus:   http.StatusOK,
			wantCSP:      true,
			wantNosniff:  true,
			wantCache:    "no-cache",
			wantType:     "text/css; charset=utf-8",
			wantContains: "body { color: red; }",
		},
		{
			name:         "nested asset svg",
			method:       http.MethodGet,
			path:         "/assets/logo.svg",
			wantStatus:   http.StatusOK,
			wantCSP:      true,
			wantNosniff:  true,
			wantCache:    "no-cache",
			wantType:     "image/svg+xml",
			wantContains: "<svg></svg>",
		},
		{
			name:         "app.js compatibility alias",
			method:       http.MethodGet,
			path:         "/app.js",
			wantStatus:   http.StatusOK,
			wantCSP:      true,
			wantNosniff:  true,
			wantCache:    "no-cache",
			wantType:     "text/javascript; charset=utf-8",
			wantContains: "console.log('test bundle');",
		},
		{
			name:         "app.css compatibility alias",
			method:       http.MethodGet,
			path:         "/app.css",
			wantStatus:   http.StatusOK,
			wantCSP:      true,
			wantNosniff:  true,
			wantCache:    "no-cache",
			wantType:     "text/css; charset=utf-8",
			wantContains: "body { color: red; }",
		},
		{
			name:         "missing asset 404",
			method:       http.MethodGet,
			path:         "/assets/missing.js",
			wantStatus:   http.StatusNotFound,
			wantContains: `{"error":"not found"}`,
		},
		{
			name:         "post method rejected",
			method:       http.MethodPost,
			path:         "/",
			wantStatus:   http.StatusNotFound,
			wantContains: `{"error":"not found"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			resp := w.Result()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status = %d; want %d", resp.StatusCode, tt.wantStatus)
			}

			if tt.wantCSP {
				csp := resp.Header.Get("Content-Security-Policy")
				if csp != cspHeader {
					t.Errorf("CSP = %q; want %q", csp, cspHeader)
				}
			}

			if tt.wantNosniff {
				nosniff := resp.Header.Get("X-Content-Type-Options")
				if nosniff != nosniffHeader {
					t.Errorf("X-Content-Type-Options = %q; want %q", nosniff, nosniffHeader)
				}
			}

			if tt.wantType != "" {
				ct := resp.Header.Get("Content-Type")
				if ct != tt.wantType {
					t.Errorf("Content-Type = %q; want %q", ct, tt.wantType)
				}
			}

			if tt.wantCache != "" {
				cc := resp.Header.Get("Cache-Control")
				if cc != tt.wantCache {
					t.Errorf("Cache-Control = %q; want %q", cc, tt.wantCache)
				}
			}

			if tt.wantContains != "" && !stringContains(string(body), tt.wantContains) {
				t.Errorf("body %q does not contain %q", string(body), tt.wantContains)
			}
		})
	}
}

func stringContains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || (len(s) > 0 && indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
