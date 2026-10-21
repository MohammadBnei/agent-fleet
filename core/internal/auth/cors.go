package auth

import (
	"net/http"
	"slices"
	"strings"
)

// corsAllowHeaders are what connect-web sends: Connect's own headers plus the
// CSRF header AgentService requires (docs/adr/0068 §3).
const corsAllowHeaders = "Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, Connect-Content-Encoding, Connect-Accept-Encoding, Accept-Encoding, X-Fleet-Dashboard"

// CORS answers cross-origin calls to one path prefix (AgentService's) for the
// exact origins the definitions list in embed.origins, and nothing else
// (docs/adr/0068 §3, amending 0066 §1). It sits outside the Gate:
//
//   - a preflight carries no cookie, so it is answered here, without auth;
//   - the Gate's own 401/403 must carry the CORS headers, or the island sees
//     an opaque network error instead of Unauthenticated.
//
// No Origin, or the fleet's own, is the console and passes untouched. Any
// other Origin, "null" included, is refused here with a 403 before the Gate or
// a handler runs: a refused preflight alone does not stop a simple POST.
// AgentService's MemberInterceptor refuses the same origins again.
//
// Every other path gets no CORS header at all, so DashboardService's CSRF
// header still forces a preflight that is never answered.
func CORS(prefix, fleetOrigin string, allowed func() []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The ESCAPED path, the string the Gate and the mux route on.
			if !strings.HasPrefix(r.URL.EscapedPath(), prefix) {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Add("Vary", "Origin")
			origin := r.Header.Get("Origin")
			if origin == "" || origin == fleetOrigin {
				next.ServeHTTP(w, r)
				return
			}
			if allowed == nil || !slices.Contains(allowed(), origin) {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST")
				h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
