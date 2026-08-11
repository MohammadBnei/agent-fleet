//go:build integration

package scheduledaudits

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

func TestCRUDAndOnChange(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.NewPool(t))

	changes := 0
	s.SetOnChange(func() { changes++ })

	a, err := s.Create(ctx, "etcd-health", "check etcd quorum", 3600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !a.Enabled {
		t.Error("new audits should be enabled by default")
	}

	if _, err := s.Create(ctx, "etcd-health", "dup", 3600); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists on duplicate name, got %v", err)
	}

	updated, err := s.Update(ctx, a.ID, "etcd-health", "check etcd quorum and disk", 7200, false)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.IntervalSeconds != 7200 || updated.Enabled {
		t.Fatalf("update didn't apply: %+v", updated)
	}

	if err := s.Delete(ctx, a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Delete(ctx, a.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected ErrNoRows deleting twice, got %v", err)
	}

	// create + update + delete == 3 (the failed duplicate and the missing
	// delete must not fire it)
	if changes != 3 {
		t.Errorf("expected 3 onChange fires, got %d", changes)
	}
}

// The load-bearing scheduler guarantee: claiming advances next_run_at in
// the same statement, so a second tick can't re-fire the same audit.
func TestClaimDueIsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.NewPool(t))

	if _, err := s.Create(ctx, "due-now", "look at things", 3600); err != nil {
		t.Fatalf("create: %v", err)
	}

	first, err := s.ClaimDue(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("expected 1 due audit, got %d", len(first))
	}

	// Immediately due again? It must not be — next_run_at moved forward.
	second, err := s.ClaimDue(ctx, 10)
	if err != nil {
		t.Fatalf("claim again: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("audit re-fired on the next tick: %+v", second)
	}
}

func TestClaimDueSkipsDisabled(t *testing.T) {
	ctx := context.Background()
	s := NewStore(dbtest.NewPool(t))

	a, err := s.Create(ctx, "paused", "should not run", 3600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.Update(ctx, a.ID, "paused", "should not run", 3600, false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	due, err := s.ClaimDue(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, d := range due {
		if d.ID == a.ID {
			t.Fatal("a disabled audit must never be claimed")
		}
	}
}
