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
	tasks       *tasks.Store
	provisioner *provisionerclient.Client
	maxInFlight int
}

func New(taskStore *tasks.Store, provisioner *provisionerclient.Client, maxInFlight int) *Loop {
	return &Loop{tasks: taskStore, provisioner: provisioner, maxInFlight: maxInFlight}
}

// Run polls every pollInterval until ctx is cancelled. Errors are logged
// and the loop continues — a transient DB or provisioner hiccup shouldn't
// stop dispatch for every other task waiting behind it.
func (l *Loop) Run(ctx context.Context, pollInterval time.Duration) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.tick(ctx)
		}
	}
}

func (l *Loop) tick(ctx context.Context) {
	inFlight, err := l.tasks.CountInFlight(ctx)
	if err != nil {
		slog.Error("dispatch: count in flight failed", "error", err)
		return
	}
	if inFlight >= l.maxInFlight {
		return
	}

	task, err := l.tasks.ClaimNextTask(ctx)
	if err != nil {
		slog.Error("dispatch: claim failed", "error", err)
		return
	}
	if task == nil {
		return // nothing eligible right now
	}

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
