package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// docs/adr/0068 §3: credentialed CORS on AgentService only, for the exact
// embed origins; the console passes; every other origin is refused before
// anything behind it runs.
func TestCORS(t *testing.T) {
	const agent = "/agentfleet.v1.AgentService/"
	reached := false
	h := CORS(agent, "https://fleet.bnei.dev", func() []string { return []string{"https://opus.bnei.dev"} })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = true
			http.Error(w, "unauthenticated", http.StatusUnauthorized) // stands in for the Gate
		}))
	do := func(method, path, origin string, preflight bool) *httptest.ResponseRecorder {
		reached = false
		r := httptest.NewRequest(method, path, nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if preflight {
			r.Header.Set("Access-Control-Request-Method", "POST")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	for _, origin := range []string{"", "https://fleet.bnei.dev"} {
		rec := do(http.MethodPost, agent+"ListThreads", origin, false)
		if !reached || rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("console origin %q: reached=%v ACAO=%q", origin, reached, rec.Header().Get("Access-Control-Allow-Origin"))
		}
	}

	rec := do(http.MethodPost, agent+"ListThreads", "https://opus.bnei.dev", false)
	if !reached || rec.Code != http.StatusUnauthorized ||
		rec.Header().Get("Access-Control-Allow-Origin") != "https://opus.bnei.dev" ||
		rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("island call: the Gate's 401 must carry CORS: code=%d headers=%v", rec.Code, rec.Header())
	}

	rec = do(http.MethodOptions, agent+"ListThreads", "https://opus.bnei.dev", true)
	if reached || rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Errorf("preflight: reached=%v code=%d", reached, rec.Code)
	}

	for _, origin := range []string{"https://evil.bnei.dev", "null", "https://opus.bnei.dev.evil.example", "http://opus.bnei.dev"} {
		rec := do(http.MethodPost, agent+"PostMessage", origin, false)
		if reached || rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("origin %q: reached=%v code=%d", origin, reached, rec.Code)
		}
	}

	// Other paths: no CORS at all, whatever the Origin. The dashboard's
	// preflight stays unanswered.
	rec = do(http.MethodOptions, "/agentfleet.v1.DashboardService/ListSessions", "https://opus.bnei.dev", true)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Vary") != "" {
		t.Errorf("dashboard path got CORS headers: %v", rec.Header())
	}
	// Escaped path, as the mux routes: an encoded slash is not the prefix.
	rec = do(http.MethodPost, "/agentfleet.v1.AgentService%2FListThreads", "https://opus.bnei.dev", false)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("an escaped path matched the AgentService prefix")
	}

	// No registry yet (nil): no island is accepted.
	none := CORS(agent, "https://fleet.bnei.dev", nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	reached = false
	r := httptest.NewRequest(http.MethodPost, agent+"ListThreads", nil)
	r.Header.Set("Origin", "https://opus.bnei.dev")
	rec = httptest.NewRecorder()
	none.ServeHTTP(rec, r)
	if reached || rec.Code != http.StatusForbidden {
		t.Errorf("nil allowed: reached=%v code=%d", reached, rec.Code)
	}
}
