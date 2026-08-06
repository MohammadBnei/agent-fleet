package k8s

import (
	"context"
	"log/slog"

	batchv1 "k8s.io/api/batch/v1"
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
		slog.Error("k8s ListPodsByLabel", "error", err)
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
	if err := c.DeletePod(ctx, taskID); err != nil {
		return err
	}
	slog.Info("k8s DeleteAll", "taskId", taskID)
	return nil
}

type LiveWorkerJob struct {
	TaskID  string
	JobName string
	// Phase mirrors the terminal-phase strings reconcile.gcTerminalWorkerJobs
	// already compares against ("Succeeded"/"Failed") — derived from the
	// Job's own status.conditions (jobPhase, below) rather than a Pod's
	// status.phase, but kept as the same plain string so that comparison
	// (and reconcile's own tests) didn't need to change shape.
	Phase string
}

// jobPhase derives a Pod-phase-shaped terminal status from a Job's
// conditions (reliability-findings.md #11) — "" for anything not yet
// finished (Running/Pending have no Job-level equivalent worth
// distinguishing here; nothing currently needs that granularity).
func jobPhase(job *batchv1.Job) string {
	for _, cond := range job.Status.Conditions {
		if cond.Status != "True" {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete:
			return "Succeeded"
		case batchv1.JobFailed:
			return "Failed"
		}
	}
	return ""
}

// ListWorkerJobsByLabel is the reconcile loop's terminal-phase GC pass
// (docs/adr/0019/0020 — the provisioner holds no DB credentials, so
// "which worker jobs exist and what state are they in" comes straight
// from Kubernetes, not a Postgres-tracked list).
func (c *Client) ListWorkerJobsByLabel(ctx context.Context) ([]LiveWorkerJob, error) {
	list, err := c.Core.BatchV1().Jobs(c.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: ComponentLabel + "=" + ComponentWorker,
	})
	if err != nil {
		slog.Error("k8s ListWorkerJobsByLabel", "error", err)
		return nil, err
	}
	out := make([]LiveWorkerJob, 0, len(list.Items))
	for _, job := range list.Items {
		taskID := job.Labels[TaskIDLabel]
		if taskID == "" {
			continue
		}
		out = append(out, LiveWorkerJob{TaskID: taskID, JobName: job.Name, Phase: jobPhase(&job)})
	}
	return out, nil
}
