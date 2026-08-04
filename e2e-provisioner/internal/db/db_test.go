//go:build integration

package db

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("agentfleettest"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		// See fleet-core/internal/transcript/postgres_test.go's identical
		// comment — waiting on the port alone races the image's internal
		// initdb restart and can hand back a connection that resets.
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
		CREATE TABLE tasks (id UUID PRIMARY KEY DEFAULT gen_random_uuid(), repo TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending');
		CREATE TABLE e2e_sessions (
			id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			task_id        UUID NOT NULL REFERENCES tasks(id),
			status         TEXT NOT NULL DEFAULT 'requested',
			pod_name       TEXT,
			ingress_path   TEXT,
			kill_requested BOOLEAN NOT NULL DEFAULT false,
			created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`)
	if err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return pool
}

func TestStore_SessionLifecycle(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	var taskID string
	if err := pool.QueryRow(ctx, `INSERT INTO tasks (repo) VALUES ('dream-analyst') RETURNING id`).Scan(&taskID); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	if existing, err := store.GetActiveSessionForTask(ctx, taskID); err != nil || existing != nil {
		t.Fatalf("expected no active session yet, got %+v, err %v", existing, err)
	}

	sess, err := store.CreateSession(ctx, taskID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if sess.Status != "requested" {
		t.Fatalf("expected initial status 'requested', got %q", sess.Status)
	}

	podName := "e2e-abc123"
	ingressPath := "/task-1"
	if err := store.SetSessionStatus(ctx, sess.ID, "running", &podName, &ingressPath); err != nil {
		t.Fatalf("set session status: %v", err)
	}

	active, err := store.GetActiveSessionForTask(ctx, taskID)
	if err != nil || active == nil {
		t.Fatalf("expected an active session, got %+v, err %v", active, err)
	}
	if active.Status != "running" || active.PodName == nil || *active.PodName != podName {
		t.Fatalf("unexpected active session state: %+v", active)
	}

	killed, err := store.RequestKill(ctx, taskID)
	if err != nil || !killed {
		t.Fatalf("expected RequestKill to affect a row, got killed=%v err=%v", killed, err)
	}

	due, err := pool.Query(ctx, `SELECT kill_requested FROM e2e_sessions WHERE id = $1`, sess.ID)
	if err != nil {
		t.Fatalf("query kill_requested: %v", err)
	}
	defer due.Close()
	var killRequested bool
	if due.Next() {
		if err := due.Scan(&killRequested); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	if !killRequested {
		t.Fatalf("expected kill_requested=true after RequestKill")
	}
}
