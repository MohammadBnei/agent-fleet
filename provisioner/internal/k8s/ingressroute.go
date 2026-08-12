package k8s

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// basicAuthMiddleware is reused across every session instead of minting a
// per-task Secret — same LAN-admin credential already gates pgweb/Alertmanager.
var basicAuthMiddleware = map[string]any{"name": "basic-admin-auth", "namespace": "default"}

func (c *Client) CreateIngressRoute(ctx context.Context, host, taskID string) error {
	name := ResourceName(taskID)
	spName := stripPrefixName(taskID)

	// clusterIPWhitelistMiddleware allows in-cluster traffic (worker pods) to
	// bypass basic auth. Middleware order matters: Traefik applies them left-to-right,
	// so IPWhiteList comes first — if source IP matches, request proceeds without
	// hitting basic auth.
	clusterIPWhitelistMiddleware := map[string]any{"name": "cluster-ip-whitelist", "namespace": c.Namespace}

	// Code-server route: stripPrefix first, then cluster IP whitelist, then basic auth.
	// IP whitelist short-circuits auth for in-cluster requests (worker pods).
	codeMiddlewares := []any{
		map[string]any{"name": spName, "namespace": c.Namespace},
		clusterIPWhitelistMiddleware,
		basicAuthMiddleware,
	}

	// App route: cluster IP whitelist first, then basic auth (no stripPrefix).
	appMiddlewares := []any{
		clusterIPWhitelistMiddleware,
		basicAuthMiddleware,
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1",
		"kind":       "IngressRoute",
		"metadata": map[string]any{
			"name":      name,
			"namespace": c.Namespace,
			"labels":    toInterfaceMap(Labels(taskID)),
		},
		"spec": map[string]any{
			"entryPoints": []any{"websecure"},
			// le-dns, not le: TLS-ALPN-01 structurally cannot issue a
			// wildcard, only DNS-01 can. Declared explicitly so every task
			// asks for the same *.<host> cert and ACME orders it exactly
			// once — see PreviewDomainFor for why per-task certs are not an
			// option. infra-bootstrap's ADR-0033 adds the resolver.
			"tls": map[string]any{
				"certResolver": "le-dns",
				"domains":      []any{map[string]any{"main": PreviewDomainFor(host)}},
			},
			"routes": []any{
				// code-server keeps a path prefix — it is proxy-aware and
				// handles X-Forwarded-Prefix correctly, unlike an arbitrary
				// target app. Priority is explicit: rule length would already
				// order these correctly, but leaving it implicit makes a
				// target app that owns /code a silent coin flip.
				//
				// ponytail: an app with its own /code route is shadowed here.
				// Give code-server its own <id>-code subdomain if that bites —
				// free now, since the wildcard cert covers any label.
				map[string]any{
					"match":    fmt.Sprintf("Host(`%s`) && PathPrefix(`/code`)", PreviewHostFor(host, taskID)),
					"kind":     "Rule",
					"priority": int64(200),
					"services": []any{
						map[string]any{"name": name, "namespace": c.Namespace, "port": int64(CodeServerPort)},
					},
					"middlewares": codeMiddlewares,
				},
				// The app at the root of its own hostname, with NO stripPrefix
				// — that is the whole point of docs/adr/0038.
				map[string]any{
					"match":    fmt.Sprintf("Host(`%s`)", PreviewHostFor(host, taskID)),
					"kind":     "Rule",
					"priority": int64(100),
					"services": []any{
						map[string]any{"name": name, "namespace": c.Namespace, "port": int64(AppPort)},
					},
					"middlewares": appMiddlewares,
				},
			},
		},
	}}
	_, err := c.Dynamic.Resource(ingressRouteGVR).Namespace(c.Namespace).Create(ctx, obj, createOpts())
	if err = ignoreAlreadyExists(err); err != nil {
		slog.Error("k8s CreateIngressRoute", "taskId", taskID, "error", err)
		return err
	}
	slog.Info("k8s CreateIngressRoute", "taskId", taskID)
	return nil
}

func (c *Client) DeleteIngressRoute(ctx context.Context, taskID string) error {
	err := ignoreNotFound(c.Dynamic.Resource(ingressRouteGVR).Namespace(c.Namespace).Delete(ctx, ResourceName(taskID), deleteOpts()))
	if err != nil {
		slog.Error("k8s DeleteIngressRoute", "taskId", taskID, "error", err)
		return err
	}
	slog.Info("k8s DeleteIngressRoute", "taskId", taskID)
	return nil
}
