//go:build integration

package promptsnippets

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

// dbtest.NewPool applies the real db/migrations/ (docs/adr/0030), which
// seeds 4 default snippets into prompt_snippets — tests below use fixture
// names outside that seeded set and assert containment/delta rather than
// an exact empty-table count.
func newTestPool(t *testing.T) *pgxpool.Pool {
	return dbtest.NewPool(t)
}

func TestStore_CreateGetListUpdateDelete(t *testing.T) {
	s := NewStore(newTestPool(t))
	ctx := context.Background()

	before, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List before Create: %v", err)
	}

	sn, err := s.Create(ctx, Snippet{Name: "test-snippet", Text: "push and open a PR"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sn.ID == "" {
		t.Fatalf("Create returned empty ID")
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != len(before)+1 {
		t.Fatalf("List after Create has %d entries, want %d", len(list), len(before)+1)
	}
	found := false
	for _, item := range list {
		if item == sn {
			found = true
		}
	}
	if !found {
		t.Fatalf("List = %+v, want it to contain %+v", list, sn)
	}

	got, err := s.GetByIDs(ctx, []string{sn.ID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(got) != 1 || got[0] != sn {
		t.Fatalf("GetByIDs = %+v, want [%+v]", got, sn)
	}

	updated := Snippet{ID: sn.ID, Name: "test-snippet (v2)", Text: "push, then gh pr create"}
	if err := s.Update(ctx, updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err = s.GetByIDs(ctx, []string{sn.ID})
	if err != nil || len(got) != 1 || got[0] != updated {
		t.Fatalf("GetByIDs after Update = (%+v, %v), want [%+v]", got, err, updated)
	}

	if err := s.Delete(ctx, sn.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	list, err = s.List(ctx)
	if err != nil || len(list) != len(before) {
		t.Fatalf("List after Delete = (%v, %v), want %d entries", list, err, len(before))
	}
}

func TestStore_CreateDuplicateName(t *testing.T) {
	s := NewStore(newTestPool(t))
	ctx := context.Background()

	if _, err := s.Create(ctx, Snippet{Name: "dup", Text: "a"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(ctx, Snippet{Name: "dup", Text: "b"}); !errors.Is(err, ErrExists) {
		t.Fatalf("second Create error = %v, want ErrExists", err)
	}
}

func TestStore_UpdateDeleteUnknownID(t *testing.T) {
	s := NewStore(newTestPool(t))
	ctx := context.Background()

	nope := "00000000-0000-0000-0000-000000000000"
	if err := s.Update(ctx, Snippet{ID: nope, Name: "nope", Text: "nope"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Update unknown error = %v, want pgx.ErrNoRows", err)
	}
	if err := s.Delete(ctx, nope); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Delete unknown error = %v, want pgx.ErrNoRows", err)
	}
}

func TestStore_GetByIDsEmpty(t *testing.T) {
	s := NewStore(newTestPool(t))
	ctx := context.Background()

	got, err := s.GetByIDs(ctx, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("GetByIDs(nil) = (%v, %v), want ([], nil)", got, err)
	}
}

func TestStore_SetOnChange(t *testing.T) {
	s := NewStore(newTestPool(t))
	ctx := context.Background()

	calls := 0
	s.SetOnChange(func() { calls++ })

	sn, err := s.Create(ctx, Snippet{Name: "dup", Text: "a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Update(ctx, sn); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := s.Delete(ctx, sn.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if calls != 3 {
		t.Fatalf("onChange called %d times, want 3", calls)
	}
}
