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
			// The e2e-runner's ReadinessProbe watches AppPort only (see
			// pod.go), so without this the pod's endpoint is marked
			// ready=false whenever the target app isn't listening, and
			// kube-proxy/Traefik route nothing to it — taking exec,
			// playwright and code-server down with the app. (The endpoint
			// stays *in* the EndpointSlice either way; it's the `ready`
			// condition that changes, and publishNotReadyAddresses forces it
			// true. Verified live on ukubi-cluster 2026-08-12: same pod, two
			// Services — with this field a request to the exec port returned
			// HTTP 200 while the app port was dead, without it the same
			// request was unreachable.) That's backwards:
			// a pod whose start command is broken is exactly when the agent
			// needs run_command and the human needs code-server, which is the
			// reason docs/adr/0036 picked readiness over liveness in the first
			// place ("a pod whose app never binds must stay alive so
			// code-server is still reachable to debug why") — ready-gated
			// endpoints silently defeated that.
			// app_ready on the dashboard card is unaffected: it reads pod
			// conditions via GetPod, not endpoints. The only cost is Traefik
			// returning a refused-connection 502 instead of a 503 while the
			// app is still starting.
			PublishNotReadyAddresses: true,
			Ports: []corev1.ServicePort{
				{Name: "app", Port: AppPort, TargetPort: intstr.FromInt32(AppPort)},
				{Name: "code-server", Port: CodeServerPort, TargetPort: intstr.FromInt32(CodeServerPort)},
				{Name: "playwright", Port: PlaywrightPort, TargetPort: intstr.FromInt32(PlaywrightPort)},
				{Name: "exec", Port: ExecPort, TargetPort: intstr.FromInt32(ExecPort)},
				{Name: "ssh", Port: SSHPort, TargetPort: intstr.FromInt32(SSHPort)},
			},
		},
	}
	_, err := c.Core.CoreV1().Services(c.Namespace).Create(ctx, svc, metav1.CreateOptions{})
	if err = ignoreAlreadyExists(err); err != nil {
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
