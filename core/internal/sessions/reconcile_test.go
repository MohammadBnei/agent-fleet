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

func (f *fakeProvisioner) SweepSession(ctx context.Context, id string) error {
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
	alive, err := store.Create(ctx, CreateParams{Repo: "agent-fleet", Title: "", Description: "alive"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	orphan, err := store.Create(ctx, CreateParams{Repo: "agent-fleet", Title: "", Description: "orphan"})
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
	loop := NewLoop(store, fake, time.Minute, time.Minute, time.Hour, 14*24*time.Hour, time.Minute)

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
}

// The upward half of the same pass, which nothing else does.
//
// The provisioner reports SCHEDULED when it creates the Job and then only
// CRASHED or TERMINATED when it ends, so without this a healthy session sat at
// SCHEDULED for its whole life — the dashboard showed "SCHEDULED" for a
// session that had been talking to a human for an hour. Found in kind, where
// the pod was plainly Running and the row plainly was not.
func TestReconcilePodPhases_SyncsScheduledUpToRunning(t *testing.T) {
	ctx := context.Background()
	store := NewStore(dbtest.NewPool(t))

	id, err := store.Create(ctx, CreateParams{Repo: "agent-fleet", Title: "", Description: "phase sync"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SetPodPhase(ctx, id, "POD_PHASE_SCHEDULED", ""); err != nil {
		t.Fatalf("phase: %v", err)
	}

	fake := &fakeProvisioner{livePods: map[string]string{id: "Running"}}
	loop := NewLoop(store, fake, time.Minute, time.Minute, time.Hour, 14*24*time.Hour, time.Minute)
	loop.reconcilePodPhases(ctx)

	s, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if s.PodPhase == nil || *s.PodPhase != "POD_PHASE_RUNNING" {
		t.Fatalf("phase = %v, want RUNNING", s.PodPhase)
	}

	// A Pending pod stays SCHEDULED: rewriting it every 60s is churn, and the
	// two mean the same thing to everything that reads them.
	pendingID, err := store.Create(ctx, CreateParams{Repo: "agent-fleet", Title: "", Description: "still pending"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SetPodPhase(ctx, pendingID, "POD_PHASE_SCHEDULED", ""); err != nil {
		t.Fatalf("phase: %v", err)
	}
	fake.livePods = map[string]string{pendingID: "Pending"}
	loop.reconcilePodPhases(ctx)

	p, err := store.Get(ctx, pendingID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.PodPhase == nil || *p.PodPhase != "POD_PHASE_SCHEDULED" {
		t.Fatalf("a Pending pod should stay SCHEDULED, got %v", p.PodPhase)
	}
}

// The warm race. A session in PROVISIONING/CREATED has no Job yet BY
// DEFINITION — the provisioner reports those phases before the clone, the
// fleet-shared sync and the pod create, which take tens of seconds together —
// so "Kubernetes has no Job" is the expected answer, not an orphan.
//
// Observed live 2026-08-15: this loop ticked 3s after a warm and wrote
// TERMINATED over a session whose pod was seconds from existing. The write
// does not heal, either: TERMINATED is not a live phase, so every later pass
// skips the row and the session shows no pod while its agent runs.
func TestReconcilePodPhases_DoesNotOrphanASessionMidProvision(t *testing.T) {
	ctx := context.Background()
	store := NewStore(dbtest.NewPool(t))

	for _, phase := range []string{"POD_PHASE_PROVISIONING", "POD_PHASE_CREATED"} {
		t.Run(phase, func(t *testing.T) {
			id, err := store.Create(ctx, CreateParams{Repo: "agent-fleet", Title: "", Description: "warming"})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := store.SetPodPhase(ctx, id, phase, "creating pod"); err != nil {
				t.Fatalf("phase: %v", err)
			}

			// Kubernetes has nothing yet — the provisioner hasn't created the
			// Job. Exactly the state the real race hit.
			loop := NewLoop(store, &fakeProvisioner{livePods: map[string]string{}}, time.Minute, time.Minute, time.Hour, 14*24*time.Hour, time.Minute)
			loop.reconcilePodPhases(ctx)

			s, err := store.Get(ctx, id)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if s.PodPhase == nil || *s.PodPhase != phase {
				got := "<nil>"
				if s.PodPhase != nil {
					got = *s.PodPhase
				}
				t.Fatalf("a session mid-provision was reconciled to %s — its pod does not exist YET, which is not the same as gone", got)
			}
		})
	}
}

// If Kubernetes cannot be reached, the correct action is none. Treating an
// unreadable pod list as "no pods exist" would terminate every live session in
// the fleet on a transient provisioner outage — the loop would do far more
// damage than the drift it exists to repair.
func TestReconcilePodPhases_ListFailureChangesNothing(t *testing.T) {
	ctx := context.Background()
	store := NewStore(dbtest.NewPool(t))

	id, err := store.Create(ctx, CreateParams{Repo: "agent-fleet", Title: "", Description: "live"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SetPodPhase(ctx, id, "POD_PHASE_RUNNING", ""); err != nil {
		t.Fatalf("phase: %v", err)
	}

	fake := &fakeProvisioner{listErr: errors.New("provisioner unreachable")}
	loop := NewLoop(store, fake, time.Minute, time.Minute, time.Hour, 14*24*time.Hour, time.Minute)

	loop.reconcilePodPhases(ctx)

	s, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if s.PodPhase == nil || *s.PodPhase != "POD_PHASE_RUNNING" {
		t.Fatalf("a failed pod listing terminated a live session: phase=%v", s.PodPhase)
	}
	if len(fake.tornWorkers) != 0 {
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

	id, err := store.Create(ctx, CreateParams{Repo: "agent-fleet", Title: "", Description: "old"})
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
	loop := NewLoop(store, fake, time.Minute, time.Minute, time.Hour, 14*24*time.Hour, time.Minute)

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
