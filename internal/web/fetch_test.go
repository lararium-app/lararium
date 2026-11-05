package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebFetch_ExtractionAgainstFixtures(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/article":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			//nolint:gosec // static test fixture; no user input reaches the response
			w.Write([]byte(`<!DOCTYPE html>
<html>
<head>
    <title>Architecture Guide</title>
</head>
<body>
    <h1>Overview</h1>
    <p>This is a <strong>core component</strong> of the system.</p>
    <p>Relative same-origin link: <a href="/details">Details Page</a></p>
    <p>Absolute same-origin link: <a href="` + "http://" + r.Host + `/specs">Specs</a></p>
    <p>External link: <a href="https://external.example.com/docs">External Docs</a></p>
    <p>Fragment link: <a href="#heading">Jump</a></p>
    <p>Javascript link: <a href="javascript:void(0)">Do Nothing</a></p>
</body>
</html>`))

		case "/details":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<html><head><title>Details</title></head><body><p>Details content</p></body></html>`))

		case "/specs":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<html><head><title>Specs</title></head><body><p>Specs content</p></body></html>`))

		case "/binary-image":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})

		case "/binary-octet":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte{0x00, 0x01, 0x02, 0x03, 0xFF})

		case "/large":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			// 1000 repetitions of text to exceed standard caps
			var sb strings.Builder
			sb.WriteString("<html><head><title>Large Page</title></head><body>")
			for range 1000 {
				sb.WriteString("<p>Paragraph content number </p>\n")
			}
			sb.WriteString("</body></html>")
			w.Write([]byte(sb.String()))

		case "/api/data.json":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"status":"ok","items":[1,2,3]}`))

		case "/notes.txt":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte("Line 1 of plain notes\nLine 2 of plain notes"))

		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	host := ts.Listener.Addr().String()

	t.Run("HTML to markdown sane with title and same-origin links", func(t *testing.T) {
		cfg := DefaultConfig()
		res, err := Fetch(context.Background(), ts.URL+"/article", cfg, WithAllowedTestHost(host))
		if err != nil {
			t.Fatalf("Fetch failed: %v", err)
		}

		if !strings.Contains(res, "Title: Architecture Guide") {
			t.Errorf("missing expected title, got:\n%s", res)
		}
		if !strings.Contains(res, "URL: "+ts.URL+"/article") {
			t.Errorf("missing final URL, got:\n%s", res)
		}
		if !strings.Contains(res, "Overview") || !strings.Contains(res, "core component") {
			t.Errorf("missing markdown text, got:\n%s", res)
		}

		// Same-origin links must be present in the Links section
		linksSection := ""
		if idx := strings.Index(res, "\nLinks:\n"); idx != -1 {
			linksSection = res[idx:]
		} else {
			t.Fatalf("missing Links: section in output:\n%s", res)
		}

		if !strings.Contains(linksSection, ts.URL+"/details") {
			t.Errorf("missing relative same-origin link in Links section, got:\n%s", linksSection)
		}
		if !strings.Contains(linksSection, ts.URL+"/specs") {
			t.Errorf("missing absolute same-origin link in Links section, got:\n%s", linksSection)
		}

		// Non same-origin or invalid links must be excluded from Links section
		if strings.Contains(linksSection, "external.example.com") {
			t.Errorf("external link should not appear in same-origin links section, got:\n%s", linksSection)
		}
		if strings.Contains(linksSection, "javascript:") {
			t.Errorf("javascript link should not appear in links section, got:\n%s", linksSection)
		}
	})

	t.Run("Cap honored on large response", func(t *testing.T) {
		cfg := Config{
			Enabled:        true,
			BrowseTimeoutS: 30,
			MaxTextBytes:   150, // Small cap
		}
		res, err := Fetch(context.Background(), ts.URL+"/large", cfg, WithAllowedTestHost(host))
		if err != nil {
			t.Fatalf("Fetch failed: %v", err)
		}

		// Output has headers + capped markdown text
		// Verify that markdown text does not exceed MaxTextBytes
		parts := strings.Split(res, "\n\n")
		if len(parts) >= 2 {
			bodyText := parts[1]
			if idx := strings.Index(bodyText, "\n\nLinks:"); idx != -1 {
				bodyText = bodyText[:idx]
			}
			if len(bodyText) > cfg.MaxTextBytes {
				t.Fatalf("extracted text exceeded cap: len = %d, max = %d", len(bodyText), cfg.MaxTextBytes)
			}
		}
	})

	t.Run("Binary content-type image message", func(t *testing.T) {
		cfg := DefaultConfig()
		res, err := Fetch(context.Background(), ts.URL+"/binary-image", cfg, WithAllowedTestHost(host))
		if err != nil {
			t.Fatalf("Fetch failed: %v", err)
		}
		want := "binary content type image/png, not fetched"
		if res != want {
			t.Fatalf("res = %q, want %q", res, want)
		}
	})

	t.Run("Binary content-type octet-stream message", func(t *testing.T) {
		cfg := DefaultConfig()
		res, err := Fetch(context.Background(), ts.URL+"/binary-octet", cfg, WithAllowedTestHost(host))
		if err != nil {
			t.Fatalf("Fetch failed: %v", err)
		}
		want := "binary content type application/octet-stream, not fetched"
		if res != want {
			t.Fatalf("res = %q, want %q", res, want)
		}
	})

	t.Run("JSON content type returns text", func(t *testing.T) {
		cfg := DefaultConfig()
		res, err := Fetch(context.Background(), ts.URL+"/api/data.json", cfg, WithAllowedTestHost(host))
		if err != nil {
			t.Fatalf("Fetch failed: %v", err)
		}
		if !strings.Contains(res, `{"status":"ok"`) {
			t.Fatalf("missing JSON body, got: %s", res)
		}
		if !strings.Contains(res, "Content-Type: application/json") {
			t.Fatalf("missing Content-Type header, got: %s", res)
		}
	})

	t.Run("Plain text content type returns text", func(t *testing.T) {
		cfg := DefaultConfig()
		res, err := Fetch(context.Background(), ts.URL+"/notes.txt", cfg, WithAllowedTestHost(host))
		if err != nil {
			t.Fatalf("Fetch failed: %v", err)
		}
		if !strings.Contains(res, "Line 1 of plain notes") {
			t.Fatalf("missing plain text content, got: %s", res)
		}
	})
}

func TestWebFetch_RedirectHopLimit(t *testing.T) {
	hopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/r0":
			http.Redirect(w, r, "/r1", http.StatusFound)
		case "/r1":
			http.Redirect(w, r, "/r2", http.StatusFound)
		case "/r2":
			http.Redirect(w, r, "/r3", http.StatusFound)
		case "/r3":
			http.Redirect(w, r, "/r4", http.StatusFound)
		case "/r4":
			w.Write([]byte("done"))
		}
	}))
	defer hopServer.Close()

	host := hopServer.Listener.Addr().String()
	cfg := DefaultConfig()

	// 4 redirects should fail because max is 3 redirects
	_, err := Fetch(context.Background(), hopServer.URL+"/r0", cfg, WithAllowedTestHost(host))
	if err == nil {
		t.Fatal("expected error after 3 redirects, got nil")
	}
	if !strings.Contains(err.Error(), "stopped after 3 redirects") {
		t.Fatalf("expected 'stopped after 3 redirects', got: %v", err)
	}

	// 2 redirects should succeed
	res, err := Fetch(context.Background(), hopServer.URL+"/r2", cfg, WithAllowedTestHost(host))
	if err != nil {
		t.Fatalf("expected 2 redirects to succeed, got: %v", err)
	}
	if !strings.Contains(res, "done") {
		t.Fatalf("expected 'done', got: %s", res)
	}
}
