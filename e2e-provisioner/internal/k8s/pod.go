package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CreatedE2ePod mirrors CreatedE2ePod in k8s.ts.
type CreatedE2ePod struct {
	PodName     string
	IngressPath string
}

// TaskRef is the subset of a task row this package needs.
type TaskRef struct {
	ID   string
	Repo string
}

func (c *Client) CreatePod(ctx context.Context, task TaskRef) error {
	startCmd, err := StartCmdFor(task.Repo)
	if err != nil {
		return err
	}
	name := ResourceName(task.ID)
	labels := Labels(task.ID)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "e2e-runner",
					Image: c.RunnerImage,
					Env: []corev1.EnvVar{
						{Name: "E2E_WORKTREE_PATH", Value: "/workspace"},
						{Name: "E2E_START_CMD", Value: startCmd},
						{Name: "E2E_APP_PORT", Value: fmt.Sprint(AppPort)},
						{Name: "E2E_CODE_SERVER_PORT", Value: fmt.Sprint(CodeServerPort)},
						{Name: "E2E_PLAYWRIGHT_PORT", Value: fmt.Sprint(PlaywrightPort)},
					},
					Ports: []corev1.ContainerPort{
						{Name: "app", ContainerPort: AppPort},
						{Name: "code-server", ContainerPort: CodeServerPort},
						{Name: "playwright", ContainerPort: PlaywrightPort},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workspace", MountPath: "/workspace", SubPath: "worktrees/" + task.ID},
						{Name: "dshm", MountPath: "/dev/shm"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "workspace",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: WorkspacePvcFor(task.Repo)},
					},
				},
				{
					Name: "dshm",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							Medium:    corev1.StorageMediumMemory,
							SizeLimit: resourcePtr("1Gi"),
						},
					},
				},
			},
		},
	}

	_, err = c.Core.CoreV1().Pods(c.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	return err
}

func (c *Client) DeletePod(ctx context.Context, taskID string) error {
	err := c.Core.CoreV1().Pods(c.Namespace).Delete(ctx, ResourceName(taskID), metav1.DeleteOptions{})
	return ignoreNotFound(err)
}

func resourcePtr(qty string) *resource.Quantity {
	q := resource.MustParse(qty)
	return &q
}

func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
