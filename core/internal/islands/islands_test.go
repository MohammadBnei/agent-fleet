package islands

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// docs/adr/0068 §3: the entry revalidates, chunks are immutable, everything is
// readable cross-origin, and nothing outside the bundle is reachable.
func TestHandler(t *testing.T) {
	h := handler("/islands/", fstest.MapFS{
		"fleet-agent.js":  {Data: []byte("entry")},
		"chunk-abc123.js": {Data: []byte("chunk")},
		".gitkeep":        {Data: nil},
		"sub/x.js":        {Data: []byte("x")},
	})
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	rec := get("/islands/fleet-agent.js")
	if rec.Code != http.StatusOK || rec.Body.String() != "entry" || rec.Header().Get("Cache-Control") != "no-cache" ||
		rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Errorf("entry: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	rec = get("/islands/chunk-abc123.js")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("chunk: %d %v", rec.Code, rec.Header())
	}
	for _, p := range []string{"/islands/", "/islands/sub/", "/islands/.gitkeep", "/islands/../go.mod", "/islands/%2e%2e/go.mod", "/islands/missing.js"} {
		if rec := get(p); rec.Code != http.StatusNotFound && rec.Code != http.StatusMovedPermanently {
			t.Errorf("%s: %d, want 404", p, rec.Code)
		} else if rec.Code == http.StatusOK {
			t.Errorf("%s served", p)
		}
	}
}
