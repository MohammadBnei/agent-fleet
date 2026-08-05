// Package tasks owns every read/write against the `tasks` table — core is
// the fleet's sole Postgres-credential holder (docs/adr/0020 point 1), so
// as of that ADR this package also owns task *claiming* (SELECT ... FOR
// UPDATE SKIP LOCKED), ported here from worker/src/db.ts's claimNextTask.
// That claim used to be scoped to one repo (one worker pod per repo,
// docs/adr/0003) — here it's repo-agnostic, since concurrency is now
// fleet-wide, not per-repo (docs/adr/0019).
package tasks

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RepoConfig is the static per-repo config the dispatch loop needs to pass
// to the provisioner's CreateWorkerPod — mirrors what used to be per-repo
// Deployment env vars (k8s/dream-analyst-worker.yaml's TARGET_REPO_URL,
// k8s/vos-monolith-worker.yaml's BASE_BRANCH="dev"). A fourth copy of the
// known-repos list (dashboard/bot/e2e-provisioner each already have their
// own, per earlier repo research) — consistent with existing precedent,
// not new debt.
type RepoConfig struct {
	URL        string
	BaseBranch string // "" means the provisioner defaults to "main"
}

// KnownRepos mirrors bot/src/db.ts's KNOWN_REPOS.
var KnownRepos = map[string]RepoConfig{
	"dream-analyst": {URL: "https://github.com/MohammadBnei/dream-analyst.git"},
	"vos-monolith":  {URL: "https://github.com/MohammadBnei/vos-monolith.git", BaseBranch: "dev"},
}

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
	LeaseID     string  `json:"-"`
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// channelID/threadID are nil for a task created from the dashboard
// (DashboardService.CreateTask) — it has no Discord channel/thread at all.
// Passing nil rather than "" matters: discord/session.go's PostToThread
// checks ThreadID == nil to skip relaying, and a non-nil empty string would
// instead attempt (and fail) a ChannelMessageSend("", ...) on every relay
// tick.
func (s *Store) CreateTask(ctx context.Context, repo, description string, channelID, threadID *string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO tasks (repo, description, discord_channel_id, discord_thread_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, repo, description, channelID, threadID).Scan(&id)
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

// ClaimNextTask is core's dispatch loop's own claim (docs/adr/0020 point
// 2 — core claims, then commands the provisioner; the provisioner never
// claims tasks itself). Ported from worker/src/db.ts's claimNextTask, with
// the repo filter dropped: concurrency is fleet-wide now, not per-repo
// (docs/adr/0019), so any repo's eligible task can be claimed. Eligible:
// pending, or claimed/planning/implementing with a stale heartbeat (>10min
// — a worker pod that crashed without failing the task cleanly, docs/adr/
// 0016). Returns (nil, nil) when nothing is eligible.
func (s *Store) ClaimNextTask(ctx context.Context) (*Task, error) {
	var t Task
	err := s.pool.QueryRow(ctx, `
		UPDATE tasks
		SET status = CASE WHEN status = 'pending' THEN 'claimed' ELSE status END,
		    heartbeat_at = now(),
		    lease_id = gen_random_uuid(),
		    retry_count = CASE WHEN status != 'pending' THEN retry_count + 1 ELSE retry_count END,
		    updated_at = now()
		WHERE id = (
			SELECT id FROM tasks
			WHERE status = 'pending'
			   OR (status IN ('claimed', 'planning', 'implementing')
			       AND (heartbeat_at IS NULL OR heartbeat_at < now() - interval '10 minutes'))
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, repo, description, status, discord_thread_id, pr_url, lease_id::text
	`).Scan(&t.ID, &t.Repo, &t.Description, &t.Status, &t.ThreadID, &t.PrURL, &t.LeaseID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim next task: %w", err)
	}
	return &t, nil
}

// CountInFlight is the dispatch loop's concurrency-headroom check
// (docs/adr/0020 point 3 — Postgres, not the provisioner's event stream,
// is ground truth: it survives a core restart, where in-memory
// stream-derived state would reset to zero).
func (s *Store) CountInFlight(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM tasks WHERE status IN ('claimed', 'planning', 'implementing')
	`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count in flight: %w", err)
	}
	return n, nil
}

// UpdateHeartbeat, SetStatus, SaveSessionID, and StillHoldsLease port
// worker/src/db.ts's identically-named functions — now called by the
// sidecar's CoreService calls instead of the worker's own direct SQL
// (docs/adr/0020 point 1: only core ever holds AGENTFLEET_DB_* credentials).

func (s *Store) UpdateHeartbeat(ctx context.Context, id, leaseID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tasks SET heartbeat_at = now(), updated_at = now() WHERE id = $1 AND lease_id::text = $2
	`, id, leaseID)
	if err != nil {
		return fmt.Errorf("update heartbeat: %w", err)
	}
	return nil
}

// SetStatus mirrors worker/src/db.ts's setTaskStatus — optional fields
// only update when supplied (nil pointer leaves the column unchanged).
func (s *Store) SetStatus(ctx context.Context, id, status string, prURL, notes, lastError *string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tasks SET
			status = $2,
			pr_url = COALESCE($3, pr_url),
			notes = COALESCE($4, notes),
			last_error = COALESCE($5, last_error),
			updated_at = now()
		WHERE id = $1
	`, id, status, prURL, notes, lastError)
	if err != nil {
		return fmt.Errorf("set status: %w", err)
	}
	return nil
}

func (s *Store) SaveSessionID(ctx context.Context, id, planningSessionID, model string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tasks SET planning_session_id = $2, model = $3, updated_at = now() WHERE id = $1
	`, id, planningSessionID, model)
	if err != nil {
		return fmt.Errorf("save session id: %w", err)
	}
	return nil
}

// StillHoldsLease is checked immediately before the irreversible push/PR
// step (mirrors worker/src/db.ts's stillHoldsLease) — guards against a
// stale/reclaimed pod completing work after ClaimNextTask's
// heartbeat-staleness reclaim has already handed the task to a fresh pod.
func (s *Store) StillHoldsLease(ctx context.Context, id, leaseID string) (bool, error) {
	var holds bool
	err := s.pool.QueryRow(ctx, `
		SELECT lease_id::text = $2 FROM tasks WHERE id = $1
	`, id, leaseID).Scan(&holds)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("still holds lease: %w", err)
	}
	return holds, nil
}
