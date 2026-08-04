// Package db ports e2e-provisioner/src/db.ts's e2e_sessions/tasks queries.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Session struct {
	ID            string
	TaskID        string
	Status        string // requested | running | failed | torn_down
	PodName       *string
	IngressPath   *string
	KillRequested bool
}

type TaskRef struct {
	ID     string
	Repo   string
	Status string
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) GetTask(ctx context.Context, taskID string) (*TaskRef, error) {
	var t TaskRef
	err := s.pool.QueryRow(ctx, `SELECT id, repo, status FROM tasks WHERE id = $1`, taskID).
		Scan(&t.ID, &t.Repo, &t.Status)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	return &t, nil
}

// GetActiveSessionForTask mirrors getActiveSessionForTask: the one active
// (not yet torn down) session for a task, if any.
func (s *Store) GetActiveSessionForTask(ctx context.Context, taskID string) (*Session, error) {
	var sess Session
	err := s.pool.QueryRow(ctx, `
		SELECT id, task_id, status, pod_name, ingress_path, kill_requested
		FROM e2e_sessions WHERE task_id = $1 AND status IN ('requested', 'running')
		ORDER BY created_at DESC LIMIT 1
	`, taskID).Scan(&sess.ID, &sess.TaskID, &sess.Status, &sess.PodName, &sess.IngressPath, &sess.KillRequested)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get active session: %w", err)
	}
	return &sess, nil
}

func (s *Store) CreateSession(ctx context.Context, taskID string) (*Session, error) {
	var sess Session
	err := s.pool.QueryRow(ctx, `
		INSERT INTO e2e_sessions (task_id) VALUES ($1)
		RETURNING id, task_id, status, pod_name, ingress_path, kill_requested
	`, taskID).Scan(&sess.ID, &sess.TaskID, &sess.Status, &sess.PodName, &sess.IngressPath, &sess.KillRequested)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &sess, nil
}

func (s *Store) SetSessionStatus(ctx context.Context, id, status string, podName, ingressPath *string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE e2e_sessions
		SET status = $2, pod_name = COALESCE($3, pod_name), ingress_path = COALESCE($4, ingress_path), updated_at = now()
		WHERE id = $1
	`, id, status, podName, ingressPath)
	if err != nil {
		return fmt.Errorf("set session status: %w", err)
	}
	return nil
}

// RequestKill mirrors requestKill/today's TS bot's direct write — but here
// it's called from e2e-provisioner's own gRPC handler (KillE2eSession), not
// written to directly by fleet-core, preserving the "no service writes
// another service's tables" rule (see docs/adr/0013).
func (s *Store) RequestKill(ctx context.Context, taskID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE e2e_sessions SET kill_requested = true, updated_at = now()
		WHERE task_id = $1 AND status IN ('requested', 'running')
	`, taskID)
	if err != nil {
		return false, fmt.Errorf("request kill: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) SessionsNeedingTeardown(ctx context.Context) ([]Session, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT e2e_sessions.id, e2e_sessions.task_id, e2e_sessions.status,
		       e2e_sessions.pod_name, e2e_sessions.ingress_path, e2e_sessions.kill_requested
		FROM e2e_sessions
		JOIN tasks ON tasks.id = e2e_sessions.task_id
		WHERE e2e_sessions.status IN ('requested', 'running')
		  AND (tasks.status IN ('done', 'failed', 'cancelled') OR e2e_sessions.kill_requested)
	`)
	if err != nil {
		return nil, fmt.Errorf("sessions needing teardown: %w", err)
	}
	return scanSessions(rows)
}

func (s *Store) RunningSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, task_id, status, pod_name, ingress_path, kill_requested
		FROM e2e_sessions WHERE status IN ('requested', 'running')
	`)
	if err != nil {
		return nil, fmt.Errorf("running sessions: %w", err)
	}
	return scanSessions(rows)
}

func scanSessions(rows pgx.Rows) ([]Session, error) {
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.TaskID, &sess.Status, &sess.PodName, &sess.IngressPath, &sess.KillRequested); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}
