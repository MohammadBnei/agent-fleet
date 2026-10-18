package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"connectrpc.com/connect"
)

type ctxKey struct{}

// FromContext returns the identity the gate established, if any. Used for
// attribution — who answered a permission prompt, who opened a session.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// exemptPrefixes are the ONLY paths reachable without a session.
//
// This is an allowlist, and the direction matters. Gating one handler and
// leaving the rest open means every route added later is public until someone
// remembers — the same shape as this repo's "a component must be wired into
// EVERY CI path" trap, with a worse failure. Wrapping the whole mux makes a
// forgotten route fail closed: visibly broken beats silently open.
//
//   - /healthz is the kubelet probe.
//   - /webhook/alertmanager has its own bearer token (ALERT_WEBHOOK_TOKEN) and
//     is called in-cluster by Prometheus and Grafana over the Service URL, which
//     never carries a browser cookie.
//   - /auth/ is the login itself.
//
// /metrics is deliberately ABSENT, and is no longer served on this mux at all —
// core carries it on CORE_METRICS_PORT (9093), which no IngressRoute reaches
// (docs/adr/0059).
//
// An earlier version of this comment claimed the exemption was unnecessary
// because /metrics "has no IngressRoute". That was false: the IngressRoute
// matches on HOST with no path constraint, so every path served on CORE_PORT is
// internet-routed and only this gate refuses it — and the ServiceMonitor,
// arriving in-cluster with no cookie, was refused along with everyone else
// (#230, every core target down). Exempting it here would have fixed the scrape
// by publishing every target repo name and RPC method. Moving the listener makes
// the original claim true instead of working around it.
var exemptPrefixes = []string{
	"/healthz",
	"/webhook/alertmanager",
	"/auth/",
}

func exempt(path string) bool {
	for _, p := range exemptPrefixes {
		if path == strings.TrimSuffix(p, "/") || strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// Gate wraps the whole mux. An unauthenticated request is redirected to the
// login for a document navigation and refused for anything else.
//
// The distinction is not cosmetic. The SPA's handler serves index.html with a
// 200 for any unknown path, so a redirect must happen BEFORE that fallback or
// an unauthenticated request to a bogus path returns the app shell. And a 302
// on a fetch() is followed transparently by the browser, so the SPA would
// receive authentik's HTML and try to parse it — the same class of failure that
// took ArgoCD's login down (infra-bootstrap #189), where an HTML body reached a
// JSON parser.
//
// An authenticated request without RequiredGroup gets a 403 — on EVERY
// request, not only at login (docs/adr/0069): a cookie outlives the moment it
// was minted, and the login check alone stops holding the day login admits
// anyone else. 403, never a redirect: login would refuse them again and the
// browser would loop.
//
// ponytail: one group for the whole mux. A service with a different audience
// (AgentService) needs its path excluded from this check and authorized by its
// own ConnectInterceptor(group) instead — add that when the service exists.
func (o *OIDC) Gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(SessionCookieName)
		if err == nil {
			if id, ok := o.signer.Verify(cookie.Value); ok {
				if !id.InGroup(RequiredGroup) {
					http.Error(w, "not authorized for this fleet", http.StatusForbidden)
					return
				}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
				return
			}
		}
		if isDocumentRequest(r) {
			http.Redirect(w, r, "/auth/login?return_to="+urlQueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
	})
}

// isDocumentRequest distinguishes "a person typed/clicked this" from "the SPA
// called it". Sec-Fetch-Mode is set by every browser that matters and is
// forbidden to fetch() callers, so it cannot be spoofed from page JS; the Accept
// check is the fallback for anything that omits it.
func isDocumentRequest(r *http.Request) bool {
	if m := r.Header.Get("Sec-Fetch-Mode"); m != "" {
		return m == "navigate"
	}
	return r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html")
}

func urlQueryEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "&", "%26"), "?", "%3F")
}

// Me answers "who am I", for the console's own chrome.
//
// Deliberately a plain JSON handler rather than a DashboardService RPC. It is
// read by one component to render one line; an RPC would mean a proto change,
// codegen in two languages and a buf breaking-change check, for a payload with
// no wire compatibility to protect.
//
// It sits under the /auth/ exempt prefix and therefore answers unauthenticated
// callers too — that is required, not an oversight: a page cannot ask "am I
// signed in" through a gate that refuses it for not being signed in. The
// response says so explicitly instead of relying on a status code, because the
// SPA's transport turns a 401 into a login redirect, and that would make simply
// LOADING the page while signed out bounce you to authentik before anything
// rendered.
//
// It returns the email and groups only. There is no user model behind this
// (docs/adr — the fleet has no `users` table); it is the verified claim, not a
// profile.
func (o *OIDC) Me(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		_ = json.NewEncoder(w).Encode(meResponse{})
		return
	}
	id, ok := o.signer.Verify(cookie.Value)
	if !ok {
		_ = json.NewEncoder(w).Encode(meResponse{})
		return
	}
	_ = json.NewEncoder(w).Encode(meResponse{
		Authenticated: true,
		Email:         id.Email,
		Groups:        id.Groups,
	})
}

type meResponse struct {
	Authenticated bool     `json:"authenticated"`
	Email         string   `json:"email,omitempty"`
	Groups        []string `json:"groups,omitempty"`
}

// ConnectInterceptor applies the same check to a Connect service: a valid
// session cookie (else Unauthenticated) carrying requiredGroup (else
// PermissionDenied), on every call. The group is the caller's argument so each
// service states its own audience; an empty group admits nobody.
//
// Registered as a Connect interceptor rather than left to Gate above because
// this covers unary AND streaming identically — the existing CSRF interceptor
// is the same shape for the same reason. Both run: the header stops a
// cross-origin page forging a call, this stops an unauthenticated one.
//
// #200's actual demonstration was a curl from a worker pod carrying only
// X-Fleet-Dashboard. That request has no session cookie, so it now returns
// unauthenticated regardless of which network it arrives from — which is the
// property a Traefik middleware could not have provided.
func (o *OIDC) ConnectInterceptor(requiredGroup string) connect.Interceptor {
	return connectAuth{signer: o.signer, group: requiredGroup}
}

type connectAuth struct {
	signer *Signer
	group  string
}

func (c connectAuth) authorize(h http.Header) (Identity, error) {
	// Reuse net/http's cookie parsing rather than splitting the header by
	// hand; a request carries several cookies and the naive split gets the
	// wrong one.
	req := http.Request{Header: h}
	cookie, err := req.Cookie(SessionCookieName)
	if err != nil {
		return Identity{}, connect.NewError(connect.CodeUnauthenticated, errUnauthenticated)
	}
	id, ok := c.signer.Verify(cookie.Value)
	if !ok {
		return Identity{}, connect.NewError(connect.CodeUnauthenticated, errUnauthenticated)
	}
	if !id.InGroup(c.group) {
		return Identity{}, connect.NewError(connect.CodePermissionDenied, errForbidden)
	}
	return id, nil
}

func (c connectAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		id, err := c.authorize(req.Header())
		if err != nil {
			return nil, err
		}
		return next(context.WithValue(ctx, ctxKey{}, id), req)
	}
}

func (c connectAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (c connectAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		id, err := c.authorize(conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(context.WithValue(ctx, ctxKey{}, id), conn)
	}
}

var (
	errUnauthenticated = &authError{"not signed in"}
	errForbidden       = &authError{"not authorized for this fleet"}
)

type authError struct{ msg string }

func (e *authError) Error() string { return e.msg }
