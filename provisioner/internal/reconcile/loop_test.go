package reconcile

import (
	"context"
	"testing"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
)

type fakeK8s struct {
	jobs    []k8s.LiveWorkerJob
	deleted []string
}

func (f *fakeK8s) ListWorkerJobsByLabel(ctx context.Context) ([]k8s.LiveWorkerJob, error) {
	return f.jobs, nil
}
func (f *fakeK8s) DeleteWorkerJob(ctx context.Context, taskID string) error {
	f.deleted = append(f.deleted, taskID)
	return nil
}

type fakeEventReporter struct {
	events []*agentfleetv1.PodEvent
}

func (f *fakeEventReporter) ReportEvent(ctx context.Context, event *agentfleetv1.PodEvent) {
	f.events = append(f.events, event)
}

func TestGcTerminalWorkerJobs_OnlyDeletesTerminalPhase(t *testing.T) {
	kc := &fakeK8s{jobs: []k8s.LiveWorkerJob{
		{TaskID: "running-1", JobName: "worker-1", Phase: "Running"},
		{TaskID: "done-1", JobName: "worker-2", Phase: "Succeeded"},
		{TaskID: "failed-1", JobName: "worker-3", Phase: "Failed"},
		{TaskID: "pending-1", JobName: "worker-4", Phase: "Pending"},
	}}
	reporter := &fakeEventReporter{}
	l := New(kc, reporter)

	l.gcTerminalWorkerJobs(context.Background())

	if len(kc.deleted) != 2 {
		t.Fatalf("expected exactly 2 terminal jobs deleted, got %v", kc.deleted)
	}
	deleted := map[string]bool{kc.deleted[0]: true, kc.deleted[1]: true}
	if !deleted["done-1"] || !deleted["failed-1"] {
		t.Errorf("expected done-1 and failed-1 deleted, got %v", kc.deleted)
	}
}

// TestGcTerminalWorkerJobs_ReportsCrashOnlyForFailed covers
// reliability-findings.md #1's fast-path accelerant: a Failed-phase Job
// reports a CRASHED event before GC, a Succeeded one does not (it's not a
// crash — reporting one would falsely trigger core's MarkCrashed reclaim
// path for a task that actually finished normally).
func TestGcTerminalWorkerJobs_ReportsCrashOnlyForFailed(t *testing.T) {
	kc := &fakeK8s{jobs: []k8s.LiveWorkerJob{
		{TaskID: "done-1", JobName: "worker-2", Phase: "Succeeded"},
		{TaskID: "failed-1", JobName: "worker-3", Phase: "Failed"},
	}}
	reporter := &fakeEventReporter{}
	l := New(kc, reporter)

	l.gcTerminalWorkerJobs(context.Background())

	if len(reporter.events) != 1 {
		t.Fatalf("expected exactly 1 reported event, got %d: %+v", len(reporter.events), reporter.events)
	}
	e := reporter.events[0]
	if e.GetTaskId() != "failed-1" {
		t.Errorf("expected the crash event for failed-1, got taskId=%q", e.GetTaskId())
	}
	if e.GetKind() != agentfleetv1.SessionKind_SESSION_KIND_WORKER {
		t.Errorf("expected SESSION_KIND_WORKER, got %v", e.GetKind())
	}
	if e.GetPhase() != agentfleetv1.PodPhase_POD_PHASE_CRASHED {
		t.Errorf("expected POD_PHASE_CRASHED, got %v", e.GetPhase())
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	kc := &fakeK8s{}
	l := New(kc, &fakeEventReporter{})

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
