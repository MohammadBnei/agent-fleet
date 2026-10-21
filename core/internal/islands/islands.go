// Package islands serves the island bundles (islands/, built by the
// "islands" Dockerfile stage into this package's dist/) at /islands/
// (docs/adr/0068 §3). Static files, no data, no SPA fallback: a missing file
// is a 404.
package islands

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// entry is the one stable URL host pages load. It must reach hosts at once
// after a release or rollback, so it is revalidated every time; the chunks it
// imports are content-hashed and never change.
const entry = "fleet-agent.js"

// Handler serves dist under prefix. Module scripts load in CORS mode from host
// pages, and nothing here is private, so any origin may read it.
func Handler(prefix string) http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("islands: dist not embedded: " + err.Error())
	}
	return handler(prefix, sub)
}

func handler(prefix string, fsys fs.FS) http.Handler {
	files := http.StripPrefix(strings.TrimSuffix(prefix, "/"), http.FileServer(http.FS(fsys)))
	// embed.FS has no modtimes, so without an ETag the no-cache entry would
	// be downloaded in full on every page load instead of revalidated.
	etags := map[string]string{}
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if b, err := fs.ReadFile(fsys, p); err == nil {
			sum := sha256.Sum256(b)
			etags[p] = `"` + hex.EncodeToString(sum[:8]) + `"`
		}
		return nil
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, prefix)
		// No directory listings, no dotfiles (.gitkeep).
		if name == "" || strings.HasSuffix(name, "/") || strings.HasPrefix(name, ".") {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("X-Content-Type-Options", "nosniff")
		if tag, ok := etags[name]; ok {
			h.Set("ETag", tag) // http.FileServer answers If-None-Match from it
		}
		if name == entry {
			h.Set("Cache-Control", "no-cache")
		} else {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}
