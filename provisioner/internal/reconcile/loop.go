// Package reconcile is a pure Kubernetes-native safety net now
// (docs/adr/0020 point 1: the provisioner holds no Postgres credentials at
// all, so there's nothing external left to reconcile pod state against).
// Two jobs: garbage-collect worker pods that reached a terminal k8s phase
// (Succeeded/Failed) but weren't cleaned up by core's own TearDownSession
// call — a safety net for a missed/failed call, not the primary teardown
// mechanism (that's coreserver.SetTaskStatus's opportunistic trigger, in
// core/) — and, as of docs/adr/0034, garbage-collect shared environment-
// recipe service instances that have sat idle past a configured timeout.
// As of docs/adr/0044 there is a third pass: e2e sandbox pods. They still
// have no natural "done" signal from k8s (they run until explicitly killed)
// and the provisioner still has no external tracking to compare against, so
// the sweep uses the two signals Kubernetes does provide — a terminal phase,
// and age past e2eMaxAge. That's coarser than the worker Jobs' real done
// signal, and deliberately so: the cost of leaking these is the NEXT sandbox
// sitting Pending forever against a full node, which presents to a human as
// "the e2e pod won't start" and was one of the motivating symptoms.
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
	// ListPodsByLabel/DeleteAll back gcDeadE2ePods (docs/adr/0044) — same
	// "grown, not split" reasoning as the shared-instance pair above.
	// ListPodsByLabel already existed and had no caller; DeleteAll (rather
	// than DeletePod) so the Service/Middleware/IngressRoute go with it.
	ListPodsByLabel(ctx context.Context) ([]k8s.LiveE2ePod, error)
	DeleteAll(ctx context.Context, taskID string) error
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
	e2eMaxAge                 time.Duration
}

func New(k8sc JobLister, core EventReporter, sharedInstanceIdleTimeout, e2eMaxAge time.Duration) *Loop {
	return &Loop{
		k8sc:                      k8sc,
		core:                      core,
		sharedInstanceIdleTimeout: sharedInstanceIdleTimeout,
		e2eMaxAge:                 e2eMaxAge,
	}
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
		// Terminal-phase report (reliability-findings.md #1) — reported
		// before GC-ing the Job. core's own coreserver.ReportPodEvents
		// scopes MarkCrashed to a non-terminal task, so this is a safe
		// no-op if the task already reported the same phase itself.
		//
		// BOTH terminal phases report, not just Failed. A Succeeded Job used
		// to be deleted silently, on the reasoning that a worker that exits
		// cleanly writes its own terminal status first — which made this
		// pass a fallback for crashes only. That reasoning depended on
		// tasks.status existing: terminal status was the sole trigger for
		// TearDownSession, and the sole thing that stopped a finished
		// session counting against the concurrency cap. With status gone
		// (docs/adr/0048) this loop IS the notification, and a Succeeded
		// Job that reports nothing leaves pod_phase at RUNNING forever —
		// which CountLivePods counts, wedging the fleet after five
		// successful sessions and staying invisible until the sixth.
		phase := agentfleetv1.PodPhase_POD_PHASE_TERMINATED
		message := "worker job reached a terminal Succeeded phase"
		if job.Phase == "Failed" {
			phase = agentfleetv1.PodPhase_POD_PHASE_CRASHED
			message = "worker job reached a terminal Failed phase"
		}
		l.core.ReportEvent(ctx, &agentfleetv1.PodEvent{
			SessionId: job.TaskID,
			Kind:      agentfleetv1.SessionKind_SESSION_KIND_WORKER,
			Phase:     phase,
			PodName:   job.JobName,
			Message:   message,
		})
		slog.Info("reconcile: gc'ing terminal worker job", "sessionId", job.TaskID, "jobName", job.JobName, "phase", job.Phase)
		if err := l.k8sc.DeleteWorkerJob(ctx, job.TaskID); err != nil {
			slog.Error("reconcile: delete worker job failed", "sessionId", job.TaskID, "error", err)
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

// gcDeadE2ePods deletes e2e sandbox pods that reached a terminal phase, and
// those older than e2eMaxAge (docs/adr/0044).
//
// The terminal-phase half should be close to dead code after 0044 — the
// entrypoint no longer lets an app crash take the pod with it, and
// CreateE2ESession replaces a corpse on the next request. It stays for the
// cases the container can't survive at all: OOMKilled, eviction, node loss.
// The age half is the one that actually reclaims capacity, since a healthy
// sandbox now runs until something deletes it.
func (l *Loop) gcDeadE2ePods(ctx context.Context) {
	pods, err := l.k8sc.ListPodsByLabel(ctx)
	if err != nil {
		slog.Error("reconcile: list e2e pods failed", "error", err)
		return
	}
	for _, pod := range pods {
		terminal := pod.Phase == "Succeeded" || pod.Phase == "Failed"
		aged := !pod.CreatedAt.IsZero() && time.Since(pod.CreatedAt) > l.e2eMaxAge
		if !terminal && !aged {
			continue
		}
		slog.Info("reconcile: gc'ing e2e pod", "sessionId", pod.TaskID, "podName", pod.PodName,
			"phase", pod.Phase, "createdAt", pod.CreatedAt, "reason", gcReason(terminal))
		// DeleteAll, not DeletePod: the Service/Middleware/IngressRoute are
		// this task's too, and unlike the recreate path in grpcserver there is
		// no follow-up create to leave them standing for.
		if err := l.k8sc.DeleteAll(ctx, pod.TaskID); err != nil {
			slog.Error("reconcile: delete e2e pod failed", "sessionId", pod.TaskID, "error", err)
			continue
		}
		// Reported so the deletion lands in the knowledge journal — an agent
		// whose sandbox vanished mid-session otherwise has no way to find out
		// why run_command started failing.
		l.core.ReportEvent(ctx, &agentfleetv1.PodEvent{
			SessionId: pod.TaskID,
			Kind:      agentfleetv1.SessionKind_SESSION_KIND_E2E,
			Phase:     agentfleetv1.PodPhase_POD_PHASE_TERMINATED,
			PodName:   pod.PodName,
			Message:   "e2e sandbox pod garbage-collected: " + gcReason(terminal),
		})
	}
}

func gcReason(terminal bool) string {
	if terminal {
		return "terminal phase"
	}
	return "older than the configured max age"
}

func (l *Loop) Run(ctx context.Context, pollInterval time.Duration) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		l.gcTerminalWorkerJobs(ctx)
		l.gcIdleSharedInstances(ctx)
		l.gcDeadE2ePods(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
