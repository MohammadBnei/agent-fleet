//go:build integration

package dashboard

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
)

// Real Postgres — Stop now writes to tasks.Store (MarkStopRequested), a
// concrete Postgres-backed type server_test.go's recordingStore-style fake
// can't stand in for. Mirrors tasks/store_test.go's/coreserver/
// server_test.go's own container setup.
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
			session_id         TEXT,
			permission_mode    TEXT,
			model              TEXT,
			retry_count        INT NOT NULL DEFAULT 0,
			heartbeat_at       TIMESTAMPTZ,
			lease_id           UUID,
			deleted_at         TIMESTAMPTZ,
			pod_phase          TEXT,
			pod_message        TEXT,
			stop_requested_at  TIMESTAMPTZ,
			created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE TABLE repos (
			name        TEXT PRIMARY KEY,
			url         TEXT NOT NULL,
			base_branch TEXT NOT NULL DEFAULT '',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`)
	if err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return pool
}

func seedTask(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO tasks (repo, description, discord_channel_id) VALUES ('dream-analyst', 'task', 'chan')
		RETURNING id
	`).Scan(&id); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return id
}

// TestServer_Stop_DefaultReason covers the plain-unit test this replaces
// (server_test.go, before Stop also wrote to tasks.Store), plus the new
// side effect: stop_requested_at gets set on the task row.
func TestServer_Stop_DefaultReason(t *testing.T) {
	pool := newTestPool(t)
	taskID := seedTask(t, pool)
	store := &recordingStore{}
	s := NewServer(tasks.NewStore(pool), store, nil, nil, nil, nil)

	resp, err := s.Stop(context.Background(), connect.NewRequest(&agentfleetv1.StopRequest{TaskId: taskID}))
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if resp.Msg.GetStatus() != "stopping" {
		t.Errorf("status = %q, want %q", resp.Msg.GetStatus(), "stopping")
	}
	if store.lastText != "stopped by human" || store.lastType != "abort" {
		t.Errorf("got (%q, %q), want (stopped by human, abort)", store.lastText, store.lastType)
	}

	var stopRequestedAt *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT stop_requested_at FROM tasks WHERE id = $1`, taskID).Scan(&stopRequestedAt); err != nil {
		t.Fatalf("read stop_requested_at: %v", err)
	}
	if stopRequestedAt == nil {
		t.Error("expected stop_requested_at to be set after Stop")
	}
}

func TestServer_Stop_CustomReason(t *testing.T) {
	pool := newTestPool(t)
	taskID := seedTask(t, pool)
	store := &recordingStore{}
	s := NewServer(tasks.NewStore(pool), store, nil, nil, nil, nil)

	reason := "wrong direction"
	req := connect.NewRequest(&agentfleetv1.StopRequest{TaskId: taskID, Reason: &reason})
	if _, err := s.Stop(context.Background(), req); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if store.lastText != "wrong direction" {
		t.Errorf("text = %q, want %q", store.lastText, "wrong direction")
	}
}
