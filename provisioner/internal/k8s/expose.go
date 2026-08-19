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

// forwardAuthMiddleware gates every preview behind authentik
// (infra-bootstrap ADR-0039's forwardAuth tier, provider declared in that
// repo's gitops/bootstrap/authentik-blueprint-forwardauth.yaml).
//
// Replaces basic-admin-auth, the shared apr1/MD5 credential whose hash is in
// k8s-cluster's git history. These hosts are public and serve whatever an agent
// happened to start, so "one password everybody knows" was the weakest lock on
// a door the fleet opens automatically.
//
// The provider runs in forward_domain mode with cookie_domain: bnei.dev
// specifically so that ONE provider covers every host PreviewHostFor mints.
// forward_single is keyed on a fixed external_host and cannot express a
// hostname that does not exist until a session asks for it.
//
// In `default`, not this namespace: k8s/provisioner/role.yaml deliberately
// grants no `middlewares` verbs, so the provisioner can only reference a
// Middleware it does not manage. Do not add the verb to make a local copy.
var forwardAuthMiddleware = map[string]any{"name": "authentik-forwardauth", "namespace": "default"}

// authentikService is where the sign-in callback goes. `platform-` is the
// ApplicationSet's release prefix and is not optional — a wrong hostname here
// fails as a 500 on the callback, not as a config error at apply time.
const (
	authentikServiceName      = "platform-authentik-server"
	authentikServiceNamespace = "authentik"
	authentikServicePort      = 80
	// outpostPathPrefix is where authentik's embedded outpost handles the
	// sign-in callback. It lands on the PREVIEW host, not on
	// authentik.bnei.dev, so every preview route has to carry it.
	outpostPathPrefix = "/outpost.goauthentik.io/"
)

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
			// TWO rules, and the second is not optional.
			//
			// authentik's sign-in callback comes back to the PREVIEW host, not
			// to authentik.bnei.dev, so each preview must route
			// /outpost.goauthentik.io/ to the outpost itself. Without it the
			// browser bounces between the app and the login forever — which
			// reads as a broken auth flow rather than as a missing route, and
			// is the single most likely way to lose an afternoon here.
			//
			// Explicit priorities rather than Traefik's rule-length default
			// (infra-bootstrap ADR-0039 Decision 5): the outpost rule is
			// strictly more specific and must win, and leaving that to an
			// implicit tiebreak makes it depend on how long a hostname happens
			// to be.
			"routes": []any{
				map[string]any{
					"match":    fmt.Sprintf("Host(`%s`) && PathPrefix(`%s`)", PreviewHostFor(host, sessionID), outpostPathPrefix),
					"kind":     "Rule",
					"priority": int64(20),
					"services": []any{
						map[string]any{
							"name":      authentikServiceName,
							"namespace": authentikServiceNamespace,
							"port":      int64(authentikServicePort),
						},
					},
					// NO middleware here, deliberately. Gating the callback
					// path with the thing it is the callback FOR is its own
					// redirect loop.
				},
				map[string]any{
					"match":    fmt.Sprintf("Host(`%s`)", PreviewHostFor(host, sessionID)),
					"kind":     "Rule",
					"priority": int64(10),
					"services": []any{
						map[string]any{"name": svcName, "namespace": c.Namespace, "port": int64(port)},
					},
					"middlewares": []any{forwardAuthMiddleware},
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
