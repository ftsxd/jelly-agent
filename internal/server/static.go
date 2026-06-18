package server

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// spaHandler serves the embedded single-page app: real files are served as-is
// (with cache headers for hashed assets), and any unknown non-API path falls
// back to index.html so client-side routing works on deep links / reloads.
func (s *Server) spaHandler() http.Handler {
	fileServer := http.FileServer(http.FS(s.static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if upath == "" {
			upath = "index.html"
		}
		if f, err := s.static.Open(upath); err == nil {
			f.Close()
			// Vite emits content-hashed filenames under assets/, safe to cache hard.
			if strings.HasPrefix(upath, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}
		serveIndex(w, s.static)
	})
}

// serveIndex writes index.html as the SPA entry (200, no-cache so deploys take
// effect immediately).
func serveIndex(w http.ResponseWriter, fsys fs.FS) {
	f, err := fsys.Open("index.html")
	if err != nil {
		http.Error(w, "frontend not built", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}
