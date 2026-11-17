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

func TestCloseThread_MainNeverClosesAndSessionIDIsDropped(t *testing.T) {
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
		if x.Key == "a" && (!x.Closed || x.AgentSessionID != "") {
			t.Fatalf("a after close = %+v", x)
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
	closed, err := store.CloseIdleThreads(ctx, 7*24*time.Hour)
	if err != nil || len(closed) != 1 || closed[0].Key != "old" || closed[0].SessionID != id {
		t.Fatalf("closed = %+v, %v; want only old (main never closes, fresh is active)", closed, err)
	}
	if again, _ := store.CloseIdleThreads(ctx, 7*24*time.Hour); len(again) != 0 {
		t.Fatalf("closed again: %+v", again)
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
		got, err := store.SettleCrash(ctx, id)
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
	if got := settle(); got["poison"].Reason != "" {
		t.Fatalf("crash 2 settled poison: %+v", got)
	}
	// Crash 3 of the same head.
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
	for range PoisonCrashes - 1 {
		if got, err := store.SettleCrash(ctx, id); err != nil || len(got) != 0 {
			t.Fatalf("settled early: %+v %v", got, err)
		}
	}
	// The head moved on (its result landed, the cursor passed it), so a new head starts at one crash.
	if _, err := pool.Exec(ctx, `UPDATE session_threads SET cursor = 2 WHERE session_id = $1 AND thread_key = 't'`, id); err != nil {
		t.Fatal(err)
	}
	put(t, pool, id, 3, "t", "human", "discussion", "two")
	put(t, pool, id, 4, "t", "agent", "assistant", "working")
	if got, err := store.SettleCrash(ctx, id); err != nil || len(got) != 0 {
		t.Fatalf("a new head settled on its first crash: %+v %v", got, err)
	}
}

func TestRestartStanding_FreshSessionsAndCursorsAtTheEnd(t *testing.T) {
	ctx := context.Background()
	store, pool, id := newCharacter(t)
	openThread(t, pool, id, "a", 0)
	put(t, pool, id, 4, "a", "human", "discussion", "old")
	if _, err := pool.Exec(ctx, `UPDATE session_threads SET agent_session_id = 'sdk', crashes = 2, crash_seq = 4 WHERE session_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.MarkStandingStopped(ctx, id); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := store.RestartStanding(ctx, id, "sha-a", "root"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	th, _ := store.Threads(ctx, id)
	for _, x := range th {
		if x.AgentSessionID != "" || x.Cursor != 5 {
			t.Fatalf("thread %s after restart = %+v, want no SDK session and cursor 5", x.Key, x)
		}
	}
	if _, ok, _ := store.ResumeFrom(ctx, id); ok {
		t.Fatal("the stopped pod's entries are replayed into the fresh session")
	}
	var crashes int
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT crashes FROM session_threads WHERE session_id = '%s' AND thread_key = 'a'`, id)).Scan(&crashes); err != nil || crashes != 0 {
		t.Fatalf("crashes = %d (%v)", crashes, err)
	}
}
