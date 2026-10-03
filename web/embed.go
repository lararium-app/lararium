// Package web holds the embedded single-page chat UI (spec §7: exactly
// index.html, app.js, app.css — vanilla, no build step, no CDN).
package web

import "embed"

// FS is the embedded page bundle served by the surface at /, /app.js,
// and /app.css.
//
//go:embed index.html app.js app.css
var FS embed.FS
