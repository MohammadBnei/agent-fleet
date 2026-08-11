package k8s

import (
	"context"
	"log/slog"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func stripPrefixName(taskID string) string {
	return ResourceName(taskID) + "-stripprefix"
}

func (c *Client) CreateMiddleware(ctx context.Context, taskID string) error {
	name := stripPrefixName(taskID)
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1",
		"kind":       "Middleware",
		"metadata": map[string]any{
			"name":      name,
			"namespace": c.Namespace,
			"labels":    toInterfaceMap(Labels(taskID)),
		},
		// Only /code now, and no longer task-ID-derived: as of docs/adr/0038
		// each task gets its own hostname, so the app is served at the root
		// with nothing stripped. code-server keeps a prefix because it is
		// proxy-aware (it reads X-Forwarded-Prefix, which stripPrefix sets);
		// an arbitrary target app is not, which was the original bug.
		//
		// ponytail: the middleware is now identical for every task, so this
		// could be one shared object. Kept per-task because DeleteAll and
		// Labels() already handle its lifecycle — collapsing it would be a
		// bigger diff than it saves.
		"spec": map[string]any{
			"stripPrefix": map[string]any{
				"prefixes": []any{"/code"},
			},
		},
	}}
	_, err := c.Dynamic.Resource(middlewareGVR).Namespace(c.Namespace).Create(ctx, obj, createOpts())
	if err = ignoreAlreadyExists(err); err != nil {
		slog.Error("k8s CreateMiddleware", "taskId", taskID, "error", err)
		return err
	}
	slog.Info("k8s CreateMiddleware", "taskId", taskID)
	return nil
}

func (c *Client) DeleteMiddleware(ctx context.Context, taskID string) error {
	err := ignoreNotFound(c.Dynamic.Resource(middlewareGVR).Namespace(c.Namespace).Delete(ctx, stripPrefixName(taskID), deleteOpts()))
	if err != nil {
		slog.Error("k8s DeleteMiddleware", "taskId", taskID, "error", err)
		return err
	}
	slog.Info("k8s DeleteMiddleware", "taskId", taskID)
	return nil
}

func toInterfaceMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
