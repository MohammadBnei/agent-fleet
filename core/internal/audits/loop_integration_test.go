//go:build integration

package audits

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/scheduledaudits"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
)

// An audit is machine-created, so it lands as a proposal a human has to
// approve — which is only safe because of the dedup key added alongside
// it. Without one, every cadence tick past an un-approved proposal would
// add another row, forever, with nothing to collapse them: the single
// mechanical reason audits could not be gated before.
//
// Uses the real schema (docs/adr/0030) rather than a fake store: the
// collapsing is done by a partial unique index, so a fake would be testing
// nothing at all.
func TestTick_SecondRunWhileProposalOpenIsSkipped(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()
	auditStore := scheduledaudits.NewStore(pool)
	taskStore := tasks.NewStore(pool)
	loop := New(auditStore, taskStore)

	audit, err := auditStore.Create(ctx, "etcd health", "check etcd", 60)
	if err != nil {
		t.Fatalf("create audit: %v", err)
	}

	loop.tick(ctx)
	if got := countAuditTasks(t, pool, audit.ID); got != 1 {
		t.Fatalf("first tick created %d tasks, want 1", got)
	}

	// ClaimDue advances next_run_at, so make the audit due again — this is
	// the next cadence arriving while nobody has approved the first run.
	if _, err := pool.Exec(ctx, `UPDATE scheduled_audits SET next_run_at = now() - interval '1 minute' WHERE id = $1`, audit.ID); err != nil {
		t.Fatalf("make due: %v", err)
	}
	loop.tick(ctx)

	if got := countAuditTasks(t, pool, audit.ID); got != 1 {
		t.Errorf("second tick left %d tasks, want 1 — an unapproved audit must not pile up one row per cadence", got)
	}
	if s := lastStatus(t, pool, audit.ID); s != "skipped: previous run still open" {
		t.Errorf("last_status = %q, want the skip recorded honestly rather than a task it did not create", s)
	}

	// Once the open run is finished, the key frees and the next cadence
	// proposes again — otherwise the audit would stop running forever.
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status = 'done' WHERE external_key = $1`, "audit:"+audit.ID); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE scheduled_audits SET next_run_at = now() - interval '1 minute' WHERE id = $1`, audit.ID); err != nil {
		t.Fatalf("make due: %v", err)
	}
	loop.tick(ctx)

	if got := countAuditTasks(t, pool, audit.ID); got != 2 {
		t.Errorf("after the previous run finished, got %d tasks, want 2 — the audit must resume", got)
	}
}

// The gate itself: an audit must not dispatch on its own either.
func TestTick_CreatesProposalNotPendingTask(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()
	auditStore := scheduledaudits.NewStore(pool)
	loop := New(auditStore, tasks.NewStore(pool))

	audit, err := auditStore.Create(ctx, "etcd health", "check etcd", 60)
	if err != nil {
		t.Fatalf("create audit: %v", err)
	}
	loop.tick(ctx)

	var status, kind string
	if err := pool.QueryRow(ctx, `SELECT status, kind FROM tasks WHERE external_key = $1`, "audit:"+audit.ID).Scan(&status, &kind); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if status != "proposed" {
		t.Errorf("status = %q, want %q", status, "proposed")
	}
	if kind != "thot" {
		t.Errorf("kind = %q, want %q", kind, "thot")
	}
}

func countAuditTasks(t *testing.T, pool *pgxpool.Pool, auditID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tasks WHERE external_key = $1 AND deleted_at IS NULL`, "audit:"+auditID).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	return n
}

func lastStatus(t *testing.T, pool *pgxpool.Pool, auditID string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT last_status FROM scheduled_audits WHERE id = $1`, auditID).Scan(&s); err != nil {
		t.Fatalf("read last_status: %v", err)
	}
	return s
}
