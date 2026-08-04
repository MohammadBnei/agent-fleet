// Package tasks ports bot/src/db.ts's task-creation and thread-lookup
// queries into fleet-core. This is the one place fleet-core legitimately
// touches the `tasks` table (inserting rows, thread<->task lookups) — task
// *claiming* (SELECT ... FOR UPDATE SKIP LOCKED) stays exclusively in
// worker/src/db.ts, untouched by this rewrite (see docs/adr/0013).
package tasks

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// KnownRepos mirrors bot/src/db.ts's KNOWN_REPOS.
var KnownRepos = []string{"dream-analyst", "vos-monolith"}

// JSON tags: this struct is now also serialized directly by the dashboard
// API (internal/dashboard, see docs/adr/0014) — Discord never serialized
// it to JSON before, so these tags are new but don't change any existing
// behavior.
type Task struct {
	ID          string  `json:"id"`
	Repo        string  `json:"repo"`
	Description string  `json:"description"`
	Status      string  `json:"status"`
	ThreadID    *string `json:"threadId,omitempty"`
	PrURL       *string `json:"prUrl,omitempty"`
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) CreateTask(ctx context.Context, repo, description, channelID, threadID string, skipCritique bool) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO tasks (repo, description, discord_channel_id, discord_thread_id, skip_critique)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`, repo, description, channelID, threadID, skipCritique).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}
	return id, nil
}

func (s *Store) FindTaskIDByThread(ctx context.Context, threadID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM tasks WHERE discord_thread_id = $1`, threadID).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find task by thread: %w", err)
	}
	return id, nil
}

func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	var t Task
	err := s.pool.QueryRow(ctx, `
		SELECT id, repo, description, status, discord_thread_id, pr_url FROM tasks WHERE id = $1
	`, id).Scan(&t.ID, &t.Repo, &t.Description, &t.Status, &t.ThreadID, &t.PrURL)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get task: %w", err)
	}
	return &t, nil
}

func (s *Store) ListRecentTasks(ctx context.Context, limit int) ([]Task, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, repo, description, status, discord_thread_id, pr_url
		FROM tasks ORDER BY created_at DESC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent tasks: %w", err)
	}
	defer rows.Close()

	// []Task{}, not var out []Task — a nil slice marshals to JSON `null`,
	// which the dashboard API now serializes directly (docs/adr/0014); an
	// empty task list should read as [], not crash a client's .map().
	out := []Task{}
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Repo, &t.Description, &t.Status, &t.ThreadID, &t.PrURL); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
