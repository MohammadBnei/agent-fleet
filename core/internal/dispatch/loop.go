// Package dispatch is core's own claim-then-command loop (docs/adr/0020
// point 2): watches for pending tasks and free concurrency headroom, claims
// a task (tasks.Store.ClaimNextTask, the SKIP LOCKED transaction — never
// exposed as an RPC the provisioner calls), then commands the provisioner
// to spawn a worker pod for it. The provisioner never claims tasks or
// decides on its own to spawn — it only ever does what core tells it,
// matching how e2e-provisioner already behaved before this rewrite.
package dispatch

import (
	"context"
	"log/slog"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// Loop owns the claim-then-command cycle, plus the stop-grace-period
// enforcement sweep below (see enforceStopGrace) — both share the same
// tasks.Store/provisionerclient.Client pair and the same periodic ticker,
// so a second loop/package for the sweep would just be more wiring around
// the same two dependencies.
type Loop struct {
	tasks          *tasks.Store
	transcr        transcript.Store
	repos          *repos.Store
	provisioner    *provisionerclient.Client
	maxInFlight    int
	maxTaskRetries int
	stopGrace      time.Duration
	idleTimeout    time.Duration
	nudge          chan struct{}
}

func New(taskStore *tasks.Store, transcr transcript.Store, repoStore *repos.Store, provisioner *provisionerclient.Client, maxInFlight, maxTaskRetries int, stopGrace, idleTimeout time.Duration) *Loop {
	return &Loop{
		tasks: taskStore, transcr: transcr, repos: repoStore, provisioner: provisioner,
		maxInFlight: maxInFlight, maxTaskRetries: maxTaskRetries,
		stopGrace:   stopGrace,
		idleTimeout: idleTimeout,
		nudge:       make(chan struct{}, 1),
	}
}

// Nudge triggers an immediate claim attempt instead of waiting for the
// next tick (reliability-findings.md #5) — non-blocking, safe to call from
// any goroutine. tasks.Store.CreateTask/SetStatus/MarkCrashed all call this
// right after their own commit, covering task creation, capacity freed up
// by a terminal status, and the crash fast-path. The ticker in Run stays
// as a fallback: recovery for a dropped nudge, plus the one case that
// still has no write to nudge on — a worker that vanishes without ever
// calling MarkCrashed (e.g. OOM-killed), caught only by the passive
// 10-minute heartbeat-staleness scan.
func (l *Loop) Nudge() {
	select {
	case l.nudge <- struct{}{}:
	default:
	}
}

// Run polls every pollInterval until ctx is cancelled. Errors are logged
// and the loop continues — a transient DB or provisioner hiccup shouldn't
// stop dispatch for every other task waiting behind it.
func (l *Loop) Run(ctx context.Context, pollInterval time.Duration) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	// time.Ticker doesn't fire at t=0 — without this, a core restart with
	// tasks already sitting `pending` in Postgres waits up to pollInterval
	// before the very first check. The nudge channel is in-memory, not
	// durable, so it has no memory of anything that happened before this
	// process started; only Postgres does.
	l.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.tick(ctx)
		case <-l.nudge:
			l.tick(ctx)
		}
	}
}

func (l *Loop) tick(ctx context.Context) {
	l.enforceStopGrace(ctx)
	l.enforceIdleTimeout(ctx)

	// The concurrency-headroom check lives inside ClaimNextTask's own query
	// now (reliability-findings.md #6) — a separate CountInFlight call
	// beforehand was a TOCTOU race under >1 dispatch-loop replica.
	task, err := l.tasks.ClaimNextTask(ctx, l.maxInFlight, l.maxTaskRetries)
	if err != nil {
		slog.Error("dispatch: claim failed", "error", err)
		return
	}
	if task == nil {
		return // nothing eligible right now
	}
	// A claim succeeded — immediately re-check for another eligible task
	// instead of waiting for the next poll tick. Nudge's buffer is size 1,
	// so a burst of N simultaneous SetStatus/MarkCrashed calls arriving
	// while this tick is already running only ever guarantees one more
	// tick after it, and tick only claims one task at a time — without
	// this self-nudge, a real backlog would trickle out one claim per
	// pollInterval instead of draining immediately.
	l.Nudge()

	repoCfg, err := l.repos.Get(ctx, task.Repo)
	if err != nil {
		slog.Error("dispatch: repo lookup failed", "taskId", task.ID, "repo", task.Repo, "error", err)
		return
	}
	if repoCfg == nil {
		slog.Error("dispatch: claimed task for unknown repo, cannot dispatch", "taskId", task.ID, "repo", task.Repo)
		return
	}

	resumeSessionID := ""
	if task.SessionID != nil {
		resumeSessionID = *task.SessionID
	}
	resumeFromSeq, err := l.transcr.LatestSeq(ctx, task.ID)
	if err != nil {
		slog.Error("dispatch: latest seq lookup failed", "taskId", task.ID, "error", err)
		return
	}
	podName, err := l.provisioner.CreateWorkerPod(ctx, task.ID, task.Repo, repoCfg.URL, repoCfg.BaseBranch, task.Description, task.LeaseID, resumeSessionID, resumeFromSeq)
	if err != nil {
		slog.Error("dispatch: CreateWorkerPod failed", "taskId", task.ID, "error", err)
		return
	}
	slog.Info("dispatch: worker pod created", "taskId", task.ID, "repo", task.Repo, "podName", podName)
}

// enforceStopGrace force-tears-down any task whose stop was requested
// (DashboardService.Stop, via tasks.Store.MarkStopRequested) more than
// stopGrace ago and still hasn't reached a terminal status — the fix for
// a hung/crashed/unreachable worker pod that never noticed the cooperative
// abort message Stop also posts. Mirrors the exact two-call TearDownSession
// pattern DashboardService.DeleteTask and CoreService.SetTaskStatus's
// opportunistic-teardown block already use.
func (l *Loop) enforceStopGrace(ctx context.Context) {
	ids, err := l.tasks.ListOverdueStopIDs(ctx, l.stopGrace)
	if err != nil {
		slog.Error("dispatch: list overdue stops failed", "error", err)
		return
	}
	for _, id := range ids {
		if _, err := l.provisioner.TearDownSession(ctx, id, agentfleetv1.SessionKind_SESSION_KIND_WORKER); err != nil {
			slog.Error("dispatch: enforceStopGrace worker teardown failed", "taskId", id, "error", err)
		}
		if _, err := l.provisioner.TearDownSession(ctx, id, agentfleetv1.SessionKind_SESSION_KIND_E2E); err != nil {
			slog.Error("dispatch: enforceStopGrace e2e teardown failed", "taskId", id, "error", err)
		}
		// No status change — this is pod teardown, not task termination
		// (sessions redesign, supersedes docs/adr/0021/0025's phase-
		// boundary framing): the worktree and saved session_id both
		// survive, so the session is idle afterward, the same as it would
		// be after a Stop that the worker honored cooperatively in time.
		// Forcing a terminal "cancelled" status here would make an
		// otherwise-resumable session look permanently dead.
		slog.Warn("dispatch: force-stopped task past grace period", "taskId", id)
	}
}

// enforceIdleTimeout is the sessions redesign's idle-timeout backstop
// (supersedes docs/adr/0021/0025's phase-boundary framing) — belongs here,
// not a provisioner-side check, per docs/adr/0020 point 2 ("the
// provisioner never decides pod lifecycle on its own"); already-settled
// architecture, not a design choice being made here. A pod that's gone
// idleTimeout with no real transcript activity (tasks.last_active_at, see
// cmd/core's activityTrackingStore) gets torn down the same way
// enforceStopGrace tears one down — same TearDownSession call, no status
// change, since the session itself (worktree + saved session_id) is still
// there to warm again later.
func (l *Loop) enforceIdleTimeout(ctx context.Context) {
	ids, err := l.tasks.ListIdleWarmTaskIDs(ctx, l.idleTimeout)
	if err != nil {
		slog.Error("dispatch: list idle warm tasks failed", "error", err)
		return
	}
	for _, id := range ids {
		if _, err := l.provisioner.TearDownSession(ctx, id, agentfleetv1.SessionKind_SESSION_KIND_WORKER); err != nil {
			slog.Error("dispatch: enforceIdleTimeout worker teardown failed", "taskId", id, "error", err)
			continue
		}
		if _, err := l.provisioner.TearDownSession(ctx, id, agentfleetv1.SessionKind_SESSION_KIND_E2E); err != nil {
			slog.Error("dispatch: enforceIdleTimeout e2e teardown failed", "taskId", id, "error", err)
		}
		slog.Info("dispatch: idle-timed-out a warm session with no recent activity", "taskId", id)
	}
}
