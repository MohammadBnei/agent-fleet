//go:build integration

package sessions

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

// The sessions store is what the whole of docs/adr/0048 rests on, and every
// property below is one no compiler can check: the SQL lives in strings, and
// three separate bugs in this rewrite (`s.thread_id does not exist`,
// `column "task_id" does not exist`, and a missing GetSession handler) were
// all invisible until something ran them against a real database.
//
// Runs against real Postgres via dbtest, which applies db/migrations/ — the
// same single source the PreSync hook uses (docs/adr/0030).

func newSession(t *testing.T, ctx context.Context, s *Store) string {
	t.Helper()
	id, err := s.Create(ctx, "agent-fleet", "t", "d", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return id
}

// Create makes a row and NOTHING else. This is the human gate, expressed
// structurally: no pod exists until a message arrives, and nothing
// machine-initiated produces a message.
func TestCreate_MakesARowWithNoPodAndNoLease(t *testing.T) {
	ctx := context.Background()
	store := NewStore(dbtest.NewPool(t))

	id := newSession(t, ctx, store)
	s, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if s.PodPhase != nil {
		t.Errorf("a new session must have no pod phase, got %q", *s.PodPhase)
	}
	if s.LeaseID != "" {
		t.Errorf("a new session must hold no lease, got %q", s.LeaseID)
	}
	if s.PermissionMode == nil || *s.PermissionMode != "default" {
		t.Errorf("sessions start in CLI-parity default mode, got %v", s.PermissionMode)
	}
	live, err := store.CountLivePods(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if live != 0 {
		t.Errorf("CreateSession consumed a concurrency slot (live=%d) — it must not", live)
	}
}

// ReserveSlot resets the per-pod state, and getting this wrong is how
// docs/adr/0040's dead-pod hole reopens: a sweep would act on the PREVIOUS
// pod's history and tear down the replacement seconds after it started.
func TestReserveSlot_ResetsPerPodStateForTheNewPod(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)
	id := newSession(t, ctx, store)

	// Simulate a previous pod that was told to stop and had been heard from.
	if _, err := store.ReserveSlot(ctx, id, 5); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if err := store.TouchActive(ctx, id, "agent", "assistant"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if err := store.MarkStopRequested(ctx, id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := store.SetPodPhase(ctx, id, "POD_PHASE_TERMINATED", "stopped"); err != nil {
		t.Fatalf("phase: %v", err)
	}
	// A human moved this session out of default mode. That choice belongs to
	// the session, not the pod, and must outlive the teardown — a session
	// silently reverting to `default` on warm re-prompts for everything the
	// human already decided not to be asked about.
	if err := store.SetPermissionMode(ctx, id, "acceptEdits"); err != nil {
		t.Fatalf("set mode: %v", err)
	}

	before, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// Warm: a second pod for the same session.
	lease2, err := store.ReserveSlot(ctx, id, 5)
	if err != nil {
		t.Fatalf("warm reserve: %v", err)
	}
	after, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// stop_requested_at is a column, not a struct field — nothing reads it
	// back except the sweep query, so this asserts against the column.
	var stopStillSet bool
	if err := pool.QueryRow(ctx,
		`SELECT stop_requested_at IS NOT NULL FROM sessions WHERE id = $1`, id).Scan(&stopStillSet); err != nil {
		t.Fatalf("read stop_requested_at: %v", err)
	}
	if stopStillSet {
		t.Error("a warmed session inherited the previous pod's stop request — the grace sweep " +
			"would tear the new pod down almost immediately")
	}
	if after.ActivitySeen {
		t.Error("activity_seen survived into the new pod: the startup-stall sweep is now blind " +
			"to a replacement pod that comes up and says nothing (docs/adr/0040)")
	}
	if before.LeaseID != "" && before.LeaseID == after.LeaseID {
		t.Error("warm reused the old lease — a lingering pod could still overwrite agent_session_id")
	}
	if lease2 == "" {
		t.Error("ReserveSlot returned an empty lease")
	}
	if after.PermissionMode == nil || *after.PermissionMode != "acceptEdits" {
		t.Errorf("permission mode did not survive the warm: got %v, want acceptEdits — "+
			"the human's choice is a property of the session, not of the pod that died", after.PermissionMode)
	}
}

// A decision the previous pod was waiting on cannot survive into the next one.
//
// Nothing else closes these out: the worker denies its pending permissions on
// Stop with interrupt:true, which records no resolution, and a crashed or
// idle-swept pod never gets that far. Since pending_decisions is a live count
// over the transcript, an unresolved request keeps the session rendering as
// blocked forever — with allow/deny buttons that reach no pod, and no way for
// the replacement pod to help, because it streams from a cursor above them and
// never sees them at all.
func TestReserveSlot_ResolvesDecisionsTheDeadPodLeftBehind(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)
	id := newSession(t, ctx, store)

	appendEntry := func(seq int, kind, text string, replyTo *int) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, reply_to_seq)
			 VALUES ($1, $2, 'agent', $3, $4, $5, $6)`,
			id, seq, text, kind, fmt.Sprintf("k-%d", seq), replyTo); err != nil {
			t.Fatalf("append %s at seq %d: %v", kind, seq, err)
		}
	}

	answered := 3
	appendEntry(1, "permission_request", `{"tool":"Bash"}`, nil)
	appendEntry(2, "question", `[{"question":"which?"}]`, nil)
	appendEntry(answered, "permission_request", `{"tool":"Edit"}`, nil)
	appendEntry(4, "permission_response", `{"behavior":"allow"}`, &answered)

	before, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if before.PendingDecisions != 2 {
		t.Fatalf("fixture is wrong: want 2 unanswered decisions, got %d", before.PendingDecisions)
	}

	if _, err := store.ReserveSlot(ctx, id, 5); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	after, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.PendingDecisions != 0 {
		t.Errorf("%d decision(s) still pending after a new pod was reserved — the session will "+
			"render as blocked on a pod that no longer exists, and stay that way through every "+
			"later warm", after.PendingDecisions)
	}

	// The already-answered request must not collect a second reply: the count
	// would be right either way, but the transcript a human reads would show a
	// decision being made twice, the second time by nobody.
	var replies int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM transcript WHERE session_id = $1 AND reply_to_seq = $2`, id, answered).Scan(&replies); err != nil {
		t.Fatalf("count replies: %v", err)
	}
	if replies != 1 {
		t.Errorf("the answered request collected %d replies, want 1", replies)
	}
}

// The cap is only a real ceiling if the transaction that checks it also writes
// the state it counts.
//
// The count reads pod_phase, and the phase is otherwise set asynchronously —
// after CreateWorkerPod reaches the provisioner and its event round-trips back.
// Without this write the advisory lock would serialize callers around a number
// none of them moved, so every warm arriving inside that window would read the
// same stale count and all of them would pass.
func TestReserveSlot_CountsAgainstTheCapImmediately(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)
	id := newSession(t, ctx, store)

	if _, err := store.ReserveSlot(ctx, id, 5); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	live, err := store.CountLivePods(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if live != 1 {
		t.Errorf("live pods = %d immediately after ReserveSlot, want 1 — until the provisioner's "+
			"event arrives the slot is invisible, and concurrent warms would all be admitted", live)
	}
}

// The lease exists for exactly one reason. A torn-down pod finishing its
// shutdown must not be able to overwrite the resume identity of the pod that
// replaced it — otherwise the next warm resumes the wrong SDK session, or
// none.
func TestSaveAgentSessionID_StaleLeaseCannotClobberTheNewPod(t *testing.T) {
	ctx := context.Background()
	store := NewStore(dbtest.NewPool(t))
	id := newSession(t, ctx, store)

	oldLease, err := store.ReserveSlot(ctx, id, 5)
	if err != nil {
		t.Fatalf("reserve 1: %v", err)
	}
	ok, err := store.SaveAgentSessionID(ctx, id, "sdk-pod-1", "claude-opus-4-8", oldLease)
	if err != nil || !ok {
		t.Fatalf("first save: ok=%v err=%v", ok, err)
	}

	newLease, err := store.ReserveSlot(ctx, id, 5)
	if err != nil {
		t.Fatalf("reserve 2: %v", err)
	}
	if _, err := store.SaveAgentSessionID(ctx, id, "sdk-pod-2", "", newLease); err != nil {
		t.Fatalf("second save: %v", err)
	}

	// The dying pod-1 writes late.
	ok, err = store.SaveAgentSessionID(ctx, id, "sdk-pod-1-late", "", oldLease)
	if err != nil {
		t.Fatalf("late save errored: %v", err)
	}
	if ok {
		t.Error("a stale lease was allowed to write — this is not an error, it must be a no-op")
	}

	s, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if s.AgentSessionID == nil || *s.AgentSessionID != "sdk-pod-2" {
		t.Fatalf("resume identity clobbered by the dead pod: got %v, want sdk-pod-2", s.AgentSessionID)
	}
}

// Archived sessions must not hold slots or be reservable. An archived session
// that still counted would be a permanent, invisible drain on the cap — the
// wedge again, just arrived at from a different direction.
func TestArchive_ReleasesTheSlotAndBlocksFurtherReservation(t *testing.T) {
	ctx := context.Background()
	store := NewStore(dbtest.NewPool(t))
	id := newSession(t, ctx, store)

	if _, err := store.ReserveSlot(ctx, id, 5); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.SetPodPhase(ctx, id, "POD_PHASE_RUNNING", ""); err != nil {
		t.Fatalf("phase: %v", err)
	}
	if err := store.Archive(ctx, id); err != nil {
		t.Fatalf("archive: %v", err)
	}

	live, err := store.CountLivePods(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if live != 0 {
		t.Errorf("an archived session still counts toward the cap (live=%d)", live)
	}
	if _, err := store.ReserveSlot(ctx, id, 5); err == nil {
		t.Error("an archived session was reservable — Warm must be impossible after archive")
	}
}

// Delete is a real DELETE now, and it only works because transcript CASCADEs.
// This is the FK pair that let deleted_at/SoftDelete die; if either side is
// dropped in a future migration, this fails with a foreign-key violation
// rather than silently reintroducing the soft-delete blocker.
func TestDelete_CascadesTranscriptAndDetachesProposals(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)
	id := newSession(t, ctx, store)

	if _, err := pool.Exec(ctx, `
		INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key)
		VALUES ($1, 0, 'human', 'hello', 'discussion', 'k1')`, id); err != nil {
		t.Fatalf("insert transcript: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO proposals (repo, source, dedup_key, title, body, session_id)
		VALUES ('agent-fleet', 'alert', 'dk1', 'ttl', 'body', $1)`, id); err != nil {
		t.Fatalf("insert proposal: %v", err)
	}

	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var entries int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM transcript WHERE session_id = $1`, id).Scan(&entries); err != nil {
		t.Fatalf("count transcript: %v", err)
	}
	if entries != 0 {
		t.Errorf("transcript did not cascade: %d rows survive their session", entries)
	}

	var orphaned int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM proposals WHERE session_id IS NULL`).Scan(&orphaned); err != nil {
		t.Fatalf("count proposals: %v", err)
	}
	if orphaned != 1 {
		t.Errorf("proposal should survive with session_id NULL, got %d such rows", orphaned)
	}
}

// The three sweeps all filter on live phases, and each one tearing down the
// wrong session is a live pod killed under a human mid-conversation. They are
// exercised together because their overlap is the risky part: a session must
// appear in exactly the sweep that describes it.
func TestSweepQueries_SelectOnlyTheSessionsTheyDescribe(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)

	// Live, freshly active, never stopped — must appear in NO sweep.
	healthy := newSession(t, ctx, store)
	if _, err := store.ReserveSlot(ctx, healthy, 5); err != nil {
		t.Fatalf("reserve healthy: %v", err)
	}
	if err := store.SetPodPhase(ctx, healthy, "POD_PHASE_RUNNING", ""); err != nil {
		t.Fatalf("phase: %v", err)
	}
	if err := store.TouchActive(ctx, healthy, "human", "discussion"); err != nil {
		t.Fatalf("touch: %v", err)
	}

	// Live, stop requested well past grace.
	stopped := newSession(t, ctx, store)
	if _, err := store.ReserveSlot(ctx, stopped, 5); err != nil {
		t.Fatalf("reserve stopped: %v", err)
	}
	if err := store.SetPodPhase(ctx, stopped, "POD_PHASE_RUNNING", ""); err != nil {
		t.Fatalf("phase: %v", err)
	}
	if err := store.MarkStopRequested(ctx, stopped); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// Live, active long ago — the idle case.
	idle := newSession(t, ctx, store)
	if _, err := store.ReserveSlot(ctx, idle, 5); err != nil {
		t.Fatalf("reserve idle: %v", err)
	}
	if err := store.SetPodPhase(ctx, idle, "POD_PHASE_RUNNING", ""); err != nil {
		t.Fatalf("phase: %v", err)
	}
	if err := store.TouchActive(ctx, idle, "agent", "assistant"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE sessions SET last_active_at = now() - interval '2 hours' WHERE id = $1`, idle); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// A zero grace/timeout makes "overdue" mean "at all", so the healthy
	// session's absence is a statement about the WHERE clause, not about
	// timing luck.
	overdue, err := store.ListOverdueStopIDs(ctx, 0)
	if err != nil {
		t.Fatalf("overdue: %v", err)
	}
	if !contains(overdue, stopped) {
		t.Error("a stop past its grace was not swept — nothing else kills a runaway agent")
	}
	if contains(overdue, healthy) || contains(overdue, idle) {
		t.Errorf("stop sweep caught a session that was never stopped: %v", overdue)
	}

	idles, err := store.ListIdleSessionIDs(ctx, time.Hour)
	if err != nil {
		t.Fatalf("idle: %v", err)
	}
	if !contains(idles, idle) {
		t.Error("a session idle for 2h was not caught by a 1h idle timeout")
	}
	if contains(idles, healthy) {
		t.Error("the idle sweep caught a session active seconds ago — this kills live conversations")
	}

	// Startup stall is gated on activity_seen, which TouchActive sets.
	stalled, err := store.ListStartupStalledIDs(ctx, 0)
	if err != nil {
		t.Fatalf("stalled: %v", err)
	}
	if contains(stalled, healthy) || contains(stalled, idle) {
		t.Errorf("startup-stall caught a session that HAS been heard from: %v", stalled)
	}
	if !contains(stalled, stopped) {
		t.Error("a pod that came up and never said anything must be startup-stalled")
	}
}

// Retention GC must never reclaim the disk of a session with a live pod —
// that is deleting the working tree out from under a running agent, which is
// the uncommitted-work loss docs/DECISIONS.md records happening twice.
func TestListRetentionExpired_NeverIncludesALivePod(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)

	live := newSession(t, ctx, store)
	if err := store.SetPodPhase(ctx, live, "POD_PHASE_RUNNING", ""); err != nil {
		t.Fatalf("phase: %v", err)
	}
	dead := newSession(t, ctx, store)
	if err := store.SetPodPhase(ctx, dead, "POD_PHASE_TERMINATED", ""); err != nil {
		t.Fatalf("phase: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE sessions SET created_at = now() - interval '30 days', last_active_at = NULL`); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	expired, err := store.ListRetentionExpired(ctx, 14*24*time.Hour)
	if err != nil {
		t.Fatalf("expired: %v", err)
	}
	if contains(expired, live) {
		t.Fatal("GC selected a session with a RUNNING pod — this deletes a working tree mid-session")
	}
	if !contains(expired, dead) {
		t.Error("a 30-day-old terminated session was not GC-eligible under a 14-day retention")
	}

	// Once swept, it must not come back round — otherwise every GC pass
	// re-deletes the same directories forever.
	if err := store.MarkSwept(ctx, dead); err != nil {
		t.Fatalf("mark swept: %v", err)
	}
	expired, err = store.ListRetentionExpired(ctx, 14*24*time.Hour)
	if err != nil {
		t.Fatalf("expired 2: %v", err)
	}
	if contains(expired, dead) {
		t.Error("an already-swept session is still GC-eligible")
	}
}

// description is nullable in the schema and Session.Description is a plain
// string, so a single NULL row failed the scan — and List() returns one error
// for the whole query, so that one row emptied the console, the reconcile
// loop's view of the fleet, and the live-state gauge at once. Create() writes
// '' rather than NULL, which is exactly why this went unnoticed: docs/adr/0048
// made description a vestigial label nothing sets on purpose any more, so
// "every writer passes a non-NULL" is a convention, not a constraint.
//
// Found by seeding a row with the column omitted, which is what any hand-run
// INSERT does.
func TestList_SurvivesANullDescription(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)

	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO sessions (repo, title) VALUES ('agent-fleet', 'no description') RETURNING id`,
	).Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if _, err := store.Get(ctx, id); err != nil {
		t.Fatalf("Get with a NULL description: %v", err)
	}
	rows, err := store.List(ctx, 100, "")
	if err != nil {
		t.Fatalf("List with a NULL description in the table: %v", err)
	}
	if !contains(ids(rows), id) {
		t.Error("the NULL-description session is missing from List")
	}
}

// A word that appears only inside a transcript row — never in the session's
// own labels — must still surface that session through List's query param, and
// an empty query must leave the default ordering untouched.
func TestList_QueryMatchesTranscriptText(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)

	hit := newSession(t, ctx, store)
	miss := newSession(t, ctx, store)
	if _, err := pool.Exec(ctx, `
		INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key)
		VALUES ($1, 0, 'human', 'please investigate the flux capacitor', 'discussion', 'k1')`, hit); err != nil {
		t.Fatalf("insert transcript: %v", err)
	}

	rows, err := store.List(ctx, 100, "capacitor")
	if err != nil {
		t.Fatalf("List with query: %v", err)
	}
	if !contains(ids(rows), hit) {
		t.Error("transcript-only match missing from query results")
	}
	if contains(ids(rows), miss) {
		t.Error("non-matching session leaked into query results")
	}

	// Empty query = unfiltered listing: both sessions present.
	all, err := store.List(ctx, 100, "")
	if err != nil {
		t.Fatalf("List empty query: %v", err)
	}
	if !contains(ids(all), hit) || !contains(ids(all), miss) {
		t.Error("empty query dropped a session")
	}
}

func ids(rows []Session) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func contains(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
