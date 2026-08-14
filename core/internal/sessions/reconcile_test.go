//go:build integration

package sessions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

// fakeProvisioner stands in for the gRPC client. ListWorkerPods is what
// Kubernetes would answer; the teardown calls just record.
type fakeProvisioner struct {
	livePods    map[string]string
	listErr     error
	sweepErr    error
	tornWorkers []string
	tornE2e     []string
	swept       []string
}

func (f *fakeProvisioner) ListWorkerPods(ctx context.Context) (map[string]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.livePods, nil
}

func (f *fakeProvisioner) TearDownWorker(ctx context.Context, id string) error {
	f.tornWorkers = append(f.tornWorkers, id)
	return nil
}

func (f *fakeProvisioner) TearDownE2e(ctx context.Context, id string) error {
	f.tornE2e = append(f.tornE2e, id)
	return nil
}

func (f *fakeProvisioner) SweepSession(ctx context.Context, id, repo string) error {
	if f.sweepErr != nil {
		return f.sweepErr
	}
	f.swept = append(f.swept, id)
	return nil
}

// The self-healing property, and the reason this loop exists at all.
//
// Pod events are pushed, and a push can be dropped — most plausibly while core
// is restarting, which is precisely when a deploy happens. A dropped TERMINATED
// leaves a finished session at RUNNING forever, and that is what the cap
// counts. The heartbeat used to be the backstop; asking Kubernetes is strictly
// better, because it is the thing that actually knows.
func TestReconcilePodPhases_OrphanedRunningRowIsTerminated(t *testing.T) {
	ctx := context.Background()
	store := NewStore(dbtest.NewPool(t))

	// One session whose pod really is up, one whose terminal event was lost.
	alive, err := store.Create(ctx, "agent-fleet", "", "alive", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	orphan, err := store.Create(ctx, "agent-fleet", "", "orphan", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, id := range []string{alive, orphan} {
		if err := store.SetPodPhase(ctx, id, "POD_PHASE_RUNNING", ""); err != nil {
			t.Fatalf("phase: %v", err)
		}
	}

	before, err := store.CountLivePods(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if before != 2 {
		t.Fatalf("expected 2 live pods before reconcile, got %d", before)
	}

	fake := &fakeProvisioner{livePods: map[string]string{alive: "Running"}}
	loop := NewLoop(store, fake, time.Minute, time.Minute, time.Hour, 14*24*time.Hour)

	loop.reconcilePodPhases(ctx)

	after, err := store.CountLivePods(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if after != 1 {
		t.Fatalf("reconcile did not free the orphaned slot: live=%d, want 1", after)
	}

	o, err := store.Get(ctx, orphan)
	if err != nil {
		t.Fatalf("get orphan: %v", err)
	}
	if o.PodPhase == nil || *o.PodPhase != "POD_PHASE_TERMINATED" {
		t.Fatalf("orphan phase = %v, want TERMINATED", o.PodPhase)
	}
	if o.PodMessage == nil || *o.PodMessage == "" {
		t.Error("a reconciled session must say it was reconciled — otherwise a pod that " +
			"vanished looks identical to one that finished normally")
	}

	a, err := store.Get(ctx, alive)
	if err != nil {
		t.Fatalf("get alive: %v", err)
	}
	if a.PodPhase == nil || *a.PodPhase != "POD_PHASE_RUNNING" {
		t.Fatalf("reconcile clobbered a session whose pod is genuinely up: %v", a.PodPhase)
	}
	// The sandbox outlives its worker only by accident; leaking one is what
	// makes the NEXT sandbox sit Pending against a full node.
	if len(fake.tornE2e) != 1 || fake.tornE2e[0] != orphan {
		t.Errorf("expected exactly the orphan's e2e pod to be torn down, got %v", fake.tornE2e)
	}
}

// If Kubernetes cannot be reached, the correct action is none. Treating an
// unreadable pod list as "no pods exist" would terminate every live session in
// the fleet on a transient provisioner outage — the loop would do far more
// damage than the drift it exists to repair.
func TestReconcilePodPhases_ListFailureChangesNothing(t *testing.T) {
	ctx := context.Background()
	store := NewStore(dbtest.NewPool(t))

	id, err := store.Create(ctx, "agent-fleet", "", "live", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SetPodPhase(ctx, id, "POD_PHASE_RUNNING", ""); err != nil {
		t.Fatalf("phase: %v", err)
	}

	fake := &fakeProvisioner{listErr: errors.New("provisioner unreachable")}
	loop := NewLoop(store, fake, time.Minute, time.Minute, time.Hour, 14*24*time.Hour)

	loop.reconcilePodPhases(ctx)

	s, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if s.PodPhase == nil || *s.PodPhase != "POD_PHASE_RUNNING" {
		t.Fatalf("a failed pod listing terminated a live session: phase=%v", s.PodPhase)
	}
	if len(fake.tornE2e)+len(fake.tornWorkers) != 0 {
		t.Error("a failed pod listing caused teardowns")
	}
}

// A failed sweep must NOT set swept_at. swept_at is what the dashboard reads
// to hide the Warm button, so recording a sweep that did not happen tells a
// human their still-resumable session is gone — and the retry that would have
// fixed it never comes, because the next pass no longer selects the row.
func TestCollectExpired_FailedSweepDoesNotMarkSwept(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)

	id, err := store.Create(ctx, "agent-fleet", "", "old", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SetPodPhase(ctx, id, "POD_PHASE_TERMINATED", ""); err != nil {
		t.Fatalf("phase: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE sessions SET created_at = now() - interval '30 days', last_active_at = NULL WHERE id = $1`, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	fake := &fakeProvisioner{sweepErr: errors.New("disk busy")}
	loop := NewLoop(store, fake, time.Minute, time.Minute, time.Hour, 14*24*time.Hour)

	loop.collectExpired(ctx)

	s, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if s.SweptAt != nil {
		t.Fatal("a failed sweep marked the session swept — the disk is still there, " +
			"the Warm button is now hidden, and no later pass will retry it")
	}

	// With the provisioner healthy again, the next pass must finish the job.
	fake.sweepErr = nil
	loop.collectExpired(ctx)

	s, err = store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if s.SweptAt == nil {
		t.Fatal("a recovered sweep never completed — the row stays GC-eligible forever")
	}
	if len(fake.swept) != 1 || fake.swept[0] != id {
		t.Errorf("expected one sweep of %s, got %v", id, fake.swept)
	}
}
