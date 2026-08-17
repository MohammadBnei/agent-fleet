// Package reconcile is a Kubernetes-native safety net (docs/adr/0020 point 1:
// the provisioner holds no Postgres credentials, so there is nothing external
// to reconcile pod state against).
//
// Two passes:
//
//   - gcTerminalWorkerJobs reports worker Jobs that reached a terminal phase,
//     and reaps the Succeeded ones. It is NOT just a fallback for a missed
//     teardown: with tasks.status gone (docs/adr/0048) this pass IS the
//     notification that a session finished, and a Succeeded Job reporting
//     nothing would leave pod_phase at RUNNING forever — which is what the
//     concurrency cap counts. A Failed Job is reported and then left for its
//     own TTL, because deleting it deletes the only record of why it failed.
//   - gcIdleSharedInstances reclaims shared Postgres/Redis instances that have
//     sat unused past a timeout (docs/adr/0034).
//
// A third pass swept e2e sandbox pods by age, because they had no natural
// "done" signal. It is gone with the sandbox (docs/adr/0048 §6) — a session's
// pod is a Job, and a Job's terminal phase is a real done signal.
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
	// ListPodsByLabel/DeleteAll backed gcDeadE2ePods and are gone with it
	// (docs/adr/0048 §6) — there are no e2e pods to list or reap.
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
	// reported is the set of sessions whose terminal phase has already been
	// reported. Only Failed Jobs can land in it more than once: they are no
	// longer deleted on sight (see gcTerminalWorkerJobs), so they stay listed
	// for their whole TTL and would otherwise re-report every tick. In-memory
	// on purpose — a provisioner restart re-reports once, which
	// coreserver.ReportPodEvents already treats as a no-op against a session
	// that is terminal.
	reported map[string]struct{}
}

func New(k8sc JobLister, core EventReporter, sharedInstanceIdleTimeout time.Duration) *Loop {
	return &Loop{
		k8sc:                      k8sc,
		core:                      core,
		sharedInstanceIdleTimeout: sharedInstanceIdleTimeout,
		reported:                  make(map[string]struct{}),
	}
}

func (l *Loop) gcTerminalWorkerJobs(ctx context.Context) {
	jobs, err := l.k8sc.ListWorkerJobsByLabel(ctx)
	if err != nil {
		slog.Error("reconcile: list worker jobs failed", "error", err)
		return
	}
	live := make(map[string]struct{}, len(jobs))
	for _, job := range jobs {
		live[job.TaskID] = struct{}{}
	}
	for id := range l.reported {
		if _, ok := live[id]; !ok {
			delete(l.reported, id) // TTL controller reaped it; forget it
		}
	}
	for _, job := range jobs {
		if job.Phase != "Succeeded" && job.Phase != "Failed" {
			continue
		}
		if _, done := l.reported[job.TaskID]; done {
			continue
		}
		l.reported[job.TaskID] = struct{}{}
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
		// A Failed Job is reported but deliberately NOT deleted: it is the
		// only surviving evidence of why the worker died, and deleting it
		// here destroyed that evidence before anything could record it.
		// Live, 2026-08-17: the pod died at 19:24:14 and this line deleted it
		// at 19:24:22, so kube-state-metrics never scraped a terminated
		// state, `kubectl describe` had nothing, and establishing that the
		// cause was an OOMKill needed the node's dmesg. The Job's own
		// TTLSecondsAfterFinished (k8s.workerJobTTLSeconds) reaps it instead
		// — that field exists for exactly this and had simply never been
		// allowed to run. A Succeeded Job carries no diagnosis worth keeping
		// and is still deleted on sight.
		if job.Phase == "Failed" {
			slog.Info("reconcile: keeping failed worker job for its TTL", "sessionId", job.TaskID, "jobName", job.JobName)
			continue
		}
		slog.Info("reconcile: gc'ing terminal worker job", "sessionId", job.TaskID, "jobName", job.JobName, "phase", job.Phase)
		if err := l.k8sc.DeleteWorkerJob(ctx, job.TaskID); err != nil {
			slog.Error("reconcile: delete worker job failed", "sessionId", job.TaskID, "error", err)
		}
		// The session's disk is deliberately untouched. A finished session
		// stays resumable — its working volume and SDK state are exactly what
		// a later warm needs — and the only thing that reclaims them is core's
		// retention GC, through SweepSession. Deleting them here would make
		// "the Job ended" mean "the work is gone", which is the opposite of
		// what a session is (docs/adr/0048).
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

// gcDeadE2ePods used to sit here, reaping e2e sandbox pods that had gone
// terminal or aged out. There are no e2e pods (docs/adr/0048 §6) — the agent's
// app runs in the session's own pod and is reaped with it by
// gcTerminalWorkerJobs.
//
// gcIdleSharedInstances stays: shared Postgres/Redis instances still exist,
// requested on demand via request_service rather than declared by a recipe,
// and an idle one still needs reclaiming.
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
