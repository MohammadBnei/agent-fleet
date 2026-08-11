package k8s

import (
	"context"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func (c *Client) CreateService(ctx context.Context, taskID string) error {
	name := ResourceName(taskID)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: Labels(taskID)},
		Spec: corev1.ServiceSpec{
			// Must include the component label: the worker pod carries the
			// same task-id label, so a task-id-only selector puts it in this
			// Service's endpoints too and ~half the preview requests land on
			// a pod with nothing on :3000 → intermittent 502.
			Selector: map[string]string{TaskIDLabel: taskID, ComponentLabel: ComponentE2eRun},
			Ports: []corev1.ServicePort{
				{Name: "app", Port: AppPort, TargetPort: intstr.FromInt32(AppPort)},
				{Name: "code-server", Port: CodeServerPort, TargetPort: intstr.FromInt32(CodeServerPort)},
				{Name: "playwright", Port: PlaywrightPort, TargetPort: intstr.FromInt32(PlaywrightPort)},
				{Name: "exec", Port: ExecPort, TargetPort: intstr.FromInt32(ExecPort)},
			},
		},
	}
	_, err := c.Core.CoreV1().Services(c.Namespace).Create(ctx, svc, metav1.CreateOptions{})
	if err != nil {
		slog.Error("k8s CreateService", "taskId", taskID, "error", err)
		return err
	}
	slog.Info("k8s CreateService", "taskId", taskID)
	return nil
}

func (c *Client) DeleteService(ctx context.Context, taskID string) error {
	err := ignoreNotFound(c.Core.CoreV1().Services(c.Namespace).Delete(ctx, ResourceName(taskID), metav1.DeleteOptions{}))
	if err != nil {
		slog.Error("k8s DeleteService", "taskId", taskID, "error", err)
		return err
	}
	slog.Info("k8s DeleteService", "taskId", taskID)
	return nil
}
