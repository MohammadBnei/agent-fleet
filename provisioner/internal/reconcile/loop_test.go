package reconcile

import (
	"context"
	"testing"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
)

type fakeK8s struct {
	jobs             []k8s.LiveWorkerJob
	deleted          []string
	instances        []k8s.LiveSharedInstance
	deletedInstances []string // "repo/serviceKey"
}

func (f *fakeK8s) ListWorkerJobsByLabel(ctx context.Context) ([]k8s.LiveWorkerJob, error) {
	return f.jobs, nil
}
func (f *fakeK8s) DeleteWorkerJob(ctx context.Context, taskID string) error {
	f.deleted = append(f.deleted, taskID)
	return nil
}
func (f *fakeK8s) ListSharedInstances(ctx context.Context) ([]k8s.LiveSharedInstance, error) {
	return f.instances, nil
}
func (f *fakeK8s) DeleteSharedInstance(ctx context.Context, repo, serviceKey string) error {
	f.deletedInstances = append(f.deletedInstances, repo+"/"+serviceKey)
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
	l := New(kc, reporter, time.Hour)

	l.gcTerminalWorkerJobs(context.Background())

	if len(kc.deleted) != 2 {
		t.Fatalf("expected exactly 2 terminal jobs deleted, got %v", kc.deleted)
	}
	deleted := map[string]bool{kc.deleted[0]: true, kc.deleted[1]: true}
	if !deleted["done-1"] || !deleted["failed-1"] {
		t.Errorf("expected done-1 and failed-1 deleted, got %v", kc.deleted)
	}
}

// TestGcTerminalWorkerJobs_ReportsBothTerminalPhases: BOTH terminal phases
// report, and they report distinct phases.
//
// This test previously asserted the opposite for Succeeded — that a clean
// exit reports nothing, because reporting would falsely trigger core's
// MarkCrashed reclaim. That reasoning was sound only while a Succeeded
// worker wrote its own terminal tasks.status first, which was the sole
// trigger for TearDownSession and the sole way a finished session stopped
// counting against the concurrency cap. docs/adr/0048 deletes status, so
// this loop is now the only notification a clean exit produces: staying
// silent would leave pod_phase at RUNNING forever, and CountLivePods counts
// exactly that — the fleet would wedge after five successful sessions.
//
// The MarkCrashed concern does not transfer, because core scopes it to
// POD_PHASE_CRASHED specifically (coreserver.ReportPodEvents); a TERMINATED
// event only writes pod_phase.
func TestGcTerminalWorkerJobs_ReportsBothTerminalPhases(t *testing.T) {
	kc := &fakeK8s{jobs: []k8s.LiveWorkerJob{
		{TaskID: "done-1", JobName: "worker-2", Phase: "Succeeded"},
		{TaskID: "failed-1", JobName: "worker-3", Phase: "Failed"},
		{TaskID: "live-1", JobName: "worker-4", Phase: "Running"},
	}}
	reporter := &fakeEventReporter{}
	l := New(kc, reporter, time.Hour)

	l.gcTerminalWorkerJobs(context.Background())

	if len(reporter.events) != 2 {
		t.Fatalf("expected exactly 2 reported events, got %d: %+v", len(reporter.events), reporter.events)
	}

	got := map[string]agentfleetv1.PodPhase{}
	for _, e := range reporter.events {
		if e.GetKind() != agentfleetv1.SessionKind_SESSION_KIND_WORKER {
			t.Errorf("%s: expected SESSION_KIND_WORKER, got %v", e.GetSessionId(), e.GetKind())
		}
		got[e.GetSessionId()] = e.GetPhase()
	}

	if p := got["failed-1"]; p != agentfleetv1.PodPhase_POD_PHASE_CRASHED {
		t.Errorf("failed-1: expected POD_PHASE_CRASHED, got %v", p)
	}
	if p := got["done-1"]; p != agentfleetv1.PodPhase_POD_PHASE_TERMINATED {
		t.Errorf("done-1: expected POD_PHASE_TERMINATED, got %v", p)
	}
	if _, reported := got["live-1"]; reported {
		t.Error("live-1: a Running job must not be reported or GC'd")
	}
}

func TestGcIdleSharedInstances_OnlyDeletesPastTimeout(t *testing.T) {
	now := time.Now()
	kc := &fakeK8s{instances: []k8s.LiveSharedInstance{
		{Repo: "dream-analyst", ServiceKey: "postgres", LastUsedAt: now.Add(-2 * time.Hour)}, // stale, GC'd
		{Repo: "dream-analyst", ServiceKey: "redis", LastUsedAt: now.Add(-1 * time.Minute)},  // recent, kept
		{Repo: "agent-fleet", ServiceKey: "postgres", LastUsedAt: time.Time{}},               // no annotation yet, kept
	}}
	l := New(kc, &fakeEventReporter{}, time.Hour)

	l.gcIdleSharedInstances(context.Background())

	if len(kc.deletedInstances) != 1 || kc.deletedInstances[0] != "dream-analyst/postgres" {
		t.Fatalf("expected exactly dream-analyst/postgres deleted, got %v", kc.deletedInstances)
	}
}

// TestGcIdleSharedInstances_Uniform confirms task-scoped and repo-scoped
// instances are NOT distinguished by this pass — docs/adr/0034's decision
// to keep GC uniform rather than exempting repo-scoped as "protected."
// LiveSharedInstance itself carries no scope-mode field at all: from the
// GC pass's point of view a shared instance is just (repo, serviceKey,
// lastUsedAt), regardless of which scope modes are minting inside it.
func TestGcIdleSharedInstances_Uniform(t *testing.T) {
	now := time.Now()
	kc := &fakeK8s{instances: []k8s.LiveSharedInstance{
		{Repo: "dream-analyst", ServiceKey: "postgres", LastUsedAt: now.Add(-2 * time.Hour)},
		{Repo: "agent-fleet", ServiceKey: "postgres", LastUsedAt: now.Add(-2 * time.Hour)},
	}}
	l := New(kc, &fakeEventReporter{}, time.Hour)

	l.gcIdleSharedInstances(context.Background())

	if len(kc.deletedInstances) != 2 {
		t.Fatalf("expected both stale instances deleted regardless of what they're used for, got %v", kc.deletedInstances)
	}
}

// TestGcDeadE2ePods_DeletesTerminalAndAged covers docs/adr/0044's reversal of
// ADR-0039's "e2e pods are never GC'd" gap. A healthy, in-use sandbox must
// survive the pass — that's the case that makes this sweep safe to run every
// 10s against pods an agent is actively working in.
func TestRun_StopsOnContextCancel(t *testing.T) {
	kc := &fakeK8s{}
	l := New(kc, &fakeEventReporter{}, time.Hour)

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
