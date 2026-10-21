package auth

import (
	"context"
	"encoding/json"
	"log/slog"
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

// exemptPaths are the ONLY paths reachable without a session, matched exactly
// against the escaped path. A prefix match let /healthzX or /auth/anything
// through to the SPA fallback, which answers any unknown path with a 200 shell.
//
// An allowlist on purpose: wrapping the whole mux makes a forgotten route fail
// closed — visibly broken beats silently open.
//
//   - /healthz is the kubelet probe.
//   - /webhook/alertmanager has its own bearer token (ALERT_WEBHOOK_TOKEN) and
//     is called in-cluster over the Service URL, never with a browser cookie.
//   - /auth/{login,callback,logout,me} are the four routes run.go registers.
//   - /auth/done is the static page the island's sign-in popup lands on and
//     closes (docs/adr/0068 §3). It carries no data.
//   - /islands/ is the one exempt PREFIX: the islands' static bundles, no
//     data, loaded by host pages before anyone signs in (docs/adr/0068 §3).
//
// /metrics is deliberately ABSENT and not served on this mux at all: it lives
// on CORE_METRICS_PORT (9093), which no IngressRoute reaches. Exempting it here
// would publish every target repo name and RPC method (#230, docs/adr/0059).
var exemptPaths = map[string]bool{
	"/healthz":              true,
	"/webhook/alertmanager": true,
	"/auth/login":           true,
	"/auth/callback":        true,
	"/auth/logout":          true,
	"/auth/me":              true,
	"/auth/done":            true,
}

// islandsPrefix is matched on the escaped path like everything here; the file
// server under it serves only what the embedded bundle holds.
const islandsPrefix = "/islands/"

func exempt(path string) bool { return exemptPaths[path] || strings.HasPrefix(path, islandsPrefix) }

// Policy is the authorization rule a layer applies to an authenticated
// identity.
//
// ponytail: identity only. A per-definition decision needs the request, which
// belongs in that service's handler; a path, if ever needed, must be the
// ESCAPED one the exemption and ServeMux use, never r.URL.Path.
type Policy func(Identity) bool

// AdminOnly admits RequiredGroup members. It is the console's policy at the
// Gate and on DashboardService.
func AdminOnly(id Identity) bool { return id.InGroup(RequiredGroup) }

// AnyGroup admits an identity in any of groups, or an admin.
func AnyGroup(groups []string) Policy {
	return func(id Identity) bool {
		if AdminOnly(id) {
			return true
		}
		for _, g := range groups {
			if id.InGroup(g) {
				return true
			}
		}
		return false
	}
}

// Route widens the Gate for one path prefix (docs/adr/0066 §2): the member
// routes, AgentService's. Every other path keeps the
// Gate's default policy, so a route added later is admin-only unless someone
// lists it here.
type Route struct {
	Prefix string
	Allow  Policy
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
// An authenticated identity allow refuses gets a 403 on every request
// (docs/adr/0069; not revocation — see docs/verification-traps.md). 403, never
// a redirect: login would refuse them again and the browser would loop.
//
// allow is the coarse "may use this console at all" check, one value for every
// gated path, so it must admit the UNION of every service's audience; each
// Connect service's own ConnectInterceptor policy then narrows it. routes
// widen it for their prefix only, matched on the escaped path.
func (o *OIDC) Gate(allow Policy, routes ...Route) func(http.Handler) http.Handler {
	authz := connectAuth{signer: o.signer, allow: allow}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The ESCAPED path, because that is what ServeMux routes on:
			// /auth%2Fme decodes to Path "/auth/me" but reaches the "/" SPA
			// fallback, so matching Path would hand it the 200 shell unauthenticated.
			path := r.URL.EscapedPath()
			if exempt(path) {
				next.ServeHTTP(w, r)
				return
			}
			a := authz
			for _, rt := range routes {
				if strings.HasPrefix(path, rt.Prefix) {
					a = connectAuth{signer: o.signer, allow: rt.Allow}
					break
				}
			}
			id, err := a.authorize(r.Header)
			switch {
			case err == nil:
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
			case connect.CodeOf(err) == connect.CodePermissionDenied:
				logForbidden(id, r.URL.EscapedPath())
				http.Error(w, errForbidden.Error(), http.StatusForbidden)
			case isDocumentRequest(r):
				http.Redirect(w, r, "/auth/login?return_to="+urlQueryEscape(r.URL.RequestURI()), http.StatusFound)
			default:
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
			}
		})
	}
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
// It is an exempt path and therefore answers unauthenticated
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

// ConnectInterceptor applies allow to a Connect service: a valid session cookie
// (else Unauthenticated) that allow admits (else PermissionDenied), on every
// call, unary and streaming alike. A nil allow admits nobody. Behind the Gate
// it is the second layer, and the one that still holds for a path the Gate is
// ever told to skip.
//
// Both this and the CSRF interceptor run: the header stops a cross-origin page
// forging a call, this stops an unauthenticated one. #200's demonstration — a
// curl from a worker pod carrying only X-Fleet-Dashboard — has no cookie, so it
// is refused from any network, which a Traefik middleware could not provide.
func (o *OIDC) ConnectInterceptor(allow Policy) connect.Interceptor {
	return connectAuth{signer: o.signer, allow: allow}
}

// connectAuth is the one authorization mechanism, shared by the Gate and the
// interceptor. Each is handed its own Policy; run.go passes both.
type connectAuth struct {
	signer *Signer
	allow  Policy
}

// authorize returns the identity even on PermissionDenied, so the caller can
// log who was refused. It does not log: in production the Gate refuses first
// and logs, and a call that ever reaches here unrefused-by-the-Gate is already
// logged by the access-log interceptor ahead of this one.
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
	if c.allow == nil || !c.allow(id) {
		return id, connect.NewError(connect.CodePermissionDenied, errForbidden)
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

// logForbidden makes a Gate denial visible. The Gate's 403 fires before the
// Connect access-log and metrics interceptors, so without this a refused caller
// leaves no trace; logged with the groups received, like the callback, because
// a renamed group or a policy typo looks exactly like this.
//
// ponytail: one line per refused request, unthrottled. No cookie the callback
// mints can be refused today; dedupe per identity (or a denial counter) when
// login admits a group the Gate refuses.
func logForbidden(id Identity, path string) {
	slog.Warn("auth: refused request by policy",
		"email", id.Email, "groups", id.Groups, "path", path)
}

type authError struct{ msg string }

func (e *authError) Error() string { return e.msg }

// WithIdentity is the context FromContext reads. For an interceptor that
// stands in for this package's in tests.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}
