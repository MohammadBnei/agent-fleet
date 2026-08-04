package dashboard

import "net/http"

// dashboardHeader is required on all write routes. BasicAuth credentials
// are auto-attached by browsers to same-origin requests regardless of
// which page triggered them, so BasicAuth alone lets a third-party page
// forge these calls. A plain HTML form/img can't set a custom header, and
// a cross-origin fetch that tries one triggers a CORS preflight this
// server never allows — so only same-origin JS (the dashboard's own SPA)
// can successfully call these routes (see docs/adr/0013).
const dashboardHeader = "X-Fleet-Dashboard"

func requireDashboardHeader(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(dashboardHeader) == "" {
			http.Error(w, "missing required header", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}
