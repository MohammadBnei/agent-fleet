//go:build integration

package schedules

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

// backdate makes a schedule due now. Creating one does NOT make it due: the
// first run of an interval schedule is one interval away and a cron's is its
// next slot, which is why "run now" exists as its own path.
func backdate(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE schedules SET next_run_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}
func every(seconds int32) Schedule {
	return Schedule{Name: "etcd-health", Repo: "infra-bootstrap", Prompt: "check etcd quorum",
		IntervalSeconds: &seconds, Enabled: true}
}

func TestCRUDAndOnChange(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.NewPool(t))

	changes := 0
	s.SetOnChange(func() { changes++ })

	sc, err := s.Create(ctx, every(3600), time.Time{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !sc.Enabled {
		t.Error("new schedules should be enabled by default")
	}
	if sc.Cron != "" || sc.IntervalSeconds == nil || *sc.IntervalSeconds != 3600 {
		t.Fatalf("round-trip lost the cadence: %+v", sc)
	}

	if _, err := s.Create(ctx, every(3600), time.Time{}); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists on a duplicate (repo, name), got %v", err)
	}
	// Same name, different repo: the whole point of the repo column.
	other := every(3600)
	other.Repo = "editable-blog"
	if _, err := s.Create(ctx, other, time.Time{}); err != nil {
		t.Fatalf("the same name in another repo must be allowed: %v", err)
	}

	sc.IntervalSeconds = nil
	sc.Cron = "0 9 * * MON"
	updated, err := s.Update(ctx, sc, time.Time{})
	if err != nil {
		t.Fatalf("update to a cron cadence: %v", err)
	}
	if updated.Cron != "0 9 * * MON" || updated.IntervalSeconds != nil {
		t.Fatalf("update didn't apply: %+v", updated)
	}
	if !updated.NextRunAt.After(time.Now()) {
		t.Fatalf("a cron schedule's next run must be in the future, got %s", updated.NextRunAt)
	}

	// Pause/resume routes through Update too, and used to compute next_run_at
	// in SQL from a now-NULL interval — which failed the NOT NULL column for
	// exactly the rows this test covers.
	updated.Enabled = false
	if paused, err := s.Update(ctx, updated, time.Time{}); err != nil || paused.Enabled {
		t.Fatalf("pausing a cron schedule: %v %+v", err, paused)
	}

	if err := s.Delete(ctx, sc.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Delete(ctx, sc.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected ErrNoRows deleting twice, got %v", err)
	}

	// create + create(other repo) + update + pause + delete == 5; the failed
	// duplicate and the missing delete must not fire it.
	if changes != 5 {
		t.Errorf("expected 5 onChange fires, got %d", changes)
	}
}

func TestOneShotFiresWhenAsked(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.NewPool(t))

	when := time.Now().Add(90 * 24 * time.Hour).Truncate(time.Second)
	sc, err := s.Create(ctx, Schedule{Name: "once", Repo: "agent-fleet", Prompt: "do it once"}, when)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The bug this guards: with no run_at on the wire, a one-shot inherits the
	// column default and fires on the very next tick instead of in 90 days.
	if !sc.NextRunAt.Equal(when) {
		t.Fatalf("one-shot must fire at the time asked for, want %s got %s", when, sc.NextRunAt)
	}

	due, _, err := s.ListDue(ctx, 10)
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("a schedule due in 90 days is not due now: %+v", due)
	}
}

// The load-bearing scheduler guarantee, now that the cursor is computed in Go
// rather than inside the claiming UPDATE: two ticks racing must not both fire.
func TestClaimIsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.NewPool(t))

	sc, err := s.Create(ctx, every(3600), time.Time{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	backdate(t, s, sc.ID)
	backdate(t, s, sc.ID)
	due, now, err := s.ListDue(ctx, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("list due: %v %+v", err, due)
	}

	next, spent, err := nextRun(due[0], now)
	if err != nil {
		t.Fatal(err)
	}
	won, err := s.Claim(ctx, sc.ID, due[0].NextRunAt, next, spent)
	if err != nil || !won {
		t.Fatalf("first claim must win: %v won=%v", err, won)
	}
	// The second tick read the same row before the first one claimed it.
	won, err = s.Claim(ctx, sc.ID, due[0].NextRunAt, next, spent)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Fatal("two ticks both claimed the same run")
	}

	again, _, err := s.ListDue(ctx, 10)
	if err != nil || len(again) != 0 {
		t.Fatalf("schedule re-fired on the next tick: %v %+v", err, again)
	}
}

// A human pausing a runaway schedule between ListDue and Claim must win — and
// the claim must not write the pause back.
func TestClaimSkipsOneDisabledAfterListing(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.NewPool(t))

	sc, err := s.Create(ctx, every(3600), time.Time{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	backdate(t, s, sc.ID)
	backdate(t, s, sc.ID)
	due, now, err := s.ListDue(ctx, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("list due: %v %+v", err, due)
	}

	sc.Enabled = false
	if _, err := s.Update(ctx, sc, time.Time{}); err != nil {
		t.Fatalf("pause: %v", err)
	}

	won, err := s.Claim(ctx, sc.ID, due[0].NextRunAt, now.Add(time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Fatal("a schedule paused after listing must not fire")
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v", err)
	}
	if list[0].Enabled {
		t.Fatal("the claim un-paused a schedule a human had just paused")
	}
}

// RunNow must not move the cadence cursor: pressing it on a Sunday for a
// Monday-morning cron would otherwise cost the schedule its Monday run.
func TestRunNowLeavesTheCronAnchorAlone(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.NewPool(t))

	sc, err := s.Create(ctx, Schedule{Name: "weekly", Repo: "editable-blog",
		Prompt: "write the rundown", Cron: "0 9 * * MON", Enabled: true}, time.Time{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	anchor := sc.NextRunAt

	if _, err := s.RunNow(ctx, sc.ID); err != nil {
		t.Fatalf("run now: %v", err)
	}
	due, now, err := s.ListDue(ctx, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("a run_now schedule must be listed even when not due: %v %+v", err, due)
	}
	if due[0].DueAt(now) {
		t.Fatal("the cadence itself is not due — only the manual trigger is")
	}

	won, err := s.ClaimRunNow(ctx, sc.ID)
	if err != nil || !won {
		t.Fatalf("first run-now claim must win: %v won=%v", err, won)
	}
	if won, err = s.ClaimRunNow(ctx, sc.ID); err != nil || won {
		t.Fatalf("a consumed run-now must not fire twice: %v won=%v", err, won)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !list[0].NextRunAt.Equal(anchor) {
		t.Fatalf("run-now moved the cron anchor from %s to %s", anchor, list[0].NextRunAt)
	}
}
