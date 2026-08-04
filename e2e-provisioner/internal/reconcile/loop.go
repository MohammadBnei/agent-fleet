// Package reconcile is the Go port of e2e-provisioner/src/reconcile.ts:
// same plain poll loop shape (no k8s informer/controller framework) —
// tears down anything sessionsNeedingTeardown reports, and on startup
// cross-checks real cluster state against the DB so a crash between "pod
// created" and "next reconcile tick" never leaks a pod nothing watches.
package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/db"
	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/k8s"
)

// SessionStore/PodManager/ClientDropper are the narrow slices of
// db.Store/k8s.Client/mcpproxy.Proxy this package actually needs — hand-
// written fakes satisfy these in tests, no mockery/gomock required.
type SessionStore interface {
	SessionsNeedingTeardown(ctx context.Context) ([]db.Session, error)
	RunningSessions(ctx context.Context) ([]db.Session, error)
	SetSessionStatus(ctx context.Context, id, status string, podName, ingressPath *string) error
}

type PodManager interface {
	ListPodsByLabel(ctx context.Context) ([]k8s.LiveE2ePod, error)
	DeleteAll(ctx context.Context, taskID string) error
}

type ClientDropper interface {
	DropClient(taskID string)
}

type Loop struct {
	store SessionStore
	k8sc  PodManager
	proxy ClientDropper
}

func New(store SessionStore, k8sc PodManager, proxy ClientDropper) *Loop {
	return &Loop{store: store, k8sc: k8sc, proxy: proxy}
}

func (l *Loop) tearDown(ctx context.Context, sess db.Session) error {
	if err := l.k8sc.DeleteAll(ctx, sess.TaskID); err != nil {
		return err
	}
	l.proxy.DropClient(sess.TaskID)
	if err := l.store.SetSessionStatus(ctx, sess.ID, "torn_down", nil, nil); err != nil {
		return err
	}
	slog.Info("e2e session torn down", "taskId", sess.TaskID, "sessionId", sess.ID)
	return nil
}

// reconcileOrphans runs once at startup.
func (l *Loop) reconcileOrphans(ctx context.Context) error {
	livePods, err := l.k8sc.ListPodsByLabel(ctx)
	if err != nil {
		return err
	}
	tracked, err := l.store.RunningSessions(ctx)
	if err != nil {
		return err
	}
	trackedTaskIDs := make(map[string]bool, len(tracked))
	for _, s := range tracked {
		trackedTaskIDs[s.TaskID] = true
	}
	for _, pod := range livePods {
		if trackedTaskIDs[pod.TaskID] {
			continue
		}
		slog.Info("cleaning up orphaned e2e pod on startup", "taskId", pod.TaskID, "podName", pod.PodName)
		if err := l.k8sc.DeleteAll(ctx, pod.TaskID); err != nil {
			slog.Error("failed cleaning up orphaned pod", "taskId", pod.TaskID, "error", err)
		}
	}
	return nil
}

func (l *Loop) Run(ctx context.Context, pollInterval time.Duration) error {
	if err := l.reconcileOrphans(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		due, err := l.store.SessionsNeedingTeardown(ctx)
		if err != nil {
			slog.Error("sessions needing teardown query failed", "error", err)
		}
		for _, sess := range due {
			if err := l.tearDown(ctx, sess); err != nil {
				slog.Error("failed tearing down e2e session", "taskId", sess.TaskID, "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
