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
	"log/slog"
	"time"

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
	ID          string     `json:"id"`
	Repo        string     `json:"repo"`
	Description string     `json:"description"`
	Status      string     `json:"status"`
	ThreadID    *string    `json:"threadId,omitempty"`
	PrURL       *string    `json:"prUrl,omitempty"`
	PodPhase    *string    `json:"podPhase,omitempty"`
	PodMessage  *string    `json:"podMessage,omitempty"`
	HeartbeatAt *time.Time `json:"heartbeatAt,omitempty"`
	RetryCount  int        `json:"retryCount"`
	LastError   *string    `json:"lastError,omitempty"`
	LeaseID     string     `json:"-"`
}

type Store struct {
	pool  *pgxpool.Pool
	nudge func() // optional; set via SetNudge
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// SetNudge wires an immediate-dispatch trigger (reliability-findings.md
// #5) — called once at startup rather than threaded through the
// constructor, since dispatch.Loop itself is constructed from a *Store
// and only exists after NewStore returns. A nil nudge (the zero value,
// before SetNudge is called) is a valid no-op.
func (s *Store) SetNudge(nudge func()) {
	s.nudge = nudge
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
		slog.Error("tasks CreateTask", "repo", repo, "error", err)
		return "", fmt.Errorf("create task: %w", err)
	}
	slog.Info("tasks CreateTask", "taskId", id, "repo", repo)
	if s.nudge != nil {
		s.nudge()
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
		slog.Error("tasks FindTaskIDByThread", "threadId", threadID, "error", err)
		return "", fmt.Errorf("find task by thread: %w", err)
	}
	return id, nil
}

func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	var t Task
	err := s.pool.QueryRow(ctx, `
		SELECT id, repo, description, status, discord_thread_id, pr_url, pod_phase, pod_message,
		       heartbeat_at, retry_count, last_error
		FROM tasks WHERE id = $1 AND deleted_at IS NULL
	`, id).Scan(&t.ID, &t.Repo, &t.Description, &t.Status, &t.ThreadID, &t.PrURL, &t.PodPhase, &t.PodMessage,
		&t.HeartbeatAt, &t.RetryCount, &t.LastError)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		slog.Error("tasks GetTask", "taskId", id, "error", err)
		return nil, fmt.Errorf("get task: %w", err)
	}
	return &t, nil
}

// TaskStatusInfo is the minimal per-task slice the dashboard's
// ListWorktrees needs to left-join a worktree against its originating
// task, if one still exists (reliability-findings.md #2) — narrower than
// GetTask's full Task, which carries fields (Repo/Description/ThreadID)
// this join doesn't use.
type TaskStatusInfo struct {
	Status    string
	LastError *string
	PrURL     *string
}

// GetTaskStatusInfo returns nil (not an error) when id doesn't match any
// task — the left-join's whole point is surfacing exactly that case (an
// orphaned worktree with no task row left to explain it), not erroring on
// it.
func (s *Store) GetTaskStatusInfo(ctx context.Context, id string) (*TaskStatusInfo, error) {
	var t TaskStatusInfo
	err := s.pool.QueryRow(ctx, `
		SELECT status, last_error, pr_url FROM tasks WHERE id = $1
	`, id).Scan(&t.Status, &t.LastError, &t.PrURL)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		slog.Error("tasks GetTaskStatusInfo", "taskId", id, "error", err)
		return nil, fmt.Errorf("get task status info: %w", err)
	}
	return &t, nil
}

func (s *Store) ListRecentTasks(ctx context.Context, limit int) ([]Task, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, repo, description, status, discord_thread_id, pr_url, pod_phase, pod_message,
		       heartbeat_at, retry_count, last_error
		FROM tasks WHERE deleted_at IS NULL ORDER BY created_at DESC LIMIT $1
	`, limit)
	if err != nil {
		slog.Error("tasks ListRecentTasks", "error", err)
		return nil, fmt.Errorf("list recent tasks: %w", err)
	}
	defer rows.Close()

	// []Task{}, not var out []Task — a nil slice marshals to JSON `null`,
	// which the dashboard API now serializes directly (docs/adr/0014); an
	// empty task list should read as [], not crash a client's .map().
	out := []Task{}
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Repo, &t.Description, &t.Status, &t.ThreadID, &t.PrURL, &t.PodPhase, &t.PodMessage,
			&t.HeartbeatAt, &t.RetryCount, &t.LastError); err != nil {
			slog.Error("tasks ListRecentTasks: scan", "error", err)
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
//
// maxInFlight gates the claim itself, inside the same FOR UPDATE SKIP
// LOCKED query, instead of a separate CountInFlight call before it
// (reliability-findings.md #6) — a caller checking headroom and then
// claiming as two round trips is a TOCTOU race under >1 dispatch-loop
// replica; folding the count into the WHERE makes the whole
// check-then-claim atomic. Matches today's existing semantics: the cap
// gates every claim, including a stale-heartbeat reclaim, not just a
// fresh pending pickup (docs/adr/0020 point 3 — Postgres stays ground
// truth for concurrency, survives a core restart).
//
// maxRetries caps the reclaim itself (reliability-findings.md #1):
// retry_count was tracked but never capped before, so a task whose worker
// pod keeps crashing looped through reclaim forever. A reclaim that would
// push retry_count to maxRetries or beyond sets status =
// 'failed_permanently' instead — a terminal state, not a claim, so this
// method returns (nil, nil) for that row rather than handing back a task
// to dispatch a pod for.
func (s *Store) ClaimNextTask(ctx context.Context, maxInFlight, maxRetries int) (*Task, error) {
	var t Task
	err := s.pool.QueryRow(ctx, `
		UPDATE tasks
		SET status = CASE
		               WHEN status = 'pending' THEN 'claimed'
		               WHEN retry_count + 1 >= $2 THEN 'failed_permanently'
		               ELSE status
		             END,
		    heartbeat_at = now(),
		    lease_id = gen_random_uuid(),
		    retry_count = CASE WHEN status != 'pending' THEN retry_count + 1 ELSE retry_count END,
		    updated_at = now()
		WHERE id = (
			SELECT id FROM tasks
			WHERE (status = 'pending'
			       OR (status IN ('claimed', 'planning', 'implementing')
			           AND (heartbeat_at IS NULL OR heartbeat_at < now() - interval '10 minutes')))
			  AND (SELECT count(*) FROM tasks WHERE status IN ('claimed', 'planning', 'implementing')) < $1
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, repo, description, status, discord_thread_id, pr_url, lease_id::text
	`, maxInFlight, maxRetries).Scan(&t.ID, &t.Repo, &t.Description, &t.Status, &t.ThreadID, &t.PrURL, &t.LeaseID)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		slog.Error("tasks ClaimNextTask", "error", err)
		return nil, fmt.Errorf("claim next task: %w", err)
	}
	if t.Status == "failed_permanently" {
		slog.Warn("tasks ClaimNextTask: retries exhausted, marking failed_permanently", "taskId", t.ID, "repo", t.Repo)
		return nil, nil
	}
	slog.Info("tasks ClaimNextTask", "taskId", t.ID, "repo", t.Repo)
	return &t, nil
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
		slog.Error("tasks UpdateHeartbeat", "taskId", id, "error", err)
		return fmt.Errorf("update heartbeat: %w", err)
	}
	return nil
}

// MarkCrashed backdates heartbeat_at past the 10-minute staleness window
// so the next ClaimNextTask tick treats this task as immediately
// reclaim-eligible (reliability-findings.md #1) — a fast-path accelerant
// on top of the heartbeat fallback, reusing the existing reclaim
// mechanism rather than inventing a second one. Scoped to non-terminal
// statuses only: a crash event arriving after the task already reached a
// terminal status (a race between the provisioner's reconcile loop and
// core's own opportunistic teardown) is a harmless no-op, not an error.
// Nudges same as CreateTask — this only stays a "fast-path" accelerant if
// the dispatch loop actually wakes up promptly instead of waiting for its
// next poll tick.
func (s *Store) MarkCrashed(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tasks
		SET heartbeat_at = now() - interval '11 minutes', updated_at = now()
		WHERE id = $1 AND status IN ('claimed', 'planning', 'implementing')
	`, id)
	if err != nil {
		slog.Error("tasks MarkCrashed", "taskId", id, "error", err)
		return fmt.Errorf("mark crashed: %w", err)
	}
	slog.Warn("tasks MarkCrashed", "taskId", id)
	if s.nudge != nil {
		s.nudge()
	}
	return nil
}

// SetPodPhase records the worker pod's own lifecycle state (created/
// scheduled/running/crashed/terminated) — distinct from `status`, the
// task's business state. Called from ReportPodEvents so the dashboard can
// show worker-pod state without kubectl. Unconditional (unlike
// MarkCrashed): pod phase isn't gated on the task still being non-terminal,
// since it's just describing the pod, not driving reclaim.
func (s *Store) SetPodPhase(ctx context.Context, id, phase, message string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tasks SET pod_phase = $2, pod_message = $3, updated_at = now() WHERE id = $1
	`, id, phase, message)
	if err != nil {
		return fmt.Errorf("set pod phase: %w", err)
	}
	return nil
}

// SetStatus mirrors worker/src/db.ts's setTaskStatus — optional fields
// only update when supplied (nil pointer leaves the column unchanged).
// Nudges unconditionally rather than special-casing terminal statuses: a
// task reaching done/failed/cancelled/failed_permanently frees a
// MaxInFlight slot a queued pending task may be waiting on, and nudging on
// every call is cheap (Loop.Nudge is a non-blocking, already-debounced
// channel send) — not worth coupling this file to the status enum just to
// skip a handful of no-op ticks.
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
		slog.Error("tasks SetStatus", "taskId", id, "status", status, "error", err)
		return fmt.Errorf("set status: %w", err)
	}
	if s.nudge != nil {
		s.nudge()
	}
	return nil
}

func (s *Store) SaveSessionID(ctx context.Context, id, planningSessionID, model string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tasks SET planning_session_id = $2, model = $3, updated_at = now() WHERE id = $1
	`, id, planningSessionID, model)
	if err != nil {
		slog.Error("tasks SaveSessionID", "taskId", id, "error", err)
		return fmt.Errorf("save session id: %w", err)
	}
	return nil
}

// SoftDelete hides a task from GetTask/ListRecentTasks without a hard
// DELETE — planning_transcript/e2e_sessions both REFERENCES tasks(id) with
// no ON DELETE CASCADE, so a real DELETE would fail once a task has any
// transcript history (effectively always). Doesn't touch `status`: a
// dashboard-initiated delete of an already-`done` task shouldn't relabel
// it `cancelled` just because it's no longer listed.
func (s *Store) SoftDelete(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tasks SET deleted_at = now(), updated_at = now() WHERE id = $1
	`, id)
	if err != nil {
		slog.Error("tasks SoftDelete", "taskId", id, "error", err)
		return fmt.Errorf("soft delete task: %w", err)
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
		slog.Error("tasks StillHoldsLease", "taskId", id, "error", err)
		return false, fmt.Errorf("still holds lease: %w", err)
	}
	return holds, nil
}
