package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/db"
	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/k8s"
)

type fakeStore struct {
	needingTeardown []db.Session
	running         []db.Session
	tornDown        []string
}

func (f *fakeStore) SessionsNeedingTeardown(ctx context.Context) ([]db.Session, error) {
	return f.needingTeardown, nil
}
func (f *fakeStore) RunningSessions(ctx context.Context) ([]db.Session, error) { return f.running, nil }
func (f *fakeStore) SetSessionStatus(ctx context.Context, id, status string, podName, ingressPath *string) error {
	if status == "torn_down" {
		f.tornDown = append(f.tornDown, id)
	}
	return nil
}

type fakeK8s struct {
	livePods []k8s.LiveE2ePod
	deleted  []string
}

func (f *fakeK8s) ListPodsByLabel(ctx context.Context) ([]k8s.LiveE2ePod, error) {
	return f.livePods, nil
}
func (f *fakeK8s) DeleteAll(ctx context.Context, taskID string) error {
	f.deleted = append(f.deleted, taskID)
	return nil
}

type fakeProxy struct{ dropped []string }

func (f *fakeProxy) DropClient(taskID string) { f.dropped = append(f.dropped, taskID) }

func TestReconcileOrphans_CleansUpUntrackedPods(t *testing.T) {
	store := &fakeStore{running: []db.Session{{TaskID: "tracked-1"}}}
	kc := &fakeK8s{livePods: []k8s.LiveE2ePod{{TaskID: "tracked-1", PodName: "e2e-1"}, {TaskID: "orphan-1", PodName: "e2e-2"}}}
	l := New(store, kc, &fakeProxy{})

	if err := l.reconcileOrphans(context.Background()); err != nil {
		t.Fatalf("reconcileOrphans: %v", err)
	}
	if len(kc.deleted) != 1 || kc.deleted[0] != "orphan-1" {
		t.Errorf("expected only orphan-1 deleted, got %v", kc.deleted)
	}
}

func TestRun_TearsDownDueSessions(t *testing.T) {
	store := &fakeStore{needingTeardown: []db.Session{{ID: "sess-1", TaskID: "task-1"}}}
	kc := &fakeK8s{}
	proxy := &fakeProxy{}
	l := New(store, kc, proxy)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_ = l.Run(ctx, 10*time.Millisecond)

	if len(kc.deleted) == 0 || kc.deleted[0] != "task-1" {
		t.Errorf("expected task-1 resources deleted, got %v", kc.deleted)
	}
	if len(proxy.dropped) == 0 || proxy.dropped[0] != "task-1" {
		t.Errorf("expected task-1 mcp client dropped, got %v", proxy.dropped)
	}
	if len(store.tornDown) == 0 || store.tornDown[0] != "sess-1" {
		t.Errorf("expected sess-1 marked torn_down, got %v", store.tornDown)
	}
}
