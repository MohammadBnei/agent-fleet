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

// jobPhase derives a Pod-phase-shaped status from a Job's own status
// (reliability-findings.md #11) — "" only while the Job has neither finished
// nor got a ready pod.
//
// "Running" is not cosmetic here: core's reconcile loop maps exactly that
// string onto POD_PHASE_RUNNING, and it was the only consumer of a phase this
// function never produced. So no session in the fleet's history ever reached
// RUNNING — every healthy one sat at SCHEDULED for its whole life, which is
// the bug core's own comment says the upward sync exists to fix. Its test
// passed because the fake returned "Running" and the real thing never did.
//
// Ready rather than Active: Active counts pending pods too, so it would
// report RUNNING while the image is still pulling.
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
	if job.Status.Ready != nil && *job.Status.Ready > 0 {
		return "Running"
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
		// A Job being deleted is not a live pod, and DeleteWorkerJob uses
		// FOREGROUND propagation on purpose — the Job object stays listed,
		// finalizer and all, until its Pod is gone. Core reconciles pod_phase
		// against this answer, so reporting a dying Job as live makes it write
		// RUNNING back over the TERMINATED the same teardown just reported.
		// The row then holds a concurrency slot, and for as long as it does,
		// WarmIfIdle no-ops on the live phase while PostMessage appends the
		// human's message to a session with no pod to read it.
		if job.DeletionTimestamp != nil {
			continue
		}
		out = append(out, LiveWorkerJob{TaskID: taskID, JobName: job.Name, Phase: jobPhase(&job)})
	}
	return out, nil
}
