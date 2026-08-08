//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

func newActivityTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("agentfleettest"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
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
		CREATE TABLE tasks (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			repo            TEXT NOT NULL,
			description     TEXT NOT NULL,
			guidance        TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL DEFAULT 'pending',
			discord_channel_id TEXT,
			last_active_at  TIMESTAMPTZ,
			deleted_at      TIMESTAMPTZ,
			created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`)
	if err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return pool
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
