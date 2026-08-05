//go:build integration

package transcript

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// This file only runs under `go test -tags=integration` (see
// .github/workflows/go.yml) — real Postgres, real transaction semantics,
// proving the two properties that actually matter for the coordination
// rewrite: the advisory lock serializes concurrent appends into a gapless
// sequence, and the idempotency-key unique index makes a retried append a
// true no-op rather than a duplicate message.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("agentfleettest"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		// The official postgres image logs "database system is ready to
		// accept connections" twice — once before an internal restart
		// during initdb, once after. Waiting on the port alone (as this
		// used to) lets a connection land in that gap and get a "connection
		// reset by peer" (hit in CI, not locally — timing-dependent).
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `
		CREATE EXTENSION IF NOT EXISTS pgcrypto;
		CREATE TABLE tasks (id UUID PRIMARY KEY DEFAULT gen_random_uuid());
		CREATE TABLE planning_transcript (
			task_id            UUID NOT NULL REFERENCES tasks(id),
			seq                BIGINT NOT NULL,
			"from"             TEXT NOT NULL,
			text               TEXT NOT NULL,
			type               TEXT,
			idempotency_key    TEXT NOT NULL,
			created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
			relayed_to_discord BOOLEAN NOT NULL DEFAULT false,
			relay_attempts     INT NOT NULL DEFAULT 0,
			relay_dead_letter  BOOLEAN NOT NULL DEFAULT false,
			relay_last_error   TEXT,
			reply_to_seq       BIGINT,
			PRIMARY KEY (task_id, seq)
		);
		CREATE UNIQUE INDEX planning_transcript_idempotency_idx
			ON planning_transcript (task_id, idempotency_key);
	`)
	if err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return pool
}

func TestPostgresStore_ConcurrentAppendsAreGaplessAndOrdered(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewPostgresStore(pool)

	var taskID string
	if err := pool.QueryRow(ctx, `INSERT INTO tasks DEFAULT VALUES RETURNING id`).Scan(&taskID); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	seqs := make([]int64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seq, err := store.Append(ctx, taskID, "proposer", "msg", "discussion", "")
			if err != nil {
				t.Errorf("append %d: %v", i, err)
				return
			}
			seqs[i] = seq
		}(i)
	}
	wg.Wait()

	seen := make(map[int64]bool, n)
	for _, s := range seqs {
		if seen[s] {
			t.Fatalf("duplicate seq assigned under concurrency: %d (seqs=%v)", s, seqs)
		}
		seen[s] = true
	}
	for i := int64(0); i < n; i++ {
		if !seen[i] {
			t.Fatalf("seq sequence has a gap — missing %d (seqs=%v)", i, seqs)
		}
	}
}

func TestPostgresStore_IdempotentAppendDoesNotDuplicate(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewPostgresStore(pool)

	var taskID string
	if err := pool.QueryRow(ctx, `INSERT INTO tasks DEFAULT VALUES RETURNING id`).Scan(&taskID); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	seq1, err := store.Append(ctx, taskID, "human", "approved", "approve", "fixed-key-1")
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	seq2, err := store.Append(ctx, taskID, "human", "approved", "approve", "fixed-key-1")
	if err != nil {
		t.Fatalf("retried append: %v", err)
	}
	if seq1 != seq2 {
		t.Fatalf("retried append with same idempotency key got a different seq: %d vs %d", seq1, seq2)
	}

	entries, _, err := store.ReadSince(ctx, taskID, 0, 100)
	if err != nil {
		t.Fatalf("read since: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 entry (no duplicate), got %d", len(entries))
	}
}

// TestPostgresStore_AppendReplyRoundTripsReplyTo covers reliability-
// findings.md #0's question-seq correlation: AppendReply's replyToSeq
// must survive a real INSERT/SELECT round trip through ReadSince, since
// that's what AskUserQuestion's own matching loop reads back.
func TestPostgresStore_AppendReplyRoundTripsReplyTo(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewPostgresStore(pool)

	var taskID string
	if err := pool.QueryRow(ctx, `INSERT INTO tasks DEFAULT VALUES RETURNING id`).Scan(&taskID); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	questionSeq, err := store.Append(ctx, taskID, "planner", `{"questions":[]}`, "question", "")
	if err != nil {
		t.Fatalf("append question: %v", err)
	}
	if _, err := store.AppendReply(ctx, taskID, "human", `{"answers":{}}`, "answer", "", questionSeq); err != nil {
		t.Fatalf("append reply: %v", err)
	}

	entries, _, err := store.ReadSince(ctx, taskID, 0, 100)
	if err != nil {
		t.Fatalf("read since: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	answer := entries[1]
	if answer.Type != "answer" || answer.ReplyTo == nil || *answer.ReplyTo != questionSeq {
		t.Fatalf("expected the answer's ReplyTo to round-trip as %d, got %+v", questionSeq, answer)
	}
	if entries[0].ReplyTo != nil {
		t.Fatalf("expected the question entry's own ReplyTo to be nil, got %v", *entries[0].ReplyTo)
	}
}
