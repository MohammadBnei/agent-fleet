//go:build integration

package transcript

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

// This file only runs under `go test -tags=integration` (see
// .github/workflows/go.yml) — real Postgres, real transaction semantics,
// proving the two properties that actually matter for the coordination
// rewrite: the advisory lock serializes concurrent appends into a gapless
// sequence, and the idempotency-key unique index makes a retried append a
// true no-op rather than a duplicate message. dbtest.NewPool applies the
// real db/migrations/ (docs/adr/0030), which — unlike the old hand-rolled
// stub table — makes tasks.repo/description NOT NULL, so every insert into
// tasks in this package supplies them (see newTestTask below).
func newTestPool(t *testing.T) *pgxpool.Pool {
	return dbtest.NewPool(t)
}

// newTestTask inserts a minimal valid task row and returns its id — the
// real tasks table (unlike the old bare-id stub) requires repo/description.
func newTestTask(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var taskID string
	if err := pool.QueryRow(ctx, `INSERT INTO tasks (repo, description) VALUES ('dream-analyst', 'task') RETURNING id`).Scan(&taskID); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	return taskID
}

func TestPostgresStore_ConcurrentAppendsAreGaplessAndOrdered(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewPostgresStore(pool)

	taskID := newTestTask(t, ctx, pool)

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

	taskID := newTestTask(t, ctx, pool)

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

	taskID := newTestTask(t, ctx, pool)

	questionSeq, err := store.Append(ctx, taskID, "agent", `{"questions":[]}`, "question", "")
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

// created_at has been stored since the first migration and was never read
// back, so no client could say when anything happened or how long a turn
// took — seq orders a feed, it does not time one.
func TestReadSince_ReturnsCreatedAt(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewPostgresStore(pool)
	taskID := newTestTask(t, ctx, pool)

	before := time.Now().Add(-2 * time.Second)
	if _, err := store.Append(ctx, taskID, "human", "hello", "discussion", "k1"); err != nil {
		t.Fatalf("Append: %v", err)
	}

	entries, _, err := store.ReadSince(ctx, taskID, 0, 10)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no entries")
	}
	got := entries[0].CreatedAt
	if got.IsZero() {
		t.Fatal("CreatedAt is zero — the column is selected but never reaches the caller")
	}
	if got.Before(before) || got.After(time.Now().Add(2*time.Second)) {
		t.Errorf("CreatedAt %s is not around now", got)
	}
}

// A zero time must serialize as "" rather than year 1 — a client has to be
// able to tell "no timestamp" from a real one.
func TestRFC3339OrEmpty(t *testing.T) {
	if got := RFC3339OrEmpty(time.Time{}); got != "" {
		t.Errorf("zero time formatted as %q, want empty", got)
	}
	ts := time.Date(2026, 8, 12, 10, 30, 0, 0, time.UTC)
	if got := RFC3339OrEmpty(ts); got != "2026-08-12T10:30:00Z" {
		t.Errorf("got %q, want 2026-08-12T10:30:00Z", got)
	}
}
