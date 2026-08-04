package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
)

type fakeK8s struct {
	pods    []k8s.LiveWorkerPod
	deleted []string
}

func (f *fakeK8s) ListWorkerPodsByLabel(ctx context.Context) ([]k8s.LiveWorkerPod, error) {
	return f.pods, nil
}
func (f *fakeK8s) DeleteWorkerPod(ctx context.Context, taskID string) error {
	f.deleted = append(f.deleted, taskID)
	return nil
}

func TestGcTerminalWorkerPods_OnlyDeletesTerminalPhase(t *testing.T) {
	kc := &fakeK8s{pods: []k8s.LiveWorkerPod{
		{TaskID: "running-1", PodName: "worker-1", Phase: "Running"},
		{TaskID: "done-1", PodName: "worker-2", Phase: "Succeeded"},
		{TaskID: "failed-1", PodName: "worker-3", Phase: "Failed"},
		{TaskID: "pending-1", PodName: "worker-4", Phase: "Pending"},
	}}
	l := New(kc)

	l.gcTerminalWorkerPods(context.Background())

	if len(kc.deleted) != 2 {
		t.Fatalf("expected exactly 2 terminal pods deleted, got %v", kc.deleted)
	}
	deleted := map[string]bool{kc.deleted[0]: true, kc.deleted[1]: true}
	if !deleted["done-1"] || !deleted["failed-1"] {
		t.Errorf("expected done-1 and failed-1 deleted, got %v", kc.deleted)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	kc := &fakeK8s{}
	l := New(kc)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		l.Run(ctx, 10*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run did not return after context cancellation")
	}
}
