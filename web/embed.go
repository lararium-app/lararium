package web

import (
	"embed"
	"io/fs"
)

// distFS embeds the production Vite build in dist/.
//
//go:embed dist
var distFS embed.FS

// FS is the embedded page bundle subtree served by the surface at /
// and under /assets/*.
var FS fs.FS

func init() {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	FS = sub
}
