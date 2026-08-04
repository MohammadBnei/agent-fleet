package k8s

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func (c *Client) CreateService(ctx context.Context, taskID string) error {
	name := ResourceName(taskID)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: Labels(taskID)},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{TaskIDLabel: taskID},
			Ports: []corev1.ServicePort{
				{Name: "app", Port: AppPort, TargetPort: intstr.FromInt32(AppPort)},
				{Name: "code-server", Port: CodeServerPort, TargetPort: intstr.FromInt32(CodeServerPort)},
				{Name: "playwright", Port: PlaywrightPort, TargetPort: intstr.FromInt32(PlaywrightPort)},
			},
		},
	}
	_, err := c.Core.CoreV1().Services(c.Namespace).Create(ctx, svc, metav1.CreateOptions{})
	return err
}

func (c *Client) DeleteService(ctx context.Context, taskID string) error {
	err := c.Core.CoreV1().Services(c.Namespace).Delete(ctx, ResourceName(taskID), metav1.DeleteOptions{})
	return ignoreNotFound(err)
}
