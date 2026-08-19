package k8s

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// There was no expose_test.go before this. Nothing anywhere asserted on the
// preview IngressRoute's rules, middlewares or services — so the route that
// publishes an agent-authored dev server on a public hostname had no
// regression net at all.
//
// What this can and cannot prove: the fake dynamic client validates the
// manifest's SHAPE and never runs Traefik. It proves the route is declared;
// only an actual browser round-trip proves auth works. That check is in the
// PR, not here.
func exposeRoutes(t *testing.T) []map[string]any {
	t.Helper()
	c := newTestClient()
	ctx := context.Background()

	if err := c.ensureExposeRoute(ctx, "task-1", "expose-task1", 3000, "bnei.dev", map[string]string{"a": "b"}); err != nil {
		t.Fatalf("ensureExposeRoute: %v", err)
	}
	obj, err := c.Dynamic.Resource(ingressRouteGVR).Namespace("agent-fleet").Get(ctx, "expose-task1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get ingressroute: %v", err)
	}
	spec, _ := obj.Object["spec"].(map[string]any)
	raw, _ := spec["routes"].([]any)
	routes := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		routes = append(routes, m)
	}
	return routes
}

func findRoute(t *testing.T, routes []map[string]any, substr string) map[string]any {
	t.Helper()
	for _, r := range routes {
		if m, _ := r["match"].(string); strings.Contains(m, substr) {
			return r
		}
	}
	t.Fatalf("no route matching %q in %+v", substr, routes)
	return nil
}

// The app route must be gated by authentik, not by the shared apr1 credential
// whose hash sits in k8s-cluster's git history.
func TestEnsureExposeRoute_AppRouteIsGatedByAuthentik(t *testing.T) {
	routes := exposeRoutes(t)
	app := findRoute(t, routes, "Host(`")

	// Pick the one WITHOUT the outpost prefix.
	for _, r := range routes {
		if m, _ := r["match"].(string); !strings.Contains(m, outpostPathPrefix) {
			app = r
		}
	}

	mws, _ := app["middlewares"].([]any)
	if len(mws) != 1 {
		t.Fatalf("app route middlewares = %+v, want exactly the forwardAuth one", mws)
	}
	mw, _ := mws[0].(map[string]any)
	if mw["name"] != "authentik-forwardauth" || mw["namespace"] != "default" {
		t.Errorf("app route middleware = %+v, want authentik-forwardauth in default", mw)
	}
	if strings.Contains(strings.ToLower(readableRoutes(routes)), "basic-admin-auth") {
		t.Error("basic-admin-auth still referenced by a preview route")
	}
}

// THE trap. authentik's sign-in callback comes back to the PREVIEW host, so
// without this rule the browser bounces between app and login forever — a
// redirect loop that reads as broken auth rather than as a missing route.
func TestEnsureExposeRoute_RoutesTheOutpostCallbackOnThePreviewHost(t *testing.T) {
	routes := exposeRoutes(t)
	if len(routes) != 2 {
		t.Fatalf("expected 2 rules (app + outpost callback), got %d: %s", len(routes), readableRoutes(routes))
	}
	outpost := findRoute(t, routes, outpostPathPrefix)

	svcs, _ := outpost["services"].([]any)
	if len(svcs) != 1 {
		t.Fatalf("outpost route services = %+v", svcs)
	}
	svc, _ := svcs[0].(map[string]any)
	if svc["name"] != authentikServiceName || svc["namespace"] != authentikServiceNamespace {
		t.Errorf("outpost route points at %+v, want %s in %s — the `platform-` release prefix is not optional and a wrong name fails as a 500 on the callback, not at apply time",
			svc, authentikServiceName, authentikServiceNamespace)
	}

	// Gating the callback path with the middleware it is the callback FOR is
	// its own redirect loop, and an easy thing to "fix" by adding.
	if mws, ok := outpost["middlewares"].([]any); ok && len(mws) > 0 {
		t.Errorf("the outpost callback route must carry NO auth middleware, got %+v", mws)
	}

	// Explicit priorities: the outpost rule is strictly more specific and must
	// win. Leaving that to Traefik's rule-length default makes it depend on how
	// long the hostname happens to be (ADR-0039 Decision 5).
	app := routes[0]
	if app["match"] == outpost["match"] {
		app = routes[1]
	}
	op, _ := outpost["priority"].(int64)
	ap, _ := app["priority"].(int64)
	if op <= ap {
		t.Errorf("outpost priority %d must exceed app priority %d", op, ap)
	}
}

func readableRoutes(routes []map[string]any) string {
	var b strings.Builder
	for _, r := range routes {
		b.WriteString(strings.TrimSpace(toString(r["match"])))
		b.WriteString(" mw=")
		b.WriteString(toString(r["middlewares"]))
		b.WriteString("; ")
	}
	return b.String()
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if list, ok := v.([]any); ok {
		var b strings.Builder
		for _, e := range list {
			if m, ok := e.(map[string]any); ok {
				b.WriteString(toString(m["name"]))
				b.WriteString(",")
			}
		}
		return b.String()
	}
	return ""
}
