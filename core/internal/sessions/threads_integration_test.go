//go:build integration

package sessions

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

func newCharacter(t *testing.T) (*Store, *pgxpool.Pool, string) {
	t.Helper()
	pool := dbtest.NewPool(t)
	insertDefinition(t, pool, "a", "sha-a", 1)
	store := NewStore(pool)
	id, err := store.Create(context.Background(), CreateParams{
		Repo: "dream-analyst", PermissionMode: "default", DefinitionName: "a", DefinitionSHA: "sha-a",
		Owner: "client:standing:a", Visibility: "private", Lifecycle: Standing, Invoker: "standing:a", ThreadKey: "main", StartedBy: "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, pool, id
}

// put writes one transcript row as the append would, with its thread. The seq is the caller's.
func put(t *testing.T, pool *pgxpool.Pool, id string, seq int64, thread, from, typ, text string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, thread_key)
		VALUES ($1, $2, $3, $4, $5, gen_random_uuid()::text, $6)`, id, seq, from, text, typ, thread); err != nil {
		t.Fatal(err)
	}
}

func openThread(t *testing.T, pool *pgxpool.Pool, id, key string, cursor int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO session_threads (session_id, thread_key, cursor) VALUES ($1, $2, $3)`, id, key, cursor); err != nil {
		t.Fatal(err)
	}
}

// Start makes the main thread; the resume point is the lowest waiting turn-starting entry over every open
// thread, from each thread's own cursor, and LatestSeq's job when nothing waits (docs/adr/0084 §4).
func TestResumeFrom_PerThreadCursorsNotLatestSeq(t *testing.T) {
	ctx := context.Background()
	store, pool, id := newCharacter(t)
	if th, err := store.Threads(ctx, id); err != nil || len(th) != 1 || th[0].Key != "main" {
		t.Fatalf("a new character's threads = %+v, %v", th, err)
	}
	if _, ok, err := store.ResumeFrom(ctx, id); err != nil || ok {
		t.Fatalf("nothing waiting: ok %v, %v", ok, err)
	}
	openThread(t, pool, id, "a", 5)
	openThread(t, pool, id, "b", 7)
	put(t, pool, id, 3, "a", "human", "discussion", "done long ago") // below a's cursor
	put(t, pool, id, 6, "a", "agent", "discussion", "agent output")  // not turn-starting
	put(t, pool, id, 8, "b", "human", "discussion", "waiting in b")
	put(t, pool, id, 9, "a", "human", "discussion", "waiting in a")
	if got, ok, err := store.ResumeFrom(ctx, id); err != nil || !ok || got != 8 {
		t.Fatalf("ResumeFrom = %d %v %v, want 8 (b's, the lowest waiting at or above its own cursor)", got, ok, err)
	}
	// A closed thread's waiting entry delivers nothing.
	if ok, err := store.CloseThread(ctx, id, "b"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if got, ok, err := store.ResumeFrom(ctx, id); err != nil || !ok || got != 9 {
		t.Fatalf("ResumeFrom with b closed = %d %v %v, want 9", got, ok, err)
	}
}

func TestCloseThread_MainNeverClosesAndTheSessionIDIsKept(t *testing.T) {
	ctx := context.Background()
	store, pool, id := newCharacter(t)
	openThread(t, pool, id, "a", 0)
	if _, err := pool.Exec(ctx, `UPDATE session_threads SET agent_session_id = 'sdk-a' WHERE session_id = $1 AND thread_key = 'a'`, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.CloseThread(ctx, id, "main"); err != nil || ok {
		t.Fatalf("closing main = %v, %v", ok, err)
	}
	if ok, err := store.CloseThread(ctx, id, "nope"); err != nil || ok {
		t.Fatalf("closing an unknown thread = %v, %v", ok, err)
	}
	if ok, err := store.CloseThread(ctx, id, "a"); err != nil || !ok {
		t.Fatalf("closing a = %v, %v", ok, err)
	}
	if ok, _ := store.CloseThread(ctx, id, "a"); ok {
		t.Fatal("a closed twice")
	}
	th, _ := store.Threads(ctx, id)
	for _, x := range th {
		if x.Key == "a" && (!x.Closed || x.AgentSessionID != "sdk-a") {
			t.Fatalf("a after close = %+v, want parked with its SDK session (amendment A4)", x)
		}
	}
}

func TestCloseIdleThreads_SevenDaysAndNeverMain(t *testing.T) {
	ctx := context.Background()
	store, pool, id := newCharacter(t)
	openThread(t, pool, id, "old", 0)
	openThread(t, pool, id, "fresh", 0)
	if _, err := pool.Exec(ctx, `UPDATE session_threads SET last_active_at = now() - interval '8 days' WHERE session_id = $1 AND thread_key IN ('old', 'main')`, id); err != nil {
		t.Fatal(err)
	}
	idle, err := store.IdleThreads(ctx, 7*24*time.Hour)
	if err != nil || len(idle) != 1 || idle[0].Key != "old" || idle[0].SessionID != id {
		t.Fatalf("idle = %+v, %v; want only old (main never closes, fresh is active)", idle, err)
	}
	// Listing closes nothing: the caller sends the abort first, so a failed append leaves the thread for the next sweep.
	if again, _ := store.IdleThreads(ctx, 7*24*time.Hour); len(again) != 1 {
		t.Fatalf("listing closed a thread: %+v", again)
	}
	if ok, err := store.CloseThread(ctx, id, "old"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if again, _ := store.IdleThreads(ctx, 7*24*time.Hour); len(again) != 0 {
		t.Fatalf("closed thread listed again: %+v", again)
	}
}

func TestSaveThreadSessionID_LeaseScopedAndReadBack(t *testing.T) {
	ctx := context.Background()
	store, pool, id := newCharacter(t)
	openThread(t, pool, id, "a", 0)
	var lease string
	if err := pool.QueryRow(ctx, `UPDATE sessions SET profile_lease_id = gen_random_uuid(), pod_phase = 'POD_PHASE_RUNNING' WHERE id = $1 RETURNING profile_lease_id::text`, id).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.SaveThreadSessionID(ctx, id, "a", "sdk-a", "00000000-0000-0000-0000-000000000000"); err != nil || ok {
		t.Fatalf("a stale lease saved: %v %v", ok, err)
	}
	if ok, err := store.SaveThreadSessionID(ctx, id, "a", "sdk-a", lease); err != nil || !ok {
		t.Fatalf("the current lease: %v %v", ok, err)
	}
	if ok, _ := store.SaveThreadSessionID(ctx, id, "unknown", "x", lease); ok {
		t.Fatal("saved to a thread that does not exist")
	}
	th, _ := store.Threads(ctx, id)
	for _, x := range th {
		if x.Key == "a" && x.AgentSessionID != "sdk-a" {
			t.Fatalf("a = %+v", x)
		}
	}
	if ok, _ := store.SaveThreadSessionID(ctx, id, "a", "", lease); !ok {
		t.Fatal("clear refused")
	}
	if th, _ := store.Threads(ctx, id); th[1].AgentSessionID != "" && th[0].AgentSessionID != "" {
		t.Fatalf("not cleared: %+v", th)
	}
}

// A crash settles a thread's head entry only when its turn was running: an effect of it ran, or it has crashed
// three times in a row (docs/adr/0084 §4). Anything else is left for the re-warm.
func TestSettleCrash_EffectRanPoisonAndTheRest(t *testing.T) {
	ctx := context.Background()
	store, pool, id := newCharacter(t)
	openThread(t, pool, id, "fx", 0)
	openThread(t, pool, id, "poison", 0)
	openThread(t, pool, id, "idle", 0)
	openThread(t, pool, id, "late", 0)
	put(t, pool, id, 1, "fx", "human", "discussion", "send it")
	put(t, pool, id, 2, "fx", "agent", "assistant", "working") // the turn started
	put(t, pool, id, 3, "poison", "human", "discussion", "boom")
	put(t, pool, id, 4, "poison", "agent", "assistant", "working")
	put(t, pool, id, 5, "idle", "human", "discussion", "never started") // no agent output: delivered, not running
	// late: a previous turn's output (and its result) lies above the new head, so it is not evidence.
	put(t, pool, id, 6, "late", "human", "discussion", "second message")
	put(t, pool, id, 7, "late", "agent", "assistant", "the first turn's tail")
	put(t, pool, id, 8, "late", "agent", "result", `{"consumedSeq":0}`)
	// early: output from before the head, no result: the head's turn never started.
	openThread(t, pool, id, "early", 0)
	put(t, pool, id, 9, "early", "agent", "assistant", "an old turn")
	put(t, pool, id, 10, "early", "human", "discussion", "waiting")

	settle := func() map[string]Settled {
		t.Helper()
		got, _, err := store.SettleCrash(ctx, id, false)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]Settled{}
		for _, s := range got {
			m[s.Key] = s
		}
		return m
	}
	if err := store.NoteEffect(ctx, id, "fx", 1); err != nil {
		t.Fatal(err)
	}
	// Crash 1: only the entry an effect ran for is settled; poison has crashed once, idle and late not at all.
	if got := settle(); len(got) != 1 || got["fx"].Seq != 1 || got["fx"].Reason != "effect" {
		t.Fatalf("crash 1 settled %+v, want fx@1 by effect", got)
	}
	// The same crash reported again (the provisioner restarted, or a re-warm failed before the pod ran) has no
	// new agent output above the last settle: it is not a second crash.
	for range 3 {
		if got := settle(); got["poison"].Reason != "" {
			t.Fatalf("a repeated report settled poison: %+v", got)
		}
	}
	// Crash 2: the re-warmed pod ran the entry again and died.
	put(t, pool, id, 11, "poison", "agent", "assistant", "working again")
	if got := settle(); got["poison"].Reason != "" {
		t.Fatalf("crash 2 settled poison: %+v", got)
	}
	// Crash 3 of the same head.
	put(t, pool, id, 12, "poison", "agent", "assistant", "and again")
	if got := settle(); got["poison"].Seq != 3 || got["poison"].Reason != "poison" {
		t.Fatalf("crash 3 settled %+v, want poison@3", got)
	}
	if got := settle(); got["idle"].Reason != "" || got["late"].Reason != "" {
		t.Fatalf("an entry whose turn never started, or whose evidence is an earlier turn's, was settled: %+v", got)
	}
	var n int
	for _, th := range []string{"idle", "late", "early"} {
		if err := pool.QueryRow(ctx, `SELECT crashes FROM session_threads WHERE session_id = $1 AND thread_key = $2`, id, th).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s crashes = %d (%v): a turn that never started was counted as a crash", th, n, err)
		}
	}
}

func TestSettleCrash_ADifferentHeadRestartsTheCount(t *testing.T) {
	ctx := context.Background()
	store, pool, id := newCharacter(t)
	openThread(t, pool, id, "t", 0)
	put(t, pool, id, 1, "t", "human", "discussion", "one")
	put(t, pool, id, 2, "t", "agent", "assistant", "working")
	for i := range PoisonCrashes - 1 {
		if i > 0 {
			put(t, pool, id, int64(10+i), "t", "agent", "assistant", "working again")
		}
		if got, _, err := store.SettleCrash(ctx, id, false); err != nil || len(got) != 0 {
			t.Fatalf("settled early: %+v %v", got, err)
		}
	}
	// The head moved on (its result landed, the cursor passed it), so a new head starts at one crash.
	if _, err := pool.Exec(ctx, `UPDATE session_threads SET cursor = 2 WHERE session_id = $1 AND thread_key = 't'`, id); err != nil {
		t.Fatal(err)
	}
	put(t, pool, id, 3, "t", "human", "discussion", "two")
	put(t, pool, id, 20, "t", "agent", "assistant", "working")
	if got, _, err := store.SettleCrash(ctx, id, false); err != nil || len(got) != 0 {
		t.Fatalf("a new head settled on its first crash: %+v %v", got, err)
	}
}

// docs/adr/0084 amendment A4: Start keeps every thread's SDK session id and cursor, so a waiting entry is delivered
// after it and nothing is lost; only the crash history restarts. A live old pod still refuses it.
func TestRestartStanding_KeepsSessionsAndCursors(t *testing.T) {
	ctx := context.Background()
	store, pool, id := newCharacter(t)
	openThread(t, pool, id, "a", 0)
	put(t, pool, id, 4, "a", "human", "discussion", "waiting")
	if _, err := pool.Exec(ctx, `UPDATE session_threads SET agent_session_id = 'sdk', crashes = 2, crash_seq = 4 WHERE session_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET agent_session_id = 'sdk-main' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.MarkStandingStopped(ctx, id); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET pod_phase = 'POD_PHASE_RUNNING' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.RestartStanding(ctx, id, "sha-a", "root", ""); err != nil || ok {
		t.Fatalf("a start over a live pod = %v, %v", ok, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET pod_phase = 'POD_PHASE_TERMINATED' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	th, _ := store.Threads(ctx, id)
	before := map[string]Thread{}
	for _, x := range th {
		before[x.Key] = x
	}
	if ok, err := store.RestartStanding(ctx, id, "sha-a", "root", ""); err != nil || !ok {
		t.Fatal(ok, err)
	}
	th, _ = store.Threads(ctx, id)
	for _, x := range th {
		if x.Cursor != before[x.Key].Cursor || x.AgentSessionID != before[x.Key].AgentSessionID {
			t.Fatalf("thread %s after restart = %+v, want %+v: cursor and SDK session kept", x.Key, x, before[x.Key])
		}
		if x.Key == "a" && x.AgentSessionID != "sdk" {
			t.Fatalf("a lost its SDK session: %+v", x)
		}
	}
	if got, _ := store.Get(ctx, id); got.AgentSessionID == nil || *got.AgentSessionID != "sdk-main" {
		t.Fatalf("main's SDK session = %v, want it kept", got.AgentSessionID)
	}
	if from, ok, _ := store.ResumeFrom(ctx, id); !ok || from != 4 {
		t.Fatalf("ResumeFrom = %d %v, want the waiting entry 4", from, ok)
	}
	if rs, err := store.RestartSeq(ctx, id); err != nil || rs != 4 {
		t.Fatalf("RestartSeq = %d, %v, want 4: the transcript's last seq at Start", rs, err)
	}
	var crashes int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT crashes FROM session_threads WHERE session_id = '%s' AND thread_key = 'a'`, id)).Scan(&crashes); err != nil || crashes != 0 {
		t.Fatalf("crashes = %d (%v)", crashes, err)
	}
}

// RunningHeads names each thread with a turn in flight and the entry it answers: Stop settles those and no other.
func TestRunningHeads(t *testing.T) {
	ctx := context.Background()
	store, pool, id := newCharacter(t)
	openThread(t, pool, id, "a", 0)
	openThread(t, pool, id, "b", 0)
	put(t, pool, id, 1, "a", "human", "discussion", "running")
	put(t, pool, id, 2, "a", "agent", "assistant", "on it")
	put(t, pool, id, 3, "b", "human", "discussion", "waiting")
	got, err := store.RunningHeads(ctx, id)
	if err != nil || len(got) != 1 || got["a"] != 1 {
		t.Fatalf("running heads = %v, %v, want only a at 1", got, err)
	}
}

// docs/adr/0084 §9: turns are the worker's results since Paris midnight; a crash result is no turn, and
// yesterday's do not count. The hold is the budget spent with no override standing.
func TestBudgetHeld(t *testing.T) {
	ctx := context.Background()
	store, pool, id := newCharacter(t)
	resBy := func(from string, seq int64, text, age string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, thread_key, created_at)
			VALUES ($1, $2, $5, $3, 'result', gen_random_uuid()::text, 'main', now() - $4::interval)`, id, seq, text, age, from); err != nil {
			t.Fatal(err)
		}
	}
	res := func(seq int64, text, age string) { resBy("worker", seq, text, age) }
	held := func(want bool) {
		t.Helper()
		if got, err := store.BudgetHeld(ctx, id); err != nil || got != want {
			t.Fatalf("BudgetHeld = %v, %v; want %v", got, err, want)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_definitions SET effective = jsonb_set(effective, '{limits}', '{"turnsPerDay":2}')`); err != nil {
		t.Fatal(err)
	}
	res(1, `{"subtype":"success"}`, "30 hours")
	resBy("fleet", 2, `{"subtype":"success","crash":true}`, "0 seconds")
	res(3, `{"subtype":"success"}`, "0 seconds")
	held(false)
	// A worker result whose agent-controlled tool input holds "crash":true is still a turn.
	res(4, `{"subtype":"success","permission_denials":[{"tool_input":{"crash":true}}]}`, "0 seconds")
	today, _, _, err := store.Turns(ctx, id)
	if err != nil || today != 2 {
		t.Fatalf("Turns today = %d, %v; want 2", today, err)
	}
	held(true)
	if err := store.RaiseBudget(ctx, id); err != nil {
		t.Fatal(err)
	}
	held(false)
	if _, err := pool.Exec(ctx, `UPDATE sessions SET budget_override_until = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	held(true)
}

// The day is Europe/Paris's: a result a second after its midnight is today's, one a second before is not.
func TestTurns_ParisDay(t *testing.T) {
	ctx := context.Background()
	store, pool, id := newCharacter(t)
	var start time.Time
	if err := pool.QueryRow(ctx, `SELECT date_trunc('day', now() AT TIME ZONE 'Europe/Paris') AT TIME ZONE 'Europe/Paris'`).Scan(&start); err != nil {
		t.Fatal(err)
	}
	for i, off := range []string{"-1 second", "+1 second"} {
		if _, err := pool.Exec(ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, thread_key, created_at)
			VALUES ($1, $2, 'worker', '{"subtype":"success"}', 'result', gen_random_uuid()::text, 'main', $3::timestamptz + $4::interval)`, id, i+1, start, off); err != nil {
			t.Fatal(err)
		}
	}
	if today, _, _, err := store.Turns(ctx, id); err != nil || today != 1 {
		t.Fatalf("today = %d, %v; want 1", today, err)
	}
}
