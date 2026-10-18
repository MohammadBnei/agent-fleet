package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The composition run.go serves: frame headers on the mux's responses AND on
// a gate's own refusals (FrameDeny must sit outside the gate), with auth on or
// off (FLEET_AUTH_DISABLED=1 is the identity gate).
func TestHTTPHandler_FrameHeadersOutsideTheGate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	gates := map[string]func(http.Handler) http.Handler{
		"auth disabled": func(h http.Handler) http.Handler { return h },
		"gate refuses": func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
		},
	}
	for name, gate := range gates {
		rec := httptest.NewRecorder()
		httpHandler(mux, gate).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/abc", nil))
		if rec.Header().Get("Content-Security-Policy") != "frame-ancestors 'none'" || rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s (%d): frame headers missing: %v", name, rec.Code, rec.Header())
		}
	}
}
