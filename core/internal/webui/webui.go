// Package webui serves the built dashboard SPA (dashboard/, built by the
// "spa" Dockerfile stage into this package's dist/ directory — go:embed
// can only reach files inside its own package's directory tree, the same
// constraint internal/db/migrate.go already works within for the
// embedded schema). See docs/adr/0013.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var distFS embed.FS

// Handler serves the SPA's static assets, falling back to index.html for
// any path that isn't a real file so client-side routing (e.g. a
// bookmarked ?task=<id> deep link) still resolves.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("webui: dist not embedded: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fs.Stat(sub, trimLeadingSlash(r.URL.Path)); err != nil {
			r = cloneWithPath(r, "/")
		}
		fileServer.ServeHTTP(w, r)
	})
}

func trimLeadingSlash(p string) string {
	if p == "" || p == "/" {
		return "."
	}
	if p[0] == '/' {
		return p[1:]
	}
	return p
}

func cloneWithPath(r *http.Request, path string) *http.Request {
	clone := r.Clone(r.Context())
	clone.URL.Path = path
	return clone
}
