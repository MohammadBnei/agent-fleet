package k8s

import (
	"context"
	"log/slog"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func createOpts() metav1.CreateOptions { return metav1.CreateOptions{} }

func deleteOpts() metav1.DeleteOptions { return metav1.DeleteOptions{} }

// LiveE2ePod, ListPodsByLabel and DeleteAll used to live here. All three
// existed to track and tear down the e2e sandbox pod — its Service, its
// Middleware, its IngressRoute and its per-task NetworkPolicy, removed
// together because every teardown path funnelled through one call.
//
// There is no sandbox (docs/adr/0048 §6). A session has one pod, its Job owns
// it, and deleting the Job is the whole of tearing it down. The only extra
// objects a session can have are the Service and route that expose() creates
// on demand, and UnexposeSession removes exactly those two.

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
