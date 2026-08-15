package sessions

import (
	"context"
	"log/slog"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/metrics"
)

// Provisioner is the narrow slice of provisionerclient this package needs, so
// the loops are testable with a hand-written fake and no gRPC connection.
type Provisioner interface {
	ListWorkerPods(ctx context.Context) (map[string]string, error) // sessionID -> phase
	TearDownWorker(ctx context.Context, sessionID string) error
	SweepSession(ctx context.Context, sessionID string) error
}

// Loop replaces core/internal/dispatch. What is gone is the dispatching:
// there is no queue to poll and nothing to claim, because a session's first
// message provisions its pod directly (docs/adr/0048).
//
// What remains is everything that was never really about dispatch — noticing
// that reality has diverged from the database, and reclaiming what nobody is
// using:
//
//   - reconcile: pod_phase vs what Kubernetes actually has
//   - stop grace: a cooperative abort that went unhonored
//   - startup stall: a pod that came up and never said anything
//   - idle timeout: a live pod nobody is talking to
//   - retention GC: disk belonging to sessions nobody has touched in weeks
type Loop struct {
	sessions    *Store
	provisioner Provisioner

	stopGrace    time.Duration
	startupStall time.Duration
	idleTimeout  time.Duration
	retention    time.Duration
	// turnStall is DeriveLiveState's threshold for `stalled`, needed here only
	// to compute the gauge — the sweeps above all key off their own timeouts.
	turnStall time.Duration
}

func NewLoop(store *Store, p Provisioner, stopGrace, startupStall, idleTimeout, retention, turnStall time.Duration) *Loop {
	return &Loop{
		sessions:     store,
		provisioner:  p,
		stopGrace:    stopGrace,
		startupStall: startupStall,
		idleTimeout:  idleTimeout,
		retention:    retention,
		turnStall:    turnStall,
	}
}

// Run ticks until ctx is done. One goroutine, sequential passes — there is no
// contention to exploit and a serial loop is one less thing to reason about
// when a pass misbehaves.
func (l *Loop) Run(ctx context.Context, interval time.Duration) {
	slog.Info("sessions loop: started", "interval", interval,
		"stopGrace", l.stopGrace, "startupStall", l.startupStall,
		"idleTimeout", l.idleTimeout, "retention", l.retention)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		l.tick(ctx)
		select {
		case <-ctx.Done():
			slog.Info("sessions loop: stopped")
			return
		case <-ticker.C:
		}
	}
}

func (l *Loop) tick(ctx context.Context) {
	l.reconcilePodPhases(ctx)
	l.enforceStopGrace(ctx)
	l.enforceStartupStall(ctx)
	l.enforceIdleTimeout(ctx)
	l.collectExpired(ctx)
	l.publishLiveStateGauge(ctx)
}

// publishLiveStateGauge is the one fleet fact neither Loki nor
// kube-state-metrics can answer: how many sessions are in each live state
// right now, which is what makes "N sessions blocked for 30m" alertable
// (docs/adr/0047).
//
// It was previously unreachable. CountByRepoLiveState was written, the gauge
// was declared, and nothing called either — so agentfleet_tasks_current has
// been exporting an empty series set. Wired here rather than on its own timer
// because this loop already runs on the cadence the gauge wants, and a second
// ticker over the same table would be two answers that can disagree.
func (l *Loop) publishLiveStateGauge(ctx context.Context) {
	counts, err := l.sessions.CountByRepoLiveState(ctx, l.turnStall)
	if err != nil {
		slog.Error("sessions loop: live-state count failed", "error", err)
		return
	}
	metrics.SetTasksCurrent(counts)
}

// reconcilePodPhases is what replaces the 30-second heartbeat.
//
// Pod events are pushed, and a push can be dropped — most plausibly while
// core is restarting, which is exactly when a deploy is happening. A dropped
// TERMINATED leaves a finished session at pod_phase RUNNING forever, and
// since that is what the concurrency cap counts, five of them wedge the fleet
// with no error anywhere. The heartbeat was the old backstop for that, but it
// only ever proved a timer inside the worker was still firing.
//
// Asking Kubernetes is strictly better: it is the thing that actually knows,
// and it cannot be wrong in the same direction as the pod it describes.
func (l *Loop) reconcilePodPhases(ctx context.Context) {
	live, err := l.provisioner.ListWorkerPods(ctx)
	if err != nil {
		slog.Error("sessions loop: list worker pods failed", "error", err)
		return
	}

	rows, err := l.sessions.List(ctx, 500, "")
	if err != nil {
		slog.Error("sessions loop: list sessions failed", "error", err)
		return
	}

	for _, s := range rows {
		if !IsPodPhaseLive(s.PodPhase) {
			continue
		}
		// PROVISIONING and CREATED mean the provisioner is still mid-call and
		// has NOT created the Job yet — those phases are reported before the
		// clone, the fleet-shared sync and the pod create, which take tens of
		// seconds together. "Kubernetes has no Job" is therefore the expected
		// answer for them, not evidence of an orphan.
		//
		// Without this the loop raced every warm it happened to tick during
		// and wrote TERMINATED over a session whose pod was seconds from
		// existing (observed live 2026-08-15, 3s after a warm). That write is
		// not self-healing: TERMINATED is not a live phase, so the next pass
		// skips the row at the check above and nothing ever corrects it. The
		// session showed no pod while its agent was running.
		//
		// A session genuinely stuck in these phases is still caught, by
		// enforceStartupStall — activity_seen stays false, which is exactly
		// what that sweep looks for.
		// PodPhase is non-nil here: IsPodPhaseLive above returns false for nil.
		if *s.PodPhase == "POD_PHASE_PROVISIONING" || *s.PodPhase == "POD_PHASE_CREATED" {
			continue
		}
		if k8sPhase, stillThere := live[s.ID]; stillThere {
			// The pod exists. Reconcile UPWARDS too, not just downwards.
			//
			// Nothing else ever reports RUNNING: the provisioner reports
			// SCHEDULED when it creates the Job and then only CRASHED or
			// TERMINATED when the Job ends. A perfectly healthy session
			// therefore sat at SCHEDULED for its entire life, and the
			// dashboard said "SCHEDULED" for a session that had been talking
			// to a human for an hour. Kubernetes already tells us the real
			// phase in the same call used to detect absence.
			if want := podPhaseFromK8s(k8sPhase); want != "" && (s.PodPhase == nil || *s.PodPhase != want) {
				if err := l.sessions.SetPodPhase(ctx, s.ID, want, ""); err != nil {
					slog.Error("sessions loop: phase sync failed", "sessionId", s.ID, "error", err)
				}
			}
			continue
		}
		// The row claims a live pod; Kubernetes has none. The pod's own
		// terminal event never arrived.
		slog.Warn("sessions loop: reconciling orphaned pod_phase",
			"sessionId", s.ID, "wasPhase", *s.PodPhase)
		if err := l.sessions.SetPodPhase(ctx, s.ID, "POD_PHASE_TERMINATED",
			"reconciled: no live worker Job — its terminal event was lost"); err != nil {
			slog.Error("sessions loop: reconcile write failed", "sessionId", s.ID, "error", err)
			continue
		}
	}
}

// podPhaseFromK8s maps a Kubernetes pod phase onto the fleet's own enum.
//
// Only the phases that mean something different from what the row already
// says are mapped. Pending is deliberately absent: a Pending pod is one this
// loop was told about at creation as SCHEDULED, and rewriting SCHEDULED to
// SCHEDULED every 60s is churn. Failed/Succeeded are absent for a different
// reason — the provisioner's own GC reports those with a real message, and
// this loop would overwrite that message with nothing.
func podPhaseFromK8s(phase string) string {
	if phase == "Running" {
		return "POD_PHASE_RUNNING"
	}
	return ""
}

// enforceStopGrace force-tears-down a pod that ignored its cooperative abort.
//
// This is the only thing that can kill a runaway agent, and that is exactly
// why it survives the sweep-deletion elsewhere in docs/adr/0048: a runaway
// keeps appending transcript entries, so last_active_at keeps moving and the
// idle timeout below never fires for it.
func (l *Loop) enforceStopGrace(ctx context.Context) {
	ids, err := l.sessions.ListOverdueStopIDs(ctx, l.stopGrace)
	if err != nil {
		slog.Error("sessions loop: list overdue stops failed", "error", err)
		return
	}
	for _, id := range ids {
		slog.Warn("sessions loop: stop grace expired, forcing teardown", "sessionId", id, "grace", l.stopGrace)
		l.tearDown(ctx, id)
	}
}

// enforceStartupStall catches a pod that came up and never said anything
// (docs/adr/0040). Gated on activity_seen, which ReserveSlot resets per pod —
// see the comment there for why it cannot be derived from the transcript.
func (l *Loop) enforceStartupStall(ctx context.Context) {
	ids, err := l.sessions.ListStartupStalledIDs(ctx, l.startupStall)
	if err != nil {
		slog.Error("sessions loop: list startup-stalled failed", "error", err)
		return
	}
	for _, id := range ids {
		slog.Warn("sessions loop: pod never reported activity, tearing down", "sessionId", id, "after", l.startupStall)
		// Recorded so the dashboard can say why, rather than showing a
		// session that silently lost its pod.
		if err := l.sessions.SetLastError(ctx, id,
			"pod started but never produced any output — torn down by the startup-stall guard"); err != nil {
			slog.Warn("sessions loop: set last error failed", "sessionId", id, "error", err)
		}
		l.tearDown(ctx, id)
	}
}

// enforceIdleTimeout reclaims a live pod nobody is talking to. The session
// itself survives — its directory and SDK state are untouched, so warming it
// again resumes exactly where it left off.
func (l *Loop) enforceIdleTimeout(ctx context.Context) {
	ids, err := l.sessions.ListIdleSessionIDs(ctx, l.idleTimeout)
	if err != nil {
		slog.Error("sessions loop: list idle failed", "error", err)
		return
	}
	for _, id := range ids {
		slog.Info("sessions loop: idle timeout, tearing down pod", "sessionId", id, "after", l.idleTimeout)
		l.tearDown(ctx, id)
	}
}

// collectExpired reclaims the disk of sessions nobody has touched in the
// retention window: the working directory and the per-session SDK state.
//
// The row and its transcript survive — a swept session is readable history,
// just not resumable. Only an explicit human delete removes those, because
// the transcript is the only record of a session that produced no PR.
//
// core is the single writer of swept_at, which is what lets the dashboard
// trust it enough to hide the Warm button. Under the old design that claim
// would have been false: the provisioner's own branch sweep deleted worktrees
// on an independent ticker without telling anyone, so a session could lose
// its disk with nothing recording the fact.
func (l *Loop) collectExpired(ctx context.Context) {
	ids, err := l.sessions.ListRetentionExpired(ctx, l.retention)
	if err != nil {
		slog.Error("sessions loop: list retention-expired failed", "error", err)
		return
	}
	for _, id := range ids {
		// No repo lookup before the sweep any more: both things being deleted
		// — the working-tree PVC and the SDK state directory — are keyed by
		// session alone, and there is no branch left for the fleet to consider
		// deleting alongside them (docs/adr/0048 §5).
		if err := l.provisioner.SweepSession(ctx, id); err != nil {
			// Do NOT mark swept — the disk is still there. Retrying next tick
			// is correct; recording a sweep that did not happen would tell
			// the dashboard a resumable session is gone.
			slog.Error("sessions loop: sweep failed", "sessionId", id, "error", err)
			continue
		}
		if err := l.sessions.MarkSwept(ctx, id); err != nil {
			slog.Error("sessions loop: mark swept failed", "sessionId", id, "error", err)
			continue
		}
		slog.Info("sessions loop: swept session disk", "sessionId", id, "after", l.retention)
	}
}

// tearDown removes a session's pod. Best-effort and logged rather than
// propagated: TearDownSession's contract is that tearing down something
// already gone is a correct no-op, so a failure here means a transient
// provisioner problem the next tick retries.
//
// It used to tear down two things — the worker pod and the session's e2e
// sandbox. There is one pod now (docs/adr/0048 §6), and whatever it published
// via expose() is removed by the provisioner alongside the Job.
func (l *Loop) tearDown(ctx context.Context, id string) {
	if err := l.provisioner.TearDownWorker(ctx, id); err != nil {
		slog.Warn("sessions loop: teardown failed", "sessionId", id, "error", err)
	}
}
