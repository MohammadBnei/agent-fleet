package k8s

import (
	"context"
	"fmt"

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
		"spec": map[string]any{
			"stripPrefix": map[string]any{
				"prefixes": []any{
					fmt.Sprintf("/%s/app", taskID),
					fmt.Sprintf("/%s/code", taskID),
				},
			},
		},
	}}
	_, err := c.Dynamic.Resource(middlewareGVR).Namespace(c.Namespace).Create(ctx, obj, createOpts())
	return err
}

func (c *Client) DeleteMiddleware(ctx context.Context, taskID string) error {
	err := c.Dynamic.Resource(middlewareGVR).Namespace(c.Namespace).Delete(ctx, stripPrefixName(taskID), deleteOpts())
	return ignoreNotFound(err)
}

func toInterfaceMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
