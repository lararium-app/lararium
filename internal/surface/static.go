package surface

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

const (
	cspHeader     = "default-src 'self'; style-src 'self'; frame-ancestors 'none'"
	nosniffHeader = "nosniff"
)

// notFound answers with the frozen JSON error shape.
func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"not found"}`))
}

func mimeFor(p string) string {
	switch path.Ext(p) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".ico":
		return "image/x-icon"
	case ".json":
		return "application/json"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	default:
		return "application/octet-stream"
	}
}

// RegisterStatic wires the embedded web dist subtree with the hardened header set.
// These routes sit outside bearer auth — the page itself carries no secret —
// but still pass through the Host gate. It serves index.html at /, and dist
// assets generically (e.g. under /assets/*) with correct content-type, CSP,
// and nosniff headers.
func RegisterStatic(mux *http.ServeMux, fsys fs.FS) {
	serveFile := func(w http.ResponseWriter, filePath string) {
		clean := path.Clean(filePath)
		if strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, "/") {
			notFound(w)
			return
		}
		data, err := fs.ReadFile(fsys, clean)
		if err != nil {
			// Compatibility fallback for /app.js and /app.css:
			if clean == "app.js" || clean == "app.css" {
				ext := path.Ext(clean)
				if entries, rerr := fs.ReadDir(fsys, "assets"); rerr == nil {
					for _, e := range entries {
						if !e.IsDir() && path.Ext(e.Name()) == ext {
							clean = path.Join("assets", e.Name())
							data, err = fs.ReadFile(fsys, clean)
							break
						}
					}
				}
			}
			if err != nil {
				notFound(w)
				return
			}
		}

		w.Header().Set("Content-Security-Policy", cspHeader)
		w.Header().Set("X-Content-Type-Options", nosniffHeader)
		w.Header().Set("Content-Type", mimeFor(clean))
		if clean != "index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		//nolint:gosec // G705: data is a static build artifact read from the
		// embedded dist fs.FS, never derived from the request.
		_, _ = w.Write(data)
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			notFound(w)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" || p == "index.html" {
			serveFile(w, "index.html")
			return
		}
		serveFile(w, p)
	})
}
