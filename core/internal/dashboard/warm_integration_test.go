//go:build integration

package dashboard

import (
	"context"
	"net"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repoprofiles"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
)

// fakeProvisionerServer records CreateWorkerPod calls instead of doing any
// real git/k8s work — real Postgres for tasks.Store/repos.Store (see
// newTestPool in stop_integration_test.go), but a real gRPC pod-creation
// call would need a real cluster, which warmIfIdle's own logic doesn't
// need to exercise this deep.
type fakeProvisionerServer struct {
	agentfleetv1.UnimplementedProvisionerServiceServer
	calls []*agentfleetv1.CreateWorkerPodRequest
}

func (f *fakeProvisionerServer) CreateWorkerPod(ctx context.Context, req *agentfleetv1.CreateWorkerPodRequest) (*agentfleetv1.CreateWorkerPodResponse, error) {
	f.calls = append(f.calls, req)
	return &agentfleetv1.CreateWorkerPodResponse{PodName: "worker-" + req.GetTaskId()}, nil
}

// newFakeProvisioner starts a real gRPC server on a real localhost port —
// provisionerclient.New only dials by address, it has no bufconn/injectable-
// conn constructor (that's client_test.go's own in-package trick, not
// available from this package).
func newFakeProvisioner(t *testing.T) (*fakeProvisionerServer, *provisionerclient.Client) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	fake := &fakeProvisionerServer{}
	agentfleetv1.RegisterProvisionerServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	client, err := provisionerclient.New(lis.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return fake, client
}

// seedIdleClaimedTask is seedTask plus a status flip out of 'pending' —
// Warm's whole premise is re-warming a session dispatch.Loop's
// ClaimNextTask already claimed at least once before, not racing that
// same claim for a brand-new task (see Warm's own pending-status guard).
func seedIdleClaimedTask(t *testing.T, pool *pgxpool.Pool, taskStore *tasks.Store) string {
	t.Helper()
	id := seedTask(t, pool)
	if err := taskStore.SetStatus(context.Background(), id, "running", nil, nil, nil); err != nil {
		t.Fatalf("seed: set status: %v", err)
	}
	return id
}

// TestServer_Warm_BootsNewPod covers the sessions redesign's explicit warm
// action (supersedes docs/adr/0021/0025's phase-boundary framing): an idle
// task (no live pod) gets a fresh pod, a refreshed lease_id (or
// StillHoldsLease would always fail against whatever tasks.lease_id held
// from a prior claim), and does NOT touch tasks.status.
func TestServer_Warm_BootsNewPod(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)
	repoStore := repos.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)
	taskID := seedIdleClaimedTask(t, pool, taskStore)

	fake, provisioner := newFakeProvisioner(t)
	s := NewServer(taskStore, &recordingStore{}, nil, repoStore, profileStore, nil, provisioner, nil, nil, 5, nil, nil)

	resp, err := s.Warm(ctx, connect.NewRequest(&agentfleetv1.WarmRequest{TaskId: taskID}))
	if err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if resp.Msg.GetStatus() != "warming" || resp.Msg.GetPodName() == "" {
		t.Errorf("Warm response = %+v, want status=warming, non-empty pod_name", resp.Msg)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected exactly 1 CreateWorkerPod call, got %d", len(fake.calls))
	}
	if fake.calls[0].GetLeaseId() == "" {
		t.Error("expected a non-empty lease_id (RefreshLease) so StillHoldsLease doesn't always fail")
	}

	task, err := taskStore.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != "running" {
		t.Errorf("status = %q, want unchanged %q — Warm must never touch tasks.status", task.Status, "running")
	}
}

// TestServer_Warm_StillPending_FailedPrecondition covers the double-
// dispatch guard: a task dispatch.Loop hasn't claimed yet (status still
// 'pending') must not also be warmable directly — both this call and the
// next ClaimNextTask tick would otherwise call CreateWorkerPod for the
// same task independently.
func TestServer_Warm_StillPending_FailedPrecondition(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)
	repoStore := repos.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)
	taskID := seedTask(t, pool) // left at the default 'pending' status

	fake, provisioner := newFakeProvisioner(t)
	s := NewServer(taskStore, &recordingStore{}, nil, repoStore, profileStore, nil, provisioner, nil, nil, 5, nil, nil)

	_, err := s.Warm(ctx, connect.NewRequest(&agentfleetv1.WarmRequest{TaskId: taskID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Warm error = %v, want CodeFailedPrecondition", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected no CreateWorkerPod call against a still-pending task, got %d", len(fake.calls))
	}
}

// TestServer_Warm_ThreadsSavedSessionID covers resume: a task with a
// previously-saved session_id must have it threaded through to
// CreateWorkerPodRequest.resume_session_id so worker/src/session.ts
// actually resumes instead of starting fresh.
func TestServer_Warm_ThreadsSavedSessionID(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)
	repoStore := repos.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)
	taskID := seedIdleClaimedTask(t, pool, taskStore)
	if _, err := taskStore.SaveSessionID(ctx, taskID, "sess-resume-me", "claude-opus-4-8", ""); err != nil {
		t.Fatalf("SaveSessionID: %v", err)
	}

	fake, provisioner := newFakeProvisioner(t)
	s := NewServer(taskStore, &recordingStore{}, nil, repoStore, profileStore, nil, provisioner, nil, nil, 5, nil, nil)

	if _, err := s.Warm(ctx, connect.NewRequest(&agentfleetv1.WarmRequest{TaskId: taskID})); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0].GetResumeSessionId() != "sess-resume-me" {
		t.Fatalf("expected resume_session_id=sess-resume-me, got calls=%+v", fake.calls)
	}
}

// TestServer_Warm_AlreadyLive_FailedPrecondition covers the idempotent
// double-click guard — Warm on a task that already has a live pod must
// reject, not spin up a second pod for the same session.
func TestServer_Warm_AlreadyLive_FailedPrecondition(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)
	repoStore := repos.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)
	taskID := seedTask(t, pool)
	if err := taskStore.SetPodPhase(ctx, taskID, "POD_PHASE_RUNNING", ""); err != nil {
		t.Fatalf("SetPodPhase: %v", err)
	}

	fake, provisioner := newFakeProvisioner(t)
	s := NewServer(taskStore, &recordingStore{}, nil, repoStore, profileStore, nil, provisioner, nil, nil, 5, nil, nil)

	_, err := s.Warm(ctx, connect.NewRequest(&agentfleetv1.WarmRequest{TaskId: taskID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("Warm error = %v, want CodeFailedPrecondition", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected no CreateWorkerPod call against an already-live task, got %d", len(fake.calls))
	}
}

// TestServer_Warm_AtCapacity_ResourceExhausted covers the reinterpreted
// MAX_IN_FLIGHT_TASKS cap — "max warm pods" now, not "max in-flight
// tasks" (same knob, same code, different meaning per the sessions
// redesign) — enforced against tasks.Store.CountLivePods, not the
// ClaimNextTask-only concurrency check dispatch.Loop uses for fresh tasks.
func TestServer_Warm_AtCapacity_ResourceExhausted(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)
	repoStore := repos.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)

	const cap = 2
	for i := 0; i < cap; i++ {
		id := seedIdleClaimedTask(t, pool, taskStore)
		if err := taskStore.SetPodPhase(ctx, id, "POD_PHASE_RUNNING", ""); err != nil {
			t.Fatalf("SetPodPhase: %v", err)
		}
	}
	idleTaskID := seedIdleClaimedTask(t, pool, taskStore)

	fake, provisioner := newFakeProvisioner(t)
	s := NewServer(taskStore, &recordingStore{}, nil, repoStore, profileStore, nil, provisioner, nil, nil, cap, nil, nil)

	_, err := s.Warm(ctx, connect.NewRequest(&agentfleetv1.WarmRequest{TaskId: idleTaskID}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("Warm error = %v, want CodeResourceExhausted", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected no CreateWorkerPod call while at capacity, got %d", len(fake.calls))
	}
}

// TestServer_Discuss_AutoWarmsIdleSession covers "boots on first
// interaction" — a message to an idle session (no live pod) must warm one
// before the message is appended, so the pod that reads it back off
// streamHumanMessages already exists.
func TestServer_Discuss_AutoWarmsIdleSession(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)
	repoStore := repos.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)
	taskID := seedIdleClaimedTask(t, pool, taskStore)

	fake, provisioner := newFakeProvisioner(t)
	store := &recordingStore{}
	s := NewServer(taskStore, store, nil, repoStore, profileStore, nil, provisioner, nil, nil, 5, nil, nil)

	if _, err := s.Discuss(ctx, connect.NewRequest(&agentfleetv1.DiscussRequest{TaskId: taskID, Text: "hey"})); err != nil {
		t.Fatalf("Discuss: %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected Discuss to auto-warm exactly once, got %d CreateWorkerPod calls", len(fake.calls))
	}
	if store.lastText != "hey" || store.lastType != "discussion" {
		t.Errorf("Discuss still must append the message itself: got (%q, %q)", store.lastText, store.lastType)
	}
}

// TestServer_Discuss_StillPending_SkipsWarmButStillSends covers Discuss's
// silent-skip half of the same guard Warm rejects loudly for (see
// TestServer_Warm_StillPending_FailedPrecondition) — a message to a task
// dispatch.Loop hasn't claimed yet must not attempt a competing
// CreateWorkerPod call, but must still be appended (Discuss always has a
// message to deliver, unlike an explicit Warm click with nothing else to do).
func TestServer_Discuss_StillPending_SkipsWarmButStillSends(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)
	repoStore := repos.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)
	taskID := seedTask(t, pool) // left at the default 'pending' status

	fake, provisioner := newFakeProvisioner(t)
	store := &recordingStore{}
	s := NewServer(taskStore, store, nil, repoStore, profileStore, nil, provisioner, nil, nil, 5, nil, nil)

	if _, err := s.Discuss(ctx, connect.NewRequest(&agentfleetv1.DiscussRequest{TaskId: taskID, Text: "hey"})); err != nil {
		t.Fatalf("Discuss: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected no CreateWorkerPod call against a still-pending task, got %d", len(fake.calls))
	}
	if store.lastText != "hey" || store.lastType != "discussion" {
		t.Errorf("Discuss must still append the message despite skipping warm: got (%q, %q)", store.lastText, store.lastType)
	}
}

// TestServer_Discuss_LivePod_NoWarmAttempt is the pre-existing happy-path
// coverage TestServer_Discuss (server_test.go) provided before Discuss
// started touching tasks.Store — moved here for the same reason Stop's
// tests already were (see stop_integration_test.go's own comment):
// tasks.Store is concrete, not an interface a nil/fake can stand in for.
func TestServer_Discuss_LivePod_NoWarmAttempt(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)
	repoStore := repos.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)
	taskID := seedTask(t, pool)
	if err := taskStore.SetPodPhase(ctx, taskID, "POD_PHASE_RUNNING", ""); err != nil {
		t.Fatalf("SetPodPhase: %v", err)
	}

	fake, provisioner := newFakeProvisioner(t)
	store := &recordingStore{}
	s := NewServer(taskStore, store, nil, repoStore, profileStore, nil, provisioner, nil, nil, 5, nil, nil)

	resp, err := s.Discuss(ctx, connect.NewRequest(&agentfleetv1.DiscussRequest{TaskId: taskID, Text: "what's the status?"}))
	if err != nil {
		t.Fatalf("Discuss: %v", err)
	}
	if resp.Msg.GetStatus() != "sent" {
		t.Errorf("status = %q, want %q", resp.Msg.GetStatus(), "sent")
	}
	if store.lastTaskID != taskID || store.lastFrom != "human" || store.lastText != "what's the status?" || store.lastType != "discussion" {
		t.Errorf("Append(%q, %q, %q, %q), want (%s, human, \"what's the status?\", discussion)",
			store.lastTaskID, store.lastFrom, store.lastText, store.lastType, taskID)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected no warm attempt against an already-live task, got %d CreateWorkerPod calls", len(fake.calls))
	}
}

// seedProposal is seedTask plus the status a machine-created task lands
// in — what tasks.Store.CreateDeduped produces for an Alertmanager alert
// or a due scheduled audit.
func seedProposal(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := seedTask(t, pool)
	if _, err := pool.Exec(context.Background(), `UPDATE tasks SET status = 'proposed' WHERE id = $1`, id); err != nil {
		t.Fatalf("seed: set proposed: %v", err)
	}
	return id
}

// TestServer_Discuss_DoesNotWarmProposal is THE test for the approval
// gate, and the reason warmIfIdle carries the guard rather than Warm's
// handler.
//
// Discuss calls warmIfIdle unconditionally on every message, silently,
// with no status check of its own. A patch that guards only the Warm
// handler passes every other test in this suite and still lets anyone
// spawn a pod running an agent with cluster access for an alert no human
// approved — just by typing into the task instead of clicking Warm.
//
// The message must still be appended: Discuss always has something to
// send regardless of whether a pod exists to read it.
func TestServer_Discuss_DoesNotWarmProposal(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)
	repoStore := repos.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)
	taskID := seedProposal(t, pool)

	fake, provisioner := newFakeProvisioner(t)
	store := &recordingStore{}
	s := NewServer(taskStore, store, nil, repoStore, profileStore, nil, provisioner, nil, nil, 5, nil, nil)

	if _, err := s.Discuss(ctx, connect.NewRequest(&agentfleetv1.DiscussRequest{TaskId: taskID, Text: "hey"})); err != nil {
		t.Fatalf("Discuss: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("Discuss warmed a pod for an unapproved proposal (%d CreateWorkerPod calls) — "+
			"any human message would bypass the approval gate", len(fake.calls))
	}
	if store.lastText != "hey" {
		t.Errorf("Discuss must still append the message: got %q", store.lastText)
	}

	task, err := taskStore.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != "proposed" {
		t.Errorf("status = %q, want it left at %q", task.Status, "proposed")
	}
}

// The obvious half of the same gate. Distinct from Discuss above because
// Warm gives an explicit reason rather than silently skipping — a human
// who clicked deserves to know why nothing happened.
func TestServer_Warm_RejectsUnapprovedProposal(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)
	repoStore := repos.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)
	taskID := seedProposal(t, pool)

	fake, provisioner := newFakeProvisioner(t)
	s := NewServer(taskStore, &recordingStore{}, nil, repoStore, profileStore, nil, provisioner, nil, nil, 5, nil, nil)

	_, err := s.Warm(ctx, connect.NewRequest(&agentfleetv1.WarmRequest{TaskId: taskID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("Warm on a proposal: err = %v (code %v), want FailedPrecondition", err, connect.CodeOf(err))
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected no CreateWorkerPod call, got %d", len(fake.calls))
	}
}

// ApproveTask hands the task to dispatch — it must NOT warm a pod itself.
// A pod warmed behind dispatch's back is invisible to ClaimNextTask's
// in-flight count, which is how MAX_IN_FLIGHT_TASKS silently drifts.
func TestServer_ApproveTask_QueuesForDispatchWithoutWarming(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)
	repoStore := repos.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)
	taskID := seedProposal(t, pool)

	fake, provisioner := newFakeProvisioner(t)
	s := NewServer(taskStore, &recordingStore{}, nil, repoStore, profileStore, nil, provisioner, nil, nil, 5, nil, nil)

	resp, err := s.ApproveTask(ctx, connect.NewRequest(&agentfleetv1.ApproveTaskRequest{TaskId: taskID}))
	if err != nil {
		t.Fatalf("ApproveTask: %v", err)
	}
	if resp.Msg.GetStatus() != "approved" {
		t.Errorf("status = %q, want %q", resp.Msg.GetStatus(), "approved")
	}
	if len(fake.calls) != 0 {
		t.Errorf("ApproveTask must not create a pod (dispatch owns the first one), got %d calls", len(fake.calls))
	}

	task, err := taskStore.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != "pending" {
		t.Errorf("status = %q, want %q so ClaimNextTask can pick it up", task.Status, "pending")
	}

	// Second click: the guarded UPDATE matched nothing, so this must be a
	// rejection rather than a silent re-queue of a task dispatch may
	// already have claimed.
	if _, err := s.ApproveTask(ctx, connect.NewRequest(&agentfleetv1.ApproveTaskRequest{TaskId: taskID})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("second ApproveTask: err = %v (code %v), want FailedPrecondition", err, connect.CodeOf(err))
	}
}
