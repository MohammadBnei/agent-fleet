//go:build integration

package transcript

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
)

// newStanding makes a standing character's row, as Start does.
func newStanding(t *testing.T, ctx context.Context, store *PostgresStore) string {
	t.Helper()
	if _, err := store.pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid)
		VALUES ('a', 'sha-a', '{}', '{"limits":{"concurrency":1},"mode":{"default":"default","allow":["default"]}}', true)`); err != nil {
		t.Fatal(err)
	}
	id, err := sessions.NewStore(store.pool).Create(ctx, sessions.CreateParams{
		Repo: "dream-analyst", PermissionMode: "default", DefinitionName: "a", DefinitionSHA: "sha-a",
		Owner: "client:standing:a", Visibility: "private", Lifecycle: sessions.Standing, Invoker: "standing:a", ThreadKey: "main", StartedBy: "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func cursor(t *testing.T, ctx context.Context, store *PostgresStore, id, thread string) (c int64, closed bool) {
	t.Helper()
	if err := store.pool.QueryRow(ctx, `SELECT cursor, closed_at IS NOT NULL FROM session_threads WHERE session_id = $1 AND thread_key = $2`, id, thread).Scan(&c, &closed); err != nil {
		t.Fatalf("thread %q: %v", thread, err)
	}
	return
}

// docs/adr/0084 §4: an entry carries its thread; the owner's message opens a thread; a result moves only its own
// thread's cursor; an answer lands in the thread of the prompt it answers.
func TestThreads_EntriesOpenAndAdvanceTheirOwnThread(t *testing.T) {
	ctx := context.Background()
	store := NewPostgresStore(newTestPool(t), noHook)
	id := newStanding(t, ctx, store)
	app := func(thread, from, text, typ string) int64 {
		t.Helper()
		seq, err := store.Append(WithThread(ctx, thread), id, from, text, typ, "")
		if err != nil {
			t.Fatalf("append in %q: %v", thread, err)
		}
		return seq
	}

	m1 := app("topic-a", "human", "hello", "discussion") // seq 0 opens topic-a
	m2 := app("topic-b", "human", "other", "discussion")
	if c, closed := cursor(t, ctx, store, id, "topic-a"); c != m1 || closed {
		t.Fatalf("topic-a cursor %d closed %v, want %d open", c, closed, m1)
	}
	if c, _ := cursor(t, ctx, store, id, "topic-b"); c != m2 {
		t.Fatalf("topic-b cursor %d, want %d", c, m2)
	}
	// An agent entry opens nothing.
	req := app("topic-a", "agent", `{"tool":"Bash"}`, "permission_request")
	if _, err := store.Append(WithThread(ctx, "topic-c"), id, "agent", "x", "discussion", ""); err != nil {
		t.Fatal(err) // allowed: the row is standing
	}
	var n int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM session_threads WHERE session_id = $1`, id).Scan(&n); err != nil || n != 3 {
		t.Fatalf("threads = %d (%v), want main, topic-a, topic-b", n, err)
	}

	// A result moves its own thread past consumedSeq, and no other.
	app("topic-a", "agent", fmt.Sprintf(`{"subtype":"success","consumedSeq":%d}`, m1), "result")
	if c, _ := cursor(t, ctx, store, id, "topic-a"); c != m1+1 {
		t.Fatalf("topic-a cursor %d after its result, want %d", c, m1+1)
	}
	if c, _ := cursor(t, ctx, store, id, "topic-b"); c != m2 {
		t.Fatalf("topic-b cursor moved to %d by another thread's result", c)
	}
	// A cursor never moves back: a result for an older entry (a retried one, a late one) leaves it where it is.
	if _, err := store.pool.Exec(ctx, `UPDATE session_threads SET cursor = 50 WHERE session_id = $1 AND thread_key = 'topic-a'`, id); err != nil {
		t.Fatal(err)
	}
	app("topic-a", "agent", `{"subtype":"success","consumedSeq":0}`, "result")
	if c, _ := cursor(t, ctx, store, id, "topic-a"); c != 50 {
		t.Fatalf("topic-a cursor went back to %d", c)
	}

	// The owner's answer to a prompt in topic-a, appended with no thread named, lands in topic-a.
	ans, err := store.AppendReply(ctx, id, "human", `{"behavior":"allow"}`, "permission_response", "", req)
	if err != nil {
		t.Fatal(err)
	}
	es, _, err := store.ReadSince(ctx, id, ans, 1)
	if err != nil || len(es) != 1 || es[0].ThreadKey != "topic-a" {
		t.Fatalf("the answer's thread = %+v (%v), want topic-a", es, err)
	}
}

func TestThreads_ClosedThreadReopensAfresh_BadThreadsRefused(t *testing.T) {
	ctx := context.Background()
	store := NewPostgresStore(newTestPool(t), noHook)
	id := newStanding(t, ctx, store)
	m1, err := store.Append(WithThread(ctx, "t1"), id, "human", "one", "discussion", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE session_threads SET agent_session_id = 'sdk', crashes = 2, closed_at = now(), cursor = 99 WHERE session_id = $1 AND thread_key = 't1'`, id); err != nil {
		t.Fatal(err)
	}
	// A later owner message reopens the key from that entry, with no SDK session and no crash history.
	m2, err := store.Append(WithThread(ctx, "t1"), id, "human", "again", "discussion", "")
	if err != nil {
		t.Fatal(err)
	}
	var sdk *string
	var crashes int
	var cur int64
	var closed bool
	if err := store.pool.QueryRow(ctx, `SELECT agent_session_id, crashes, cursor, closed_at IS NOT NULL FROM session_threads WHERE session_id = $1 AND thread_key = 't1'`, id).Scan(&sdk, &crashes, &cur, &closed); err != nil {
		t.Fatal(err)
	}
	if sdk != nil || crashes != 0 || cur != m2 || closed || m2 == m1 {
		t.Fatalf("reopened: sdk %v crashes %d cursor %d (want %d) closed %v", sdk, crashes, cur, m2, closed)
	}

	for _, bad := range []string{"UPPER", "has space", "-lead", "a_b", "x234567890123456789012345678901234"} {
		if _, err := store.Append(WithThread(ctx, bad), id, "human", "x", "discussion", ""); !errors.Is(err, ErrBadThread) {
			t.Errorf("thread %q: %v, want ErrBadThread", bad, err)
		}
	}
	// Only a standing row has threads.
	plain := newTestTask(t, ctx, store.pool)
	if _, err := store.Append(WithThread(ctx, "t1"), plain, "human", "x", "discussion", ""); !errors.Is(err, ErrBadThread) {
		t.Errorf("a thread on a resident row: %v, want ErrBadThread", err)
	}
	if _, err := store.Append(ctx, plain, "human", "x", "discussion", ""); err != nil {
		t.Errorf("main on a resident row: %v", err)
	}
}
