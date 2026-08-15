package k8s

import (
	"context"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// basicAuthMiddleware is reused across every session instead of minting a
// per-session Secret — the same LAN-admin credential already gating
// pgweb/Alertmanager.
//
// It used to sit in ingressroute.go alongside a cluster-IP-whitelist
// middleware that let worker pods reach each other's previews without a
// password. That file is gone with the e2e pod (docs/adr/0048 §6), and so is
// the whitelist: there is no second pod to let through.
var basicAuthMiddleware = map[string]any{"name": "basic-admin-auth", "namespace": "default"}

// toInterfaceMap converts a label map for an unstructured object, which is
// map[string]any all the way down.
func toInterfaceMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ExposeSession publishes one port from a session's pod at
// https://<session>.<host>, and returns the URL.
//
// This is the whole of what replaced the e2e preview (docs/adr/0048 §6). The
// old path provisioned a second pod, installed a toolchain into it, ran a
// recipe-resolved start command, and waited on a readiness probe — because the
// platform owned starting the app. Here the agent has already started its own
// server with Bash; all that is left is a Service and a route.
//
// Idempotent in both halves. The agent is told it may call expose() again
// after a warm, and warms replace the pod while the hostname must not change.
// Re-exposing a DIFFERENT port updates the Service rather than erroring: the
// agent moving its dev server is an ordinary thing, and failing the call would
// leave the old port routed with no way to correct it.
func (c *Client) ExposeSession(ctx context.Context, sessionID string, port int32, host string) (string, error) {
	if port <= 0 || port > 65535 {
		return "", fmt.Errorf("port %d out of range", port)
	}
	name := ExposeResourceName(sessionID)
	labels := map[string]string{
		"app.kubernetes.io/part-of": "agent-fleet",
		"agent-fleet/session-id":    shortID(sessionID),
		"agent-fleet/exposed":       "true",
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			// Selects the session's own pod by the labels its Job already
			// carries — no new labelling scheme, and nothing to keep in sync
			// when a warm replaces the pod.
			Selector: WorkerSelectorLabels(sessionID),
			Ports: []corev1.ServicePort{{
				Name:       "app",
				Port:       port,
				TargetPort: intstr.FromInt32(port),
			}},
		},
	}

	if existing, err := c.Core.CoreV1().Services(c.Namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		// Update in place rather than delete-and-recreate: recreating would
		// briefly 503 a URL the human may be looking at.
		existing.Spec.Ports = svc.Spec.Ports
		existing.Spec.Selector = svc.Spec.Selector
		if _, err := c.Core.CoreV1().Services(c.Namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			return "", fmt.Errorf("update expose service: %w", err)
		}
	} else if _, err := c.Core.CoreV1().Services(c.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		if err = ignoreAlreadyExists(err); err != nil {
			return "", fmt.Errorf("create expose service: %w", err)
		}
	}

	if err := c.ensureExposeRoute(ctx, sessionID, name, port, host, labels); err != nil {
		return "", err
	}

	url := "https://" + PreviewHostFor(host, sessionID)
	slog.Info("k8s ExposeSession", "sessionId", sessionID, "port", port, "url", url)
	return url, nil
}

// ensureExposeRoute writes the IngressRoute for an exposed session.
//
// One route at the host root, no path prefix and no stripPrefix — an app
// served under a path it does not know it is under breaks its own asset URLs,
// which is why docs/adr/0038 moved previews to per-session subdomains in the
// first place.
//
// Deleted and recreated rather than patched, unlike the Service: the route's
// service port is nested inside an unstructured spec, and rewriting the object
// wholesale is less code than a typed patch into a map tree. A momentary gap
// here is invisible — Traefik reloads in well under the time it takes anyone
// to click.
func (c *Client) ensureExposeRoute(ctx context.Context, sessionID, svcName string, port int32, host string, labels map[string]string) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1",
		"kind":       "IngressRoute",
		"metadata": map[string]any{
			"name":      svcName,
			"namespace": c.Namespace,
			"labels":    toInterfaceMap(labels),
		},
		"spec": map[string]any{
			"entryPoints": []any{"websecure"},
			// le-dns, not le: TLS-ALPN-01 structurally cannot issue a
			// wildcard, only DNS-01 can. Every session asks for the same
			// *.<host> cert so ACME orders it exactly once
			// (infra-bootstrap ADR-0033).
			"tls": map[string]any{
				"certResolver": "le-dns",
				"domains":      []any{map[string]any{"main": PreviewDomainFor(host)}},
			},
			"routes": []any{
				map[string]any{
					"match": fmt.Sprintf("Host(`%s`)", PreviewHostFor(host, sessionID)),
					"kind":  "Rule",
					"services": []any{
						map[string]any{"name": svcName, "namespace": c.Namespace, "port": int64(port)},
					},
					// Basic auth only. The cluster-IP whitelist that used to
					// sit in front of this existed so worker pods could reach
					// each other's previews without a password; there is no
					// second pod to let through any more.
					"middlewares": []any{basicAuthMiddleware},
				},
			},
		},
	}}

	_ = ignoreNotFound(c.Dynamic.Resource(ingressRouteGVR).Namespace(c.Namespace).Delete(ctx, svcName, deleteOpts()))
	if _, err := c.Dynamic.Resource(ingressRouteGVR).Namespace(c.Namespace).Create(ctx, obj, createOpts()); err != nil {
		if err = ignoreAlreadyExists(err); err != nil {
			slog.Error("k8s ensureExposeRoute", "sessionId", sessionID, "error", err)
			return fmt.Errorf("create expose route: %w", err)
		}
	}
	return nil
}

// UnexposeSession removes a session's Service and route. The agent's server
// keeps running — this only stops publishing it.
//
// Called on teardown as well as by the agent, so "nothing to remove" is the
// common case and must not be an error.
func (c *Client) UnexposeSession(ctx context.Context, sessionID string) error {
	name := ExposeResourceName(sessionID)
	if err := ignoreNotFound(c.Dynamic.Resource(ingressRouteGVR).Namespace(c.Namespace).Delete(ctx, name, deleteOpts())); err != nil {
		return fmt.Errorf("delete expose route: %w", err)
	}
	if err := ignoreNotFound(c.Core.CoreV1().Services(c.Namespace).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
		return fmt.Errorf("delete expose service: %w", err)
	}
	slog.Info("k8s UnexposeSession", "sessionId", sessionID)
	return nil
}
