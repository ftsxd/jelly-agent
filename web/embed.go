// Package web embeds the built Vue frontend (dist/) into the binary so the
// dashboard ships as a single executable. The build is produced by
// `npm run build` under web/; a placeholder dist keeps `go build` working
// before the frontend is built.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// DistFS returns the embedded frontend build rooted at dist/ (so "index.html"
// resolves directly). Returns nil if the build is empty/missing, letting the
// server fall back to API-only mode.
func DistFS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil
	}
	if _, err := sub.Open("index.html"); err != nil {
		return nil // not built yet
	}
	return sub
}
