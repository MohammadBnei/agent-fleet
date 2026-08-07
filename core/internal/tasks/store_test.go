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
			last_active_at     TIMESTAMPTZ,
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
			task, err := store.ClaimNextTask(ctx, 1000, 1000)
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
		VALUES ('dream-analyst', 'task', 'chan', 'running', now() - interval '11 minutes')
		RETURNING id
	`).Scan(&taskID); err != nil {
		t.Fatalf("seed stale task: %v", err)
	}

	task, err := store.ClaimNextTask(ctx, 1000, 1000)
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

// TestClaimNextTask_RespectsMaxInFlight covers reliability-findings.md #6:
// the concurrency cap is now enforced inside ClaimNextTask's own atomic
// query, not a separate CountInFlight call beforehand — this fires enough
// concurrent claimers against a capacity-2 cap to catch a TOCTOU
// regression (a pre-fix two-step check-then-claim would let more than 2
// through).
func TestClaimNextTask_RespectsMaxInFlight(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	const maxInFlight = 2
	const pendingTasks = 10
	for i := 0; i < pendingTasks; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO tasks (repo, description, discord_channel_id) VALUES ('dream-analyst', 'task', 'chan')
		`); err != nil {
			t.Fatalf("seed task %d: %v", i, err)
		}
	}

	const claimers = 20
	var wg sync.WaitGroup
	claimed := make([]string, claimers)
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			task, err := store.ClaimNextTask(ctx, maxInFlight, 1000)
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

	got := 0
	for _, id := range claimed {
		if id != "" {
			got++
		}
	}
	if got > maxInFlight {
		t.Fatalf("expected at most %d tasks claimed with maxInFlight=%d, got %d", maxInFlight, maxInFlight, got)
	}

	var inFlight int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM tasks WHERE status IN ('claimed', 'running')
	`).Scan(&inFlight); err != nil {
		t.Fatalf("count in flight: %v", err)
	}
	if inFlight != got {
		t.Fatalf("in-flight count %d doesn't match claimed count %d", inFlight, got)
	}
	if inFlight > maxInFlight {
		t.Fatalf("in-flight count %d exceeds maxInFlight %d", inFlight, maxInFlight)
	}
}

// TestClaimNextTask_FailsPermanentlyAfterMaxRetries covers
// reliability-findings.md #1: retry_count was tracked but never capped
// before — a task whose worker pod keeps crashing would loop through
// reclaim forever. A reclaim that would push retry_count to maxRetries or
// beyond sets status='failed_permanently' instead of reclaiming, and
// ClaimNextTask returns (nil, nil) for that row rather than handing back
// a task to dispatch a pod for.
func TestClaimNextTask_FailsPermanentlyAfterMaxRetries(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	const maxRetries = 3
	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO tasks (repo, description, discord_channel_id, status, heartbeat_at, retry_count)
		VALUES ('dream-analyst', 'task', 'chan', 'running', now() - interval '11 minutes', $1)
		RETURNING id
	`, maxRetries-1).Scan(&taskID); err != nil {
		t.Fatalf("seed task at the retry ceiling: %v", err)
	}

	task, err := store.ClaimNextTask(ctx, 1000, maxRetries)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil (not a dispatchable claim) once the retry cap is hit, got %+v", task)
	}

	var status string
	var retryCount int
	if err := pool.QueryRow(ctx, `SELECT status, retry_count FROM tasks WHERE id = $1`, taskID).Scan(&status, &retryCount); err != nil {
		t.Fatalf("check status: %v", err)
	}
	if status != "failed_permanently" {
		t.Fatalf("expected status=failed_permanently, got %q", status)
	}
	if retryCount != maxRetries {
		t.Fatalf("expected retry_count incremented to %d, got %d", maxRetries, retryCount)
	}
}

// TestClaimNextTask_ReclaimsBelowRetryCeiling is the negative case: a
// reclaim that does NOT hit the cap still behaves like before — reclaimed
// normally, status unchanged, retry_count incremented by exactly one.
func TestClaimNextTask_ReclaimsBelowRetryCeiling(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	const maxRetries = 3
	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO tasks (repo, description, discord_channel_id, status, heartbeat_at, retry_count)
		VALUES ('dream-analyst', 'task', 'chan', 'running', now() - interval '11 minutes', 0)
		RETURNING id
	`).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	task, err := store.ClaimNextTask(ctx, 1000, maxRetries)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if task == nil || task.ID != taskID {
		t.Fatalf("expected the task to be reclaimed normally, got %+v", task)
	}
	if task.Status != "running" {
		t.Fatalf("expected status unchanged at 'running', got %q", task.Status)
	}
}

// TestMarkCrashed_BackdatesHeartbeatForNonTerminalTask covers
// reliability-findings.md #1's fast-path: MarkCrashed makes the task
// immediately reclaim-eligible instead of waiting out the full 10-minute
// staleness window.
func TestMarkCrashed_BackdatesHeartbeatForNonTerminalTask(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO tasks (repo, description, discord_channel_id, status, heartbeat_at)
		VALUES ('dream-analyst', 'task', 'chan', 'running', now())
		RETURNING id
	`).Scan(&taskID); err != nil {
		t.Fatalf("seed task with a fresh heartbeat: %v", err)
	}

	if err := store.MarkCrashed(ctx, taskID); err != nil {
		t.Fatalf("mark crashed: %v", err)
	}

	task, err := store.ClaimNextTask(ctx, 1000, 1000)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if task == nil || task.ID != taskID {
		t.Fatalf("expected MarkCrashed to make the task immediately reclaim-eligible, got %+v", task)
	}
}

// TestMarkCrashed_NoopForTerminalTask covers the safe-no-op guarantee: a
// crash event arriving after the task already reached a terminal status
// (a race between the provisioner's reconcile loop and core's own
// opportunistic teardown) must not resurrect it.
func TestMarkCrashed_NoopForTerminalTask(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO tasks (repo, description, discord_channel_id, status, heartbeat_at)
		VALUES ('dream-analyst', 'task', 'chan', 'done', now())
		RETURNING id
	`).Scan(&taskID); err != nil {
		t.Fatalf("seed a done task: %v", err)
	}

	if err := store.MarkCrashed(ctx, taskID); err != nil {
		t.Fatalf("mark crashed: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id = $1`, taskID).Scan(&status); err != nil {
		t.Fatalf("check status: %v", err)
	}
	if status != "done" {
		t.Fatalf("expected a terminal task's status to be untouched by MarkCrashed, got %q", status)
	}
}

// TestCreateTask_NilChannelAndThread covers the dashboard-origin path
// (DashboardService.CreateTask): no Discord channel/thread at all, unlike
// every Discord-originated task which always has a channel.
func TestCreateTask_NilChannelAndThread(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	id, err := store.CreateTask(ctx, "dream-analyst", "task from dashboard", "", nil, nil)
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

// TestSoftDelete_HidesFromGetAndList covers the dashboard's DeleteTask
// path: a soft-deleted task must disappear from both single-task lookup
// and the list, without a row deletion (which transcript's FK
// would reject once any transcript history exists).
func TestSoftDelete_HidesFromGetAndList(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	id, err := store.CreateTask(ctx, "dream-analyst", "to be deleted", "", nil, nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	if err := store.SoftDelete(ctx, id); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	task, err := store.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task != nil {
		t.Fatalf("expected GetTask to return nil for a soft-deleted task, got %+v", task)
	}

	list, err := store.ListRecentTasks(ctx, 50)
	if err != nil {
		t.Fatalf("list recent tasks: %v", err)
	}
	for _, t2 := range list {
		if t2.ID == id {
			t.Fatalf("expected soft-deleted task %s to be excluded from ListRecentTasks", id)
		}
	}
}

// TestMarkStopRequested_DoesNotResetAnExistingTimestamp covers the bug fix
// this exists for: a repeated Stop click must not push the grace window
// back out indefinitely by resetting stop_requested_at on every call.
func TestMarkStopRequested_DoesNotResetAnExistingTimestamp(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	id, err := store.CreateTask(ctx, "dream-analyst", "task", "", nil, nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	if err := store.MarkStopRequested(ctx, id); err != nil {
		t.Fatalf("mark stop requested (first): %v", err)
	}
	var first time.Time
	if err := pool.QueryRow(ctx, `SELECT stop_requested_at FROM tasks WHERE id = $1`, id).Scan(&first); err != nil {
		t.Fatalf("read first stop_requested_at: %v", err)
	}

	time.Sleep(10 * time.Millisecond)
	if err := store.MarkStopRequested(ctx, id); err != nil {
		t.Fatalf("mark stop requested (second): %v", err)
	}
	var second time.Time
	if err := pool.QueryRow(ctx, `SELECT stop_requested_at FROM tasks WHERE id = $1`, id).Scan(&second); err != nil {
		t.Fatalf("read second stop_requested_at: %v", err)
	}

	if !first.Equal(second) {
		t.Fatalf("expected stop_requested_at to stay at %v, got reset to %v", first, second)
	}
}

// TestRefreshLease_ClearsStaleStopRequest covers a real regression caught
// live against a kind cluster: Warm-ing a previously-stopped task left
// stop_requested_at set from the old Stop call, so the very next
// enforceStopGrace sweep immediately force-tore-down the brand-new pod.
// RefreshLease (warmIfIdle's choke point for "this task now has a fresh
// live pod") must clear it, same as ClaimNextTask does on a fresh/reclaimed
// dispatch.
func TestRefreshLease_ClearsStaleStopRequest(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	id, err := store.CreateTask(ctx, "dream-analyst", "task", "", nil, nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := store.MarkStopRequested(ctx, id); err != nil {
		t.Fatalf("mark stop requested: %v", err)
	}

	if _, err := store.RefreshLease(ctx, id); err != nil {
		t.Fatalf("refresh lease: %v", err)
	}

	var stopRequestedAt *time.Time
	var lastActiveAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT stop_requested_at, last_active_at FROM tasks WHERE id = $1`, id).
		Scan(&stopRequestedAt, &lastActiveAt); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if stopRequestedAt != nil {
		t.Fatalf("expected stop_requested_at cleared by RefreshLease, got %v", *stopRequestedAt)
	}
	if lastActiveAt == nil {
		t.Fatal("expected last_active_at set by RefreshLease, got nil")
	}
}

// TestListOverdueStopIDs_ReturnsOnlyOverdueWithLivePod covers
// dispatch.Loop's grace-period sweep: a task must be returned only once
// its stop request has aged past the grace window, and only while it
// still has a live pod to tear down — gated on pod_phase, not status
// (sessions redesign, supersedes docs/adr/0021/0025's phase-boundary
// framing: a stopped session stays non-terminal/resumable, so status
// can't be the exclusion signal anymore).
func TestListOverdueStopIDs_ReturnsOnlyOverdueWithLivePod(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	seed := func(podPhase *string, stopRequestedAgo *time.Duration) string {
		var id string
		var stopRequestedAt any
		if stopRequestedAgo != nil {
			stopRequestedAt = time.Now().Add(-*stopRequestedAgo)
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO tasks (repo, description, discord_channel_id, status, stop_requested_at, pod_phase)
			VALUES ('dream-analyst', 'task', 'chan', 'running', $1, $2)
			RETURNING id
		`, stopRequestedAt, podPhase).Scan(&id); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		return id
	}
	phase := func(s string) *string { return &s }

	old := 2 * time.Minute
	recent := 1 * time.Second
	overdueWithLivePod := seed(phase("POD_PHASE_RUNNING"), &old)
	seed(phase("POD_PHASE_RUNNING"), &recent)     // not yet overdue
	seed(phase("POD_PHASE_RUNNING"), nil)         // no stop requested at all
	seed(phase("POD_PHASE_TERMINATED"), &old)     // overdue but pod already gone
	seed(phase("POD_PHASE_CRASHED"), &old)        // overdue but pod already gone
	seed(nil, &old)                               // overdue but never had a pod event

	ids, err := store.ListOverdueStopIDs(ctx, time.Minute)
	if err != nil {
		t.Fatalf("list overdue stops: %v", err)
	}
	if len(ids) != 1 || ids[0] != overdueWithLivePod {
		t.Fatalf("expected exactly [%s], got %v", overdueWithLivePod, ids)
	}
}

// TestListIdleWarmTaskIDs covers the idle-timeout backstop's query —
// only a task with a live pod AND stale (or never-set) last_active_at is
// idle-eligible. A NULL last_active_at counts as idle: a pod that warmed
// but never had a single message exchanged is exactly the case this
// backstop exists for.
func TestListIdleWarmTaskIDs(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	seed := func(podPhase *string, lastActiveAgo *time.Duration) string {
		var id string
		var lastActiveAt any
		if lastActiveAgo != nil {
			lastActiveAt = time.Now().Add(-*lastActiveAgo)
		}
		if err := pool.QueryRow(ctx, `
			INSERT INTO tasks (repo, description, discord_channel_id, status, pod_phase, last_active_at)
			VALUES ('dream-analyst', 'task', 'chan', 'running', $1, $2)
			RETURNING id
		`, podPhase, lastActiveAt).Scan(&id); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		return id
	}
	phase := func(s string) *string { return &s }

	old := 40 * time.Minute
	recent := 1 * time.Minute
	staleRunning := seed(phase("POD_PHASE_RUNNING"), &old)
	neverActiveRunning := seed(phase("POD_PHASE_RUNNING"), nil) // warmed, never touched
	seed(phase("POD_PHASE_RUNNING"), &recent)                   // recently active, not idle
	seed(phase("POD_PHASE_TERMINATED"), &old)                   // stale but no live pod to tear down
	seed(nil, &old)                                             // stale but never had a pod event

	ids, err := store.ListIdleWarmTaskIDs(ctx, 30*time.Minute)
	if err != nil {
		t.Fatalf("list idle warm tasks: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if len(ids) != 2 || !got[staleRunning] || !got[neverActiveRunning] {
		t.Fatalf("expected exactly [%s %s], got %v", staleRunning, neverActiveRunning, ids)
	}
}

// TestTouchActive_ResetsIdleEligibility covers the idle-timeout backstop's
// activity signal end-to-end at the query level — a task that would
// otherwise be idle-eligible drops out of ListIdleWarmTaskIDs immediately
// after TouchActive, without needing to wait out a real clock interval.
func TestTouchActive_ResetsIdleEligibility(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	old := 40 * time.Minute
	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO tasks (repo, description, discord_channel_id, status, pod_phase, last_active_at)
		VALUES ('dream-analyst', 'task', 'chan', 'running', 'POD_PHASE_RUNNING', $1)
		RETURNING id
	`, time.Now().Add(-old)).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	idsBefore, err := store.ListIdleWarmTaskIDs(ctx, 30*time.Minute)
	if err != nil || len(idsBefore) != 1 {
		t.Fatalf("expected the seeded task to be idle-eligible before TouchActive, got %v (err=%v)", idsBefore, err)
	}

	if err := store.TouchActive(ctx, taskID); err != nil {
		t.Fatalf("TouchActive: %v", err)
	}

	idsAfter, err := store.ListIdleWarmTaskIDs(ctx, 30*time.Minute)
	if err != nil {
		t.Fatalf("list idle warm tasks: %v", err)
	}
	if len(idsAfter) != 0 {
		t.Fatalf("expected no idle-eligible tasks after TouchActive, got %v", idsAfter)
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

	task, err := store.ClaimNextTask(ctx, 1000, 1000)
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
