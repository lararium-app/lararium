package surface

import (
	"embed"
	"net/http"
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

// RegisterStatic wires the embedded web page (spec §7: exactly three
// files at /, /app.js, /app.css) with the hardened header set. These
// routes sit outside bearer auth — the page itself carries no secret —
// but still pass through the Host gate.
func RegisterStatic(mux *http.ServeMux, fs embed.FS) {
	asset := func(name, contentType string, exactPath bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if exactPath && r.URL.Path != "/" {
				notFound(w)
				return
			}
			data, err := fs.ReadFile(name)
			if err != nil {
				notFound(w)
				return
			}
			w.Header().Set("Content-Security-Policy", cspHeader)
			w.Header().Set("X-Content-Type-Options", nosniffHeader)
			w.Header().Set("Content-Type", contentType)
			if name != "index.html" {
				w.Header().Set("Cache-Control", "no-cache")
			}
			_, _ = w.Write(data)
		}
	}

	mux.HandleFunc("/", asset("index.html", "text/html; charset=utf-8", true))
	mux.HandleFunc("/app.js", asset("app.js", "text/javascript; charset=utf-8", false))
	mux.HandleFunc("/app.css", asset("app.css", "text/css; charset=utf-8", false))
}
