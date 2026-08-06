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

	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
)

// Loop owns the claim-then-command cycle.
type Loop struct {
	tasks          *tasks.Store
	provisioner    *provisionerclient.Client
	maxInFlight    int
	maxTaskRetries int
	nudge          chan struct{}
}

func New(taskStore *tasks.Store, provisioner *provisionerclient.Client, maxInFlight, maxTaskRetries int) *Loop {
	return &Loop{
		tasks: taskStore, provisioner: provisioner,
		maxInFlight: maxInFlight, maxTaskRetries: maxTaskRetries,
		nudge: make(chan struct{}, 1),
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

	repoCfg, ok := tasks.KnownRepos[task.Repo]
	if !ok {
		slog.Error("dispatch: claimed task for unknown repo, cannot dispatch", "taskId", task.ID, "repo", task.Repo)
		return
	}

	podName, err := l.provisioner.CreateWorkerPod(ctx, task.ID, task.Repo, repoCfg.URL, repoCfg.BaseBranch, task.Description, task.LeaseID)
	if err != nil {
		slog.Error("dispatch: CreateWorkerPod failed", "taskId", task.ID, "error", err)
		return
	}
	slog.Info("dispatch: worker pod created", "taskId", task.ID, "repo", task.Repo, "podName", podName)
}
