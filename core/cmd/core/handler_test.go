package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
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
		httpHandler(mux, gate, func(h http.Handler) http.Handler { return h }).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/abc", nil))
		if rec.Header().Get("Content-Security-Policy") != "frame-ancestors 'none'" || rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s (%d): frame headers missing: %v", name, rec.Code, rec.Header())
		}
	}
}

// docs/adr/0068 §3: an island's call refused by the gate still carries the
// CORS and frame headers, so the island reads Unauthenticated, not a network
// error.
func TestHTTPHandler_CORSOutsideTheGate(t *testing.T) {
	const agent = "/agentfleet.v1.AgentService/"
	mux := http.NewServeMux()
	refuse := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	}
	cors := auth.CORS(agent, "https://fleet.bnei.dev", func() []string { return []string{"https://opus.bnei.dev"} })
	r := httptest.NewRequest(http.MethodPost, agent+"ListThreads", nil)
	r.Header.Set("Origin", "https://opus.bnei.dev")
	rec := httptest.NewRecorder()
	httpHandler(mux, refuse, cors).ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("Access-Control-Allow-Origin") != "https://opus.bnei.dev" ||
		rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("%d %v", rec.Code, rec.Header())
	}
}
