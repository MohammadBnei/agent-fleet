//go:build integration

package tasks

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

// Real Postgres, real SKIP LOCKED semantics — the property that actually
// matters for docs/adr/0020 point 2 (core claims, then commands the
// provisioner): concurrent dispatch-loop callers must never claim the same
// task twice. Mirrors transcript/postgres_test.go's container setup.
func newTestPool(t *testing.T) *pgxpool.Pool {
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

	// Minimal subset of db/schema.sql's tasks table — just the columns
	// this package's queries actually touch.
	_, err = pool.Exec(ctx, `
		CREATE EXTENSION IF NOT EXISTS pgcrypto;
		CREATE TABLE tasks (
			id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			repo               TEXT NOT NULL,
			description        TEXT NOT NULL,
			status             TEXT NOT NULL DEFAULT 'pending',
			discord_channel_id TEXT,
			discord_thread_id  TEXT,
			pr_url             TEXT,
			notes              TEXT,
			last_error         TEXT,
			planning_session_id TEXT,
			model              TEXT,
			retry_count        INT NOT NULL DEFAULT 0,
			heartbeat_at       TIMESTAMPTZ,
			lease_id           UUID,
			created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`)
	if err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return pool
}

func TestClaimNextTask_ConcurrentCallersNeverDoubleClaim(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	const n = 10
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO tasks (repo, description, discord_channel_id) VALUES ('dream-analyst', 'task', 'chan')
		`); err != nil {
			t.Fatalf("seed task %d: %v", i, err)
		}
	}

	// More concurrent claimers than tasks, so some legitimately get nil —
	// the property under test is that no task ID is ever returned twice.
	const claimers = 20
	var wg sync.WaitGroup
	claimed := make([]string, claimers)
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			task, err := store.ClaimNextTask(ctx)
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				return
			}
			if task != nil {
				claimed[i] = task.ID
			}
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	got := 0
	for _, id := range claimed {
		if id == "" {
			continue
		}
		got++
		if seen[id] {
			t.Fatalf("task %s claimed by more than one concurrent caller", id)
		}
		seen[id] = true
	}
	if got != n {
		t.Fatalf("expected exactly %d tasks claimed across %d callers, got %d", n, claimers, got)
	}
}

func TestClaimNextTask_ReclaimsStaleHeartbeat(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO tasks (repo, description, discord_channel_id, status, heartbeat_at)
		VALUES ('dream-analyst', 'task', 'chan', 'planning', now() - interval '11 minutes')
		RETURNING id
	`).Scan(&taskID); err != nil {
		t.Fatalf("seed stale task: %v", err)
	}

	task, err := store.ClaimNextTask(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if task == nil {
		t.Fatal("expected the stale-heartbeat task to be reclaimed, got nil")
	}
	if task.ID != taskID {
		t.Fatalf("claimed wrong task: got %s want %s", task.ID, taskID)
	}

	var retryCount int
	if err := pool.QueryRow(ctx, `SELECT retry_count FROM tasks WHERE id = $1`, taskID).Scan(&retryCount); err != nil {
		t.Fatalf("check retry_count: %v", err)
	}
	if retryCount != 1 {
		t.Fatalf("expected retry_count incremented to 1 on reclaim, got %d", retryCount)
	}
}

func TestCountInFlight(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	for _, status := range []string{"pending", "claimed", "planning", "implementing", "done", "failed"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO tasks (repo, description, discord_channel_id, status) VALUES ('dream-analyst', 'task', 'chan', $1)
		`, status); err != nil {
			t.Fatalf("seed %s task: %v", status, err)
		}
	}

	n, err := store.CountInFlight(ctx)
	if err != nil {
		t.Fatalf("count in flight: %v", err)
	}
	// claimed, planning, implementing count; pending, done, failed don't.
	if n != 3 {
		t.Fatalf("expected 3 in-flight tasks, got %d", n)
	}
}

// TestCreateTask_NilChannelAndThread covers the dashboard-origin path
// (DashboardService.CreateTask): no Discord channel/thread at all, unlike
// every Discord-originated task which always has a channel.
func TestCreateTask_NilChannelAndThread(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	id, err := store.CreateTask(ctx, "dream-analyst", "task from dashboard", nil, nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	task, err := store.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task == nil {
		t.Fatal("expected task to exist")
	}
	if task.ThreadID != nil {
		t.Fatalf("expected nil ThreadID, got %v", *task.ThreadID)
	}

	var channelID *string
	if err := pool.QueryRow(ctx, `SELECT discord_channel_id FROM tasks WHERE id = $1`, id).Scan(&channelID); err != nil {
		t.Fatalf("check discord_channel_id: %v", err)
	}
	if channelID != nil {
		t.Fatalf("expected NULL discord_channel_id, got %v", *channelID)
	}
}

func TestStillHoldsLease(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO tasks (repo, description, discord_channel_id) VALUES ('dream-analyst', 'task', 'chan') RETURNING id
	`).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	task, err := store.ClaimNextTask(ctx)
	if err != nil || task == nil {
		t.Fatalf("claim: task=%v err=%v", task, err)
	}

	holds, err := store.StillHoldsLease(ctx, taskID, task.LeaseID)
	if err != nil {
		t.Fatalf("still holds lease (own): %v", err)
	}
	if !holds {
		t.Fatal("expected the claiming caller to still hold its own lease")
	}

	holds, err = store.StillHoldsLease(ctx, taskID, "00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("still holds lease (wrong id): %v", err)
	}
	if holds {
		t.Fatal("expected a mismatched lease id to not hold the lease")
	}
}
