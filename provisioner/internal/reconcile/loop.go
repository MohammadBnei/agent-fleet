// Package reconcile is a pure Kubernetes-native safety net now
// (docs/adr/0020 point 1: the provisioner holds no Postgres credentials at
// all, so there's nothing external left to reconcile pod state against).
// Two jobs: garbage-collect worker pods that reached a terminal k8s phase
// (Succeeded/Failed) but weren't cleaned up by core's own TearDownSession
// call — a safety net for a missed/failed call, not the primary teardown
// mechanism (that's coreserver.SetTaskStatus's opportunistic trigger, in
// core/) — and, as of docs/adr/0034, garbage-collect shared environment-
// recipe service instances that have sat idle past a configured timeout.
// e2e pods still have no natural "done" signal from k8s alone (they run
// until explicitly killed) and no external tracking left to compare
// against, so they're not GC'd here — an accepted gap, not a silent one: a
// leaked e2e pod would need a different mechanism entirely, deferred as
// future scope.
package reconcile

import (
	"context"
	"log/slog"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
)

// JobLister is the narrow slice of k8s.Client this package needs — a
// hand-written fake satisfies it in tests, no live cluster required.
// Worker pods are now batch/v1.Jobs (reliability-findings.md #11) — Job's
// own TTLSecondsAfterFinished does most of what this pass used to GC by
// hand, but core's TearDownSession call (the primary teardown mechanism)
// can still miss a terminal Job, so this stays as the safety net for
// that case, on the same ticker-poll shape every other loop in this
// codebase uses (a client-go informer/watch was considered and rejected
// as more machinery than a shrunk safety-net pass justifies).
type JobLister interface {
	ListWorkerJobsByLabel(ctx context.Context) ([]k8s.LiveWorkerJob, error)
	DeleteWorkerJob(ctx context.Context, taskID string) error
	// ListSharedInstances/DeleteSharedInstance back gcIdleSharedInstances
	// (docs/adr/0034) — same "narrow slice of k8s.Client" reasoning as the
	// two worker-job methods above, just grown rather than split into a
	// second interface for two more methods.
	ListSharedInstances(ctx context.Context) ([]k8s.LiveSharedInstance, error)
	DeleteSharedInstance(ctx context.Context, repo, serviceKey string) error
}

// EventReporter is the narrow slice of coreclient.Client this package
// needs — a hand-written fake satisfies it in tests, no live gRPC
// connection to core required. Same method shape grpcserver.Server's own
// EventReporter already uses (a separate interface in a different
// package, not shared directly, to avoid a cross-package coupling neither
// side needs).
type EventReporter interface {
	ReportEvent(ctx context.Context, event *agentfleetv1.PodEvent)
}

type Loop struct {
	k8sc                      JobLister
	core                      EventReporter
	sharedInstanceIdleTimeout time.Duration
}

func New(k8sc JobLister, core EventReporter, sharedInstanceIdleTimeout time.Duration) *Loop {
	return &Loop{k8sc: k8sc, core: core, sharedInstanceIdleTimeout: sharedInstanceIdleTimeout}
}

func (l *Loop) gcTerminalWorkerJobs(ctx context.Context) {
	jobs, err := l.k8sc.ListWorkerJobsByLabel(ctx)
	if err != nil {
		slog.Error("reconcile: list worker jobs failed", "error", err)
		return
	}
	for _, job := range jobs {
		if job.Phase != "Succeeded" && job.Phase != "Failed" {
			continue
		}
		// Fast-path crash report (reliability-findings.md #1) — reported
		// before GC-ing the Job, on top of the heartbeat-reclaim fallback
		// core's own ClaimNextTask already has. core's own
		// coreserver.ReportPodEvents scopes MarkCrashed to a non-terminal
		// task, so this is a safe no-op if the task already reached a
		// terminal status through its own SetTaskStatus call first.
		if job.Phase == "Failed" {
			l.core.ReportEvent(ctx, &agentfleetv1.PodEvent{
				TaskId:  job.TaskID,
				Kind:    agentfleetv1.SessionKind_SESSION_KIND_WORKER,
				Phase:   agentfleetv1.PodPhase_POD_PHASE_CRASHED,
				PodName: job.JobName,
				Message: "worker job reached a terminal Failed phase",
			})
		}
		slog.Info("reconcile: gc'ing terminal worker job", "taskId", job.TaskID, "jobName", job.JobName, "phase", job.Phase)
		if err := l.k8sc.DeleteWorkerJob(ctx, job.TaskID); err != nil {
			slog.Error("reconcile: delete worker job failed", "taskId", job.TaskID, "error", err)
		}
		// Worktree cleanup is intentionally skipped here — this pass exists
		// for the case where core's TearDownSession call (which does clean
		// the worktree) never fired; the job's RepoLabel would still tell us
		// the repo, but a genuinely orphaned worktree with no task record
		// left to explain it is a small enough leak (one directory, on a
		// path keyed by a task ID that's already terminal) not to add a
		// second GC path on top of the job cleanup that actually matters.
	}
}

// gcIdleSharedInstances deletes any shared environment-recipe service
// instance (docs/adr/0034) that hasn't been touched (EnsureSharedInstance
// updates last-used-at on every call, first-use and every reuse alike) in
// over sharedInstanceIdleTimeout. Deliberately uniform across task-scoped
// and repo-scoped instances — no special-casing repo-scoped as
// "protected": re-provisioning is cheap via the same idempotent
// EnsureSharedInstance path that created it the first time, so correctness
// rests on that path being solid, not on indefinite preservation.
func (l *Loop) gcIdleSharedInstances(ctx context.Context) {
	instances, err := l.k8sc.ListSharedInstances(ctx)
	if err != nil {
		slog.Error("reconcile: list shared instances failed", "error", err)
		return
	}
	for _, inst := range instances {
		if inst.LastUsedAt.IsZero() {
			continue // no annotation yet — likely mid-creation, don't sweep prematurely
		}
		if time.Since(inst.LastUsedAt) < l.sharedInstanceIdleTimeout {
			continue
		}
		slog.Info("reconcile: gc'ing idle shared instance", "repo", inst.Repo, "serviceKey", inst.ServiceKey, "lastUsedAt", inst.LastUsedAt)
		if err := l.k8sc.DeleteSharedInstance(ctx, inst.Repo, inst.ServiceKey); err != nil {
			slog.Error("reconcile: delete shared instance failed", "repo", inst.Repo, "serviceKey", inst.ServiceKey, "error", err)
		}
	}
}

func (l *Loop) Run(ctx context.Context, pollInterval time.Duration) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		l.gcTerminalWorkerJobs(ctx)
		l.gcIdleSharedInstances(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
