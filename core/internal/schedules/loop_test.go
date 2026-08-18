//go:build integration

package schedules

import (
	"context"
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

type filed struct {
	repo, source, dedupKey, title, body string
}

type stubRunner struct {
	calls []filed
	next  int
}

func (r *stubRunner) Create(_ context.Context, repo, source, dedupKey, title, body string) (string, bool, error) {
	r.calls = append(r.calls, filed{repo, source, dedupKey, title, body})
	r.next++
	return "proposal-id", true, nil
}

// The bug this whole change started from: the repo was a Go constant, so every
// schedule filed against infra-bootstrap no matter what it targeted. Asserting
// on the proposal's repo specifically — sessions.Create does not validate the
// repo, so an assertion further downstream would pass with any string at all.
func TestTickFilesAgainstTheSchedulesRepo(t *testing.T) {
	ctx := context.Background()
	store := NewStore(dbtest.NewPool(t))
	runner := &stubRunner{}
	loop := NewLoop(store, runner)

	sc, err := store.Create(ctx, Schedule{Name: "weekly-rundown", Repo: "editable-blog",
		Prompt: "Run the weekly-rundown skill.", IntervalSeconds: ptr(604800), Enabled: true},
		time.Time{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Creating it does not make it due — the first run is one interval away.
	backdate(t, store, sc.ID)

	loop.tick(ctx)

	if len(runner.calls) != 1 {
		t.Fatalf("expected exactly one proposal, got %d", len(runner.calls))
	}
	got := runner.calls[0]
	if got.repo != "editable-blog" {
		t.Fatalf("filed against %q, want editable-blog", got.repo)
	}
	// proposals.source is a CHECK-constrained enum; a value outside it fails
	// every insert and the loop only records that in last_status.
	if got.source != "schedule" {
		t.Fatalf("source %q is not one the proposals CHECK allows", got.source)
	}

	// The interval said a week, so a second tick immediately after must not
	// file again.
	loop.tick(ctx)
	if len(runner.calls) != 1 {
		t.Fatalf("a weekly schedule fired twice in one second: %+v", runner.calls)
	}
}

func TestTickDisablesACronThatCanNeverFire(t *testing.T) {
	ctx := context.Background()
	store := NewStore(dbtest.NewPool(t))
	runner := &stubRunner{}
	loop := NewLoop(store, runner)

	// Written straight to the table: Create rejects this at the trust
	// boundary, which is the point — this covers a row that got in before the
	// validation existed, or by hand.
	sc, err := store.Create(ctx, Schedule{Name: "leap", Repo: "agent-fleet", Prompt: "x",
		Cron: "0 9 * * MON", Enabled: true}, time.Time{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.pool.Exec(ctx,
		`UPDATE schedules SET cron = '0 0 30 2 *', next_run_at = now() WHERE id = $1`, sc.ID); err != nil {
		t.Fatal(err)
	}

	loop.tick(ctx)
	loop.tick(ctx)

	if len(runner.calls) != 0 {
		t.Fatalf("a schedule that can never fire must not file: %+v", runner.calls)
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Left enabled, it would be due on every tick forever.
	if list[0].Enabled {
		t.Fatal("an unsatisfiable cron must be disabled, not retried every 60s")
	}
	if list[0].LastStatus == "" {
		t.Fatal("disabling a schedule silently is how a dead cadence goes unnoticed")
	}
}
