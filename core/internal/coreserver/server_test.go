//go:build integration

package coreserver

import (
	"context"
	"strings"
	"testing"
	"time"

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

	id, err := store.Create(ctx, sessions.CreateParams{Repo: "agent-fleet", Title: "", Description: "wedge guard"})
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

	id, err := store.Create(ctx, sessions.CreateParams{Repo: "agent-fleet", Title: "", Description: "crash guard"})
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

	id, err := store.Create(ctx, sessions.CreateParams{Repo: "agent-fleet", Title: "", Description: "resume identity"})
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
		id, err := store.Create(ctx, sessions.CreateParams{Repo: "agent-fleet", Title: "", Description: "cap race"})
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

// A pod phase transition must NOT write a journal row.
//
// Every one used to: ~80% of knowledge_journal was pod.POD_PHASE_* telemetry
// with no reader anywhere — no dashboard view calls GetJournal, no session
// ever searched for one — while the live value it duplicated was written by
// the very same function two statements later, into sessions.pod_phase. The
// cost was paid by the one caller that did want to read the journal: a
// seven-day, all-repo query came back four parts noise to one part signal
// (issue #198; the ADR proposing this is in PR #199 and unnumbered until a
// human lands it — do not cite a number that is not written yet).
//
// Re-adding the append would look harmless in review, so pin it here.
func TestApplyPodEvent_WritesNoJournalRow(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	journalStore := journal.NewStore(pool)
	srv := New(nil, store, journalStore, nil, nil, nil, nil)

	id, err := store.Create(ctx, sessions.CreateParams{Repo: "agent-fleet", Title: "", Description: "journal noise guard"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	for _, phase := range []agentfleetv1.PodPhase{
		agentfleetv1.PodPhase_POD_PHASE_CREATED,
		agentfleetv1.PodPhase_POD_PHASE_RUNNING,
		agentfleetv1.PodPhase_POD_PHASE_TERMINATED,
	} {
		if err := srv.applyPodEvent(ctx, &agentfleetv1.PodEvent{
			SessionId: id,
			Kind:      agentfleetv1.SessionKind_SESSION_KIND_WORKER,
			Phase:     phase,
			PodName:   "worker-z",
		}); err != nil {
			t.Fatalf("applyPodEvent(%v): %v", phase, err)
		}
	}

	entries, err := journalStore.List(ctx, "", 0, 100)
	if err != nil {
		t.Fatalf("list journal: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("pod events must not reach knowledge_journal, found %d rows (first: %s) — "+
			"the journal is what a future session needs to know about a repo, not pod telemetry",
			len(entries), entries[0].EventType)
	}

	// The state that replaced it is still written.
	s, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if s.PodPhase == nil || *s.PodPhase != "POD_PHASE_TERMINATED" {
		t.Fatalf("pod_phase must still be recorded, got %v", s.PodPhase)
	}
}

// A bare YYYY-MM-DD until covers that whole day.
//
// until is an exclusive bound, which is right for an instant and wrong for a
// date: "since=2026-08-11, until=2026-08-18" is the literal seven-day window
// an agent writes, and read as a raw exclusive timestamp it drops every entry
// from the 18th — the day the report most wanted. Silent, because a shorter
// result set looks exactly like a quieter week. The first version of this
// change had that bug, and its own test asserted the buggy window.
func TestSearchJournal_DateOnlyUntilCoversThatWholeDay(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	journalStore := journal.NewStore(pool)
	srv := New(nil, sessions.NewStore(pool), journalStore, nil, nil, nil, nil)

	// Midday on the 18th — inside the day, after its opening instant.
	at := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO knowledge_journal (repo, actor, event_type, payload, created_at)
		VALUES ('agent-fleet', 'worker', 'agent_note', '{"note":"today"}', $1)`, at); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp, err := srv.SearchJournal(ctx, &agentfleetv1.SearchJournalRequest{
		Since: "2026-08-11", Until: "2026-08-18",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.GetEntries()) != 1 {
		t.Fatalf("a bare date until must include that day's entries, got %d", len(resp.GetEntries()))
	}

	// A full RFC3339 until named an instant, and is honoured as one.
	resp, err = srv.SearchJournal(ctx, &agentfleetv1.SearchJournalRequest{
		Since: "2026-08-11", Until: "2026-08-18T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.GetEntries()) != 0 {
		t.Fatalf("an explicit RFC3339 until must stay exclusive, got %d entries", len(resp.GetEntries()))
	}
}

// A bad bound is a named error, not a silently empty window.
func TestSearchJournal_RejectsAnUnparseableBound(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	srv := New(nil, sessions.NewStore(pool), journal.NewStore(pool), nil, nil, nil, nil)

	_, err := srv.SearchJournal(ctx, &agentfleetv1.SearchJournalRequest{Since: "last tuesday"})
	if err == nil {
		t.Fatal("an unparseable since must error, not quietly read the whole table")
	}
	if !strings.Contains(err.Error(), "since") {
		t.Errorf("the error must name the offending field, got %v", err)
	}
}
