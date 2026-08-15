//go:build integration

package coreserver

import (
	"context"
	"testing"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
)

// Real Postgres via core/internal/dbtest, which applies db/migrations/ —
// docs/adr/0030's single-source rule means these run against the same schema
// the PreSync hook applies in production, never a hand-copied fixture.

// The wedge property, pinned at the layer where it actually lives.
//
// Deleting tasks.status removed the only TearDownSession trigger, and
// gcTerminalWorkerJobs reported nothing at all for a Succeeded Job. A
// finished session would therefore sit at pod_phase RUNNING forever —
// and since CountLivePods counts exactly those phases, the fleet stops
// accepting work after MAX_LIVE_SESSIONS of them, silently, with no error
// anywhere until someone tries to open the next one (docs/adr/0048).
//
// So: a TERMINATED event must actually free the slot.
func TestReportPodEvents_TerminatedFreesTheSlot(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	srv := New(nil, store, journal.NewStore(pool), nil, nil, nil, nil)

	id, err := store.Create(ctx, "agent-fleet", "", "wedge guard", "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Pretend a pod came up.
	if err := store.SetPodPhase(ctx, id, "POD_PHASE_RUNNING", ""); err != nil {
		t.Fatalf("set running: %v", err)
	}
	live, err := store.CountLivePods(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if live != 1 {
		t.Fatalf("a running pod must occupy a slot, got live=%d", live)
	}

	// The Job succeeded, and the provisioner reported it. Before the fix this
	// event did not exist at all for a clean exit.
	srv.applyPodEvent(ctx, &agentfleetv1.PodEvent{
		SessionId: id,
		Kind:      agentfleetv1.SessionKind_SESSION_KIND_WORKER,
		Phase:     agentfleetv1.PodPhase_POD_PHASE_TERMINATED,
		PodName:   "worker-x",
		Message:   "worker job reached a terminal Succeeded phase",
	})

	live, err = store.CountLivePods(ctx)
	if err != nil {
		t.Fatalf("count after terminate: %v", err)
	}
	if live != 0 {
		t.Fatalf("a TERMINATED session still holds a slot (live=%d) — this is the wedge: "+
			"five of these and the fleet refuses all new work", live)
	}
}

// A crashed pod frees its slot too, and keeps enough state for a human to see
// why — the CRASHED phase plus its message are what the dashboard renders now
// that there is no `failed` status to read.
func TestReportPodEvents_CrashedFreesSlotAndKeepsReason(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	srv := New(nil, store, journal.NewStore(pool), nil, nil, nil, nil)

	id, err := store.Create(ctx, "agent-fleet", "", "crash guard", "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.SetPodPhase(ctx, id, "POD_PHASE_RUNNING", ""); err != nil {
		t.Fatalf("set running: %v", err)
	}

	srv.applyPodEvent(ctx, &agentfleetv1.PodEvent{
		SessionId: id,
		Kind:      agentfleetv1.SessionKind_SESSION_KIND_WORKER,
		Phase:     agentfleetv1.PodPhase_POD_PHASE_CRASHED,
		PodName:   "worker-y",
		Message:   "worker job reached a terminal Failed phase",
	})

	live, err := store.CountLivePods(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if live != 0 {
		t.Fatalf("a CRASHED session still holds a slot (live=%d)", live)
	}

	s, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if s.PodPhase == nil || *s.PodPhase != "POD_PHASE_CRASHED" {
		t.Fatalf("expected pod_phase CRASHED, got %v", s.PodPhase)
	}
	if s.PodMessage == nil || *s.PodMessage == "" {
		t.Fatal("a crash must keep its reason — it is all a human has to go on now")
	}
}

// Two ids flow through SaveAgentSessionId and they are NOT interchangeable:
// session_id keys the row, agent_session_id is the SDK's own conversation
// handle that a later `resume:` replays. The task_id -> session_id rename left
// this handler reading session_id for both, which type-checks perfectly
// (they're both strings), passes every unit test with a mocked store, and
// fails only as "the warmed agent doesn't remember anything" — the SDK,
// handed an id that was never one of its own, just starts a fresh
// conversation. Found against a live kind cluster, not by any check.
func TestSaveAgentSessionId_StoresTheAgentIdNotTheSessionId(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	srv := New(nil, store, journal.NewStore(pool), nil, nil, nil, nil)

	id, err := store.Create(ctx, "agent-fleet", "", "resume identity", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	lease, err := store.ReserveSlot(ctx, id, 5)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	const agentID = "sdk-conversation-9f3a"
	if _, err := srv.SaveAgentSessionId(ctx, &agentfleetv1.SaveAgentSessionIdRequest{
		SessionId:      id,
		AgentSessionId: agentID,
		Model:          "claude-opus-4-8",
		LeaseId:        lease,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	s, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if s.AgentSessionID == nil {
		t.Fatal("no agent_session_id stored at all")
	}
	if *s.AgentSessionID == id {
		t.Fatal("agent_session_id was set to the fleet's own session id — every resume will " +
			"silently start a brand-new conversation instead of continuing this one")
	}
	if *s.AgentSessionID != agentID {
		t.Fatalf("agent_session_id = %q, want %q", *s.AgentSessionID, agentID)
	}
}

// ReserveSlot is the whole of what replaced ClaimNextTask's cap, and the
// advisory lock inside it is not decoration: CI once observed 4 tasks claimed
// with a cap of 2, because under READ COMMITTED every concurrent caller's
// count(*) sees the snapshot from before any of the others committed.
func TestReserveSlot_ConcurrentCallersCannotExceedTheCap(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)

	const cap = 2
	const attempts = 8

	ids := make([]string, attempts)
	for i := range ids {
		id, err := store.Create(ctx, "agent-fleet", "", "cap race", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ids[i] = id
	}

	// Fire them all at once — the case a read-then-act check gets wrong.
	start := make(chan struct{})
	granted := make(chan string, attempts)
	for _, id := range ids {
		go func(id string) {
			<-start
			if _, err := store.ReserveSlot(ctx, id, cap); err == nil {
				granted <- id
			}
		}(id)
	}
	close(start)

	var won []string
	for i := 0; i < attempts; i++ {
		select {
		case id := <-granted:
			won = append(won, id)
			// A granted slot is only real once the pod phase says so; that is
			// what the next caller counts.
			if err := store.SetPodPhase(ctx, id, "POD_PHASE_RUNNING", ""); err != nil {
				t.Errorf("set phase: %v", err)
			}
		default:
		}
	}

	if len(won) > cap {
		t.Fatalf("%d callers got a slot with a cap of %d — the advisory lock is not holding, "+
			"which is exactly the 4-with-cap-2 failure CI caught before", len(won), cap)
	}
}
