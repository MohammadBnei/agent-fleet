package k8s

import (
	"context"
	"fmt"
	"os"

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
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c.WorkspacePVC},
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

// GetPod returns the pod's current phase, or exists=false if it doesn't —
// Kubernetes itself is this fleet's durable source of truth for "is this
// session active," now that the provisioner holds no DB credentials
// (docs/adr/0020 point 1). A provisioner restart loses nothing: pod
// existence and phase survive it for free, no separate in-memory or
// Postgres-backed tracking needed.
func (c *Client) GetPod(ctx context.Context, name string) (phase corev1.PodPhase, exists bool, err error) {
	pod, err := c.Core.CoreV1().Pods(c.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return pod.Status.Phase, true, nil
}

// CreateWorkerPod builds the two-container pod (worker + sidecar,
// docs/adr/0020 point 5) for a claimed task. The worker never runs git
// itself — its worktree is already prepared by git.Manager before this is
// called (docs/adr/0019 point 2) — mounted into both containers via the
// same subPath convention e2e pods already use.
func (c *Client) CreateWorkerPod(ctx context.Context, taskID, repo, description, leaseID, baseBranch string) error {
	if baseBranch == "" {
		baseBranch = "main" // matches git.Manager.CreateWorktree's own default
	}
	name := WorkerResourceName(taskID)
	labels := WorkerLabels(taskID, repo)
	subPath := "worktrees/" + taskID

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "worker",
					Image: c.WorkerImage,
					Env: []corev1.EnvVar{
						{Name: "TASK_ID", Value: taskID},
						{Name: "TARGET_REPO", Value: repo},
						{Name: "TASK_DESCRIPTION", Value: description},
						{Name: "LEASE_ID", Value: leaseID},
						{Name: "BASE_BRANCH", Value: baseBranch},
						{Name: "SIDECAR_MCP_ADDR", Value: fmt.Sprintf("localhost:%d", SidecarMCPPort)},
						{Name: "SIDECAR_API_ADDR", Value: fmt.Sprintf("localhost:%d", SidecarAPIPort)},
						// The worker's own git push/gh pr create needs auth
						// independently of the provisioner's clone/fetch —
						// separate containers, only /workspace is shared, not
						// $HOME (see worker/src/index.ts's configureGitAuth).
						// Forwarded from the provisioner's own Infisical-sourced
						// env, same value.
						{Name: "GH_TOKEN", Value: os.Getenv("GH_TOKEN")},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workspace", MountPath: "/workspace", SubPath: subPath},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2000m"),
							corev1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
				{
					Name:  "sidecar",
					Image: c.SidecarImage,
					Env: []corev1.EnvVar{
						{Name: "TASK_ID", Value: taskID},
						{Name: "TARGET_REPO", Value: repo},
						{Name: "MCP_PORT", Value: fmt.Sprint(SidecarMCPPort)},
						{Name: "LOCAL_API_PORT", Value: fmt.Sprint(SidecarAPIPort)},
					},
					Ports: []corev1.ContainerPort{
						{Name: "mcp", ContainerPort: SidecarMCPPort},
						{Name: "local-api", ContainerPort: SidecarAPIPort},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workspace", MountPath: "/workspace", SubPath: subPath},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("50m"),
							corev1.ResourceMemory: resource.MustParse("64Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "workspace",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c.WorkspacePVC},
					},
				},
			},
		},
	}

	_, err := c.Core.CoreV1().Pods(c.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	return err
}

func (c *Client) DeleteWorkerPod(ctx context.Context, taskID string) error {
	err := c.Core.CoreV1().Pods(c.Namespace).Delete(ctx, WorkerResourceName(taskID), metav1.DeleteOptions{})
	return ignoreNotFound(err)
}

// GetWorkerPodRepo recovers which repo a worker pod belongs to from its
// own RepoLabel — the only way to know, since the provisioner holds no DB
// credentials to look it up any other way (docs/adr/0020 point 1). Needed
// by TearDownSession to remove the right worktree.
func (c *Client) GetWorkerPodRepo(ctx context.Context, taskID string) (repo string, exists bool, err error) {
	pod, err := c.Core.CoreV1().Pods(c.Namespace).Get(ctx, WorkerResourceName(taskID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return pod.Labels[RepoLabel], true, nil
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
