package k8s

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func createOpts() metav1.CreateOptions { return metav1.CreateOptions{} }

func deleteOpts() metav1.DeleteOptions { return metav1.DeleteOptions{} }

type LiveE2ePod struct {
	TaskID  string
	PodName string
}

// ListPodsByLabel mirrors listE2ePodsByLabel — the reconcile loop's
// startup cross-check against tracked DB sessions.
func (c *Client) ListPodsByLabel(ctx context.Context) ([]LiveE2ePod, error) {
	list, err := c.Core.CoreV1().Pods(c.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: ComponentLabel + "=" + ComponentE2eRun,
	})
	if err != nil {
		return nil, err
	}
	out := make([]LiveE2ePod, 0, len(list.Items))
	for _, pod := range list.Items {
		taskID := pod.Labels[TaskIDLabel]
		if taskID == "" {
			continue
		}
		out = append(out, LiveE2ePod{TaskID: taskID, PodName: pod.Name})
	}
	return out, nil
}

// DeleteAll tears down every resource for a task, ignoring 404s — mirrors
// deleteE2eResources's ignore404 behavior exactly.
func (c *Client) DeleteAll(ctx context.Context, taskID string) error {
	if err := c.DeleteIngressRoute(ctx, taskID); err != nil {
		return err
	}
	if err := c.DeleteMiddleware(ctx, taskID); err != nil {
		return err
	}
	if err := c.DeleteService(ctx, taskID); err != nil {
		return err
	}
	return c.DeletePod(ctx, taskID)
}
