package auth

import "net/http"

// FrameDeny forbids framing the console, on every response — the gate's own
// redirects and refusals included, which is why it wraps OUTSIDE the gate
// (docs/adr/0069). This is the surface where permission prompts get answered,
// so a page that could frame it could clickjack an approval. X-Frame-Options is
// for browsers that predate CSP frame-ancestors.
//
// frame-ancestors governs only who may EMBED a response; it does not touch
// top-level navigation, so the OIDC redirects through /auth/ are unaffected.
// The planned /embed/* shell will be the one exception, added with it.
func FrameDeny(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}
