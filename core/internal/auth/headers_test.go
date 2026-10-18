package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// One middleware, every response: the console must not be framed (clickjacking
// on the surface where permission prompts are answered). That includes the
// gate's own 302/401/403 and the /auth/ pages — frame-ancestors governs only
// who may embed the response, so it cannot interfere with the OIDC redirects.
func TestFrameDeny_SetsHeadersOnEveryResponse(t *testing.T) {
	o := &OIDC{signer: mustSigner(t, "key-one")}
	handler := FrameDeny(o.Gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/login" {
			http.Redirect(w, r, "https://authentik.bnei.dev/application/o/authorize/", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})))
	for _, tc := range []struct {
		path, mode string
		want       int
	}{
		{"/healthz", "", http.StatusOK},
		{"/auth/login", "navigate", http.StatusFound},
		{"/sessions/abc", "navigate", http.StatusFound}, // gate redirect to login
		{"/agentfleet.v1.DashboardService/ListSessions", "cors", http.StatusUnauthorized},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.mode != "" {
			req.Header.Set("Sec-Fetch-Mode", tc.mode)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: got %d, want %d", tc.path, rec.Code, tc.want)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
			t.Errorf("%s: CSP = %q", tc.path, got)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q", tc.path, got)
		}
	}
}
