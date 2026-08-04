// Package reconcile is a pure Kubernetes-native safety net now
// (docs/adr/0020 point 1: the provisioner holds no Postgres credentials at
// all, so there's nothing external left to reconcile pod state against).
// Its only job: garbage-collect worker pods that reached a terminal k8s
// phase (Succeeded/Failed) but weren't cleaned up by core's own
// TearDownSession call — a safety net for a missed/failed call, not the
// primary teardown mechanism (that's coreserver.SetTaskStatus's
// opportunistic trigger, in core/). e2e pods have no natural "done" signal
// from k8s alone (they run until explicitly killed) and no external
// tracking left to compare against, so they're not GC'd here — an accepted
// gap, not a silent one: a leaked e2e pod would need a different mechanism
// entirely (e.g. a max-age timeout), deferred as future scope.
package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
)

// PodLister is the narrow slice of k8s.Client this package needs — a
// hand-written fake satisfies it in tests, no live cluster required.
type PodLister interface {
	ListWorkerPodsByLabel(ctx context.Context) ([]k8s.LiveWorkerPod, error)
	DeleteWorkerPod(ctx context.Context, taskID string) error
}

type Loop struct {
	k8sc PodLister
}

func New(k8sc PodLister) *Loop {
	return &Loop{k8sc: k8sc}
}

func (l *Loop) gcTerminalWorkerPods(ctx context.Context) {
	pods, err := l.k8sc.ListWorkerPodsByLabel(ctx)
	if err != nil {
		slog.Error("reconcile: list worker pods failed", "error", err)
		return
	}
	for _, pod := range pods {
		if pod.Phase != "Succeeded" && pod.Phase != "Failed" {
			continue
		}
		slog.Info("reconcile: gc'ing terminal worker pod", "taskId", pod.TaskID, "podName", pod.PodName, "phase", pod.Phase)
		if err := l.k8sc.DeleteWorkerPod(ctx, pod.TaskID); err != nil {
			slog.Error("reconcile: delete worker pod failed", "taskId", pod.TaskID, "error", err)
		}
		// Worktree cleanup is intentionally skipped here — this pass exists
		// for the case where core's TearDownSession call (which does clean
		// the worktree) never fired; the pod's RepoLabel would still tell us
		// the repo, but a genuinely orphaned worktree with no task record
		// left to explain it is a small enough leak (one directory, on a
		// path keyed by a task ID that's already terminal) not to add a
		// second GC path for on top of the pod cleanup that actually matters.
	}
}

func (l *Loop) Run(ctx context.Context, pollInterval time.Duration) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		l.gcTerminalWorkerPods(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
