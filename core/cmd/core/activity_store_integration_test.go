//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// dbtest.NewPool applies the real db/migrations/ (docs/adr/0030) rather
// than a hand-rolled subset.
func newActivityTestPool(t *testing.T) *pgxpool.Pool {
	return dbtest.NewPool(t)
}

// fakeTranscriptStore is the minimal transcript.Store an
// activityTrackingStore can wrap without a real transcript table — its own
// logic is what's under test here, not PostgresStore's.
type fakeTranscriptStore struct {
	appendCalls int
}

func (f *fakeTranscriptStore) Append(context.Context, string, string, string, string, string) (int64, error) {
	f.appendCalls++
	return int64(f.appendCalls), nil
}

func (f *fakeTranscriptStore) AppendReply(context.Context, string, string, string, string, string, int64) (int64, error) {
	f.appendCalls++
	return int64(f.appendCalls), nil
}

func (f *fakeTranscriptStore) ReadSince(context.Context, string, int64, int) ([]transcript.Entry, int64, error) {
	return nil, 0, nil
}

func (f *fakeTranscriptStore) LatestSeq(context.Context, string) (int64, error) {
	return 0, nil
}

// TestActivityTrackingStore_TouchesOnAppend covers the idle-timeout
// backstop's activity signal at its actual choke point — Append/
// AppendReply must bump tasks.last_active_at, not just the transcript
// entry itself.
func TestActivityTrackingStore_TouchesOnAppend(t *testing.T) {
	pool := newActivityTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)

	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO tasks (repo, description, discord_channel_id) VALUES ('dream-analyst', 'task', 'chan')
		RETURNING id
	`).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	fake := &fakeTranscriptStore{}
	store := newActivityTrackingStore(fake, taskStore)

	if _, err := store.Append(ctx, taskID, "human", "hi", "discussion", "k1"); err != nil {
		t.Fatalf("Append: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		var lastActiveAt *time.Time
		if err := pool.QueryRow(ctx, `SELECT last_active_at FROM tasks WHERE id = $1`, taskID).Scan(&lastActiveAt); err != nil {
			t.Fatalf("read last_active_at: %v", err)
		}
		if lastActiveAt != nil {
			return // touched — test passes
		}
		if time.Now().After(deadline) {
			t.Fatal("last_active_at was never set after Append (touch's fire-and-forget goroutine)")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForAwaitingHuman(t *testing.T, ctx context.Context, pool *pgxpool.Pool, taskID string, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var awaiting bool
		if err := pool.QueryRow(ctx, `SELECT awaiting_human FROM tasks WHERE id = $1`, taskID).Scan(&awaiting); err != nil {
			t.Fatalf("read awaiting_human: %v", err)
		}
		if awaiting == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("awaiting_human = %v, want %v (fire-and-forget goroutine never settled)", awaiting, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestActivityTrackingStore_AwaitingHuman covers the exact regression
// caught live (kind-local): a Kill/Interrupt entry must clear
// awaiting_human even though it never posts a real permission_response —
// worker/src/session.ts's resolveAllPendingDeny only resolves the pending
// canUseTool call in-memory, so this decorator is the only place that
// state gets reflected back into the durable task row at all.
func TestActivityTrackingStore_AwaitingHuman(t *testing.T) {
	pool := newActivityTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)

	seed := func() string {
		var taskID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO tasks (repo, description, discord_channel_id) VALUES ('dream-analyst', 'task', 'chan')
			RETURNING id
		`).Scan(&taskID); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		return taskID
	}

	t.Run("permission_request opens it, interrupt clears it without a response", func(t *testing.T) {
		taskID := seed()
		store := newActivityTrackingStore(&fakeTranscriptStore{}, taskStore)
		if _, err := store.Append(ctx, taskID, "agent", "{}", "permission_request", "k1"); err != nil {
			t.Fatalf("Append permission_request: %v", err)
		}
		waitForAwaitingHuman(t, ctx, pool, taskID, true)
		if _, err := store.Append(ctx, taskID, "human", "interrupted by human", "interrupt", "k2"); err != nil {
			t.Fatalf("Append interrupt: %v", err)
		}
		waitForAwaitingHuman(t, ctx, pool, taskID, false)
	})

	t.Run("permission_request opens it, abort clears it", func(t *testing.T) {
		taskID := seed()
		store := newActivityTrackingStore(&fakeTranscriptStore{}, taskStore)
		if _, err := store.Append(ctx, taskID, "agent", "{}", "permission_request", "k1"); err != nil {
			t.Fatalf("Append permission_request: %v", err)
		}
		waitForAwaitingHuman(t, ctx, pool, taskID, true)
		if _, err := store.Append(ctx, taskID, "human", "killed by human", "abort", "k2"); err != nil {
			t.Fatalf("Append abort: %v", err)
		}
		waitForAwaitingHuman(t, ctx, pool, taskID, false)
	})

	t.Run("question opens it, answer clears it", func(t *testing.T) {
		taskID := seed()
		store := newActivityTrackingStore(&fakeTranscriptStore{}, taskStore)
		if _, err := store.Append(ctx, taskID, "agent", `{"questions":[]}`, "question", "k1"); err != nil {
			t.Fatalf("Append question: %v", err)
		}
		waitForAwaitingHuman(t, ctx, pool, taskID, true)
		if _, err := store.AppendReply(ctx, taskID, "human", `{"answers":{}}`, "answer", "k2", 1); err != nil {
			t.Fatalf("AppendReply answer: %v", err)
		}
		waitForAwaitingHuman(t, ctx, pool, taskID, false)
	})

	t.Run("a plain discussion append never sets it", func(t *testing.T) {
		taskID := seed()
		store := newActivityTrackingStore(&fakeTranscriptStore{}, taskStore)
		if _, err := store.Append(ctx, taskID, "human", "hi", "discussion", "k1"); err != nil {
			t.Fatalf("Append discussion: %v", err)
		}
		// No fire-and-forget goroutine is even started for this msgType, so
		// there's nothing to poll for — a short sleep then a direct read is
		// the honest check here, not waitForAwaitingHuman's poll-until-match
		// (which would pass trivially on a column that was never touched).
		time.Sleep(100 * time.Millisecond)
		var awaiting bool
		if err := pool.QueryRow(ctx, `SELECT awaiting_human FROM tasks WHERE id = $1`, taskID).Scan(&awaiting); err != nil {
			t.Fatalf("read awaiting_human: %v", err)
		}
		if awaiting {
			t.Error("awaiting_human = true after a plain discussion append, want false")
		}
	})
}
