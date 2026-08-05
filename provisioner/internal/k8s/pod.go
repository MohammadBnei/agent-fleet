package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// workerJobTTLSeconds is how long a finished worker Job (and the pod it
// owns) sticks around before Kubernetes' TTL controller garbage-collects
// it — reliability-findings.md #11: this replaces the reconcile loop's own
// hand-rolled terminal-phase GC pass. Long enough to `kubectl logs` a
// crashed worker, short enough not to accumulate.
const workerJobTTLSeconds = 300

func int32Ptr(i int32) *int32 { return &i }

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
	if err != nil {
		slog.Error("k8s CreatePod", "taskId", task.ID, "error", err)
		return err
	}
	slog.Info("k8s CreatePod", "taskId", task.ID)
	return nil
}

func (c *Client) DeletePod(ctx context.Context, taskID string) error {
	err := ignoreNotFound(c.Core.CoreV1().Pods(c.Namespace).Delete(ctx, ResourceName(taskID), metav1.DeleteOptions{}))
	if err != nil {
		slog.Error("k8s DeletePod", "taskId", taskID, "error", err)
		return err
	}
	slog.Info("k8s DeletePod", "taskId", taskID)
	return nil
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
		slog.Error("k8s GetPod", "name", name, "error", err)
		return "", false, err
	}
	return pod.Status.Phase, true, nil
}

// CreateWorkerPod builds the two-container pod (worker + sidecar,
// docs/adr/0020 point 5) for a claimed task, wrapped in a batch/v1.Job
// (reliability-findings.md #11) rather than a bare Pod — retry/backoff
// and terminal-state GC come from Job's own semantics instead of the
// reconcile loop hand-rolling them. BackoffLimit is deliberately 0: core's
// heartbeat/reclaim (ClaimNextTask) stays the sole retry mechanism
// (ADR-0020 pt.2 — the provisioner never decides pod lifecycle on its
// own), a k8s-level retry here would silently double up against it.
//
// e2e-preview pods (CreatePod, below) deliberately stay bare Pods: they're
// long-running/interactive, killed explicitly via KillE2eSession, not
// run-to-completion — Job's retry/backoff/TTL semantics don't apply to a
// workload with no expected completion, and (per reconcile/loop.go's own
// doc comment) they were never part of the hand-rolled GC this finding is
// about in the first place.
func (c *Client) CreateWorkerPod(ctx context.Context, taskID, repo, description, leaseID, baseBranch, worktreePath string) error {
	if baseBranch == "" {
		baseBranch = "main" // matches git.Manager.CreateWorktree's own default
	}
	name := WorkerResourceName(taskID)
	labels := WorkerLabels(taskID, repo)

	sidecarRestartAlways := corev1.ContainerRestartPolicyAlways

	podSpec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		// sidecar runs as a native sidecar (K8s 1.29+, this cluster is
		// v1.35): an init container with RestartPolicy Always plus a
		// StartupProbe, so kubelet blocks starting the worker container
		// until the sidecar is actually accepting connections. Without
		// this, both containers start concurrently and the worker's
		// first sidecar call can lose the race (observed live: worker
		// crashed 6ms after start, ~7s before the sidecar logged
		// "listening").
		InitContainers: []corev1.Container{
			{
				Name:          "sidecar",
				Image:         c.SidecarImage,
				RestartPolicy: &sidecarRestartAlways,
				Env: []corev1.EnvVar{
					{Name: "TASK_ID", Value: taskID},
					{Name: "TARGET_REPO", Value: repo},
					{Name: "MCP_PORT", Value: fmt.Sprint(SidecarMCPPort)},
					{Name: "LOCAL_API_PORT", Value: fmt.Sprint(SidecarAPIPort)},
					{Name: "WORKTREE_PATH", Value: worktreePath},
					{Name: "LOG_LEVEL", Value: c.LogLevel},
				},
				Ports: []corev1.ContainerPort{
					{Name: "mcp", ContainerPort: SidecarMCPPort},
					{Name: "local-api", ContainerPort: SidecarAPIPort},
				},
				VolumeMounts: []corev1.VolumeMount{
					// Whole PVC, not a per-task SubPath: a linked git
					// worktree's .git file is an absolute-path gitlink
					// back to the main clone's repos/<repo>/.git/worktrees/
					// <taskId> admin dir (HEAD/index/commondir) — a
					// SubPath scoped to just worktrees/<taskId> cuts that
					// path off entirely, so every git command in this
					// container failed with "not a git repository"
					// (produced by design in ADR-0019 but never checked
					// against how linked worktrees actually work).
					{Name: "workspace", MountPath: "/workspace"},
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
				StartupProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(SidecarAPIPort)},
					},
					PeriodSeconds:    1,
					FailureThreshold: 30,
				},
			},
		},
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
					{Name: "WORKTREE_PATH", Value: worktreePath},
					{Name: "LOG_LEVEL", Value: c.LogLevel},
				},
				VolumeMounts: []corev1.VolumeMount{
					// Whole PVC, not a per-task SubPath — see the sidecar
					// container's identical mount above for why.
					{Name: "workspace", MountPath: "/workspace"},
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
		},
		Volumes: []corev1.Volume{
			{
				Name: "workspace",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c.WorkspacePVC},
				},
			},
		},
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			TTLSecondsAfterFinished: int32Ptr(workerJobTTLSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       podSpec,
			},
		},
	}

	_, err := c.Core.BatchV1().Jobs(c.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		slog.Error("k8s CreateWorkerPod", "taskId", taskID, "repo", repo, "error", err)
		return err
	}
	slog.Info("k8s CreateWorkerPod", "taskId", taskID, "repo", repo)
	return nil
}

// jobForegroundDeletion cascades to the Job's own Pod synchronously —
// without it, the default background propagation can leave the Pod
// visible for a moment after DeleteWorkerJob returns.
func jobForegroundDeletion() metav1.DeleteOptions {
	policy := metav1.DeletePropagationForeground
	return metav1.DeleteOptions{PropagationPolicy: &policy}
}

func (c *Client) DeleteWorkerJob(ctx context.Context, taskID string) error {
	err := ignoreNotFound(c.Core.BatchV1().Jobs(c.Namespace).Delete(ctx, WorkerResourceName(taskID), jobForegroundDeletion()))
	if err != nil {
		slog.Error("k8s DeleteWorkerJob", "taskId", taskID, "error", err)
		return err
	}
	slog.Info("k8s DeleteWorkerJob", "taskId", taskID)
	return nil
}

// GetWorkerJobRepo recovers which repo a worker job belongs to from its
// own RepoLabel — the only way to know, since the provisioner holds no DB
// credentials to look it up any other way (docs/adr/0020 point 1). Needed
// by TearDownSession to remove the right worktree.
func (c *Client) GetWorkerJobRepo(ctx context.Context, taskID string) (repo string, exists bool, err error) {
	job, err := c.Core.BatchV1().Jobs(c.Namespace).Get(ctx, WorkerResourceName(taskID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		slog.Error("k8s GetWorkerJobRepo", "taskId", taskID, "error", err)
		return "", false, err
	}
	return job.Labels[RepoLabel], true, nil
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
