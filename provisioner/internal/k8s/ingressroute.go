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
	middlewares := []any{
		map[string]any{"name": spName, "namespace": c.Namespace},
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
			"tls":         map[string]any{"certResolver": "le"},
			"routes": []any{
				map[string]any{
					"match": fmt.Sprintf("Host(`%s`) && PathPrefix(`/%s/app`)", host, taskID),
					"kind":  "Rule",
					"services": []any{
						map[string]any{"name": name, "namespace": c.Namespace, "port": int64(AppPort)},
					},
					"middlewares": middlewares,
				},
				map[string]any{
					"match": fmt.Sprintf("Host(`%s`) && PathPrefix(`/%s/code`)", host, taskID),
					"kind":  "Rule",
					"services": []any{
						map[string]any{"name": name, "namespace": c.Namespace, "port": int64(CodeServerPort)},
					},
					"middlewares": middlewares,
				},
			},
		},
	}}
	_, err := c.Dynamic.Resource(ingressRouteGVR).Namespace(c.Namespace).Create(ctx, obj, createOpts())
	if err != nil {
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
