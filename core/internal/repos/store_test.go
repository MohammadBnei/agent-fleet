//go:build integration

package repos

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

// dbtest.NewPool applies the real db/migrations/ (docs/adr/0030), which
// seeds 'dream-analyst'/'vos-monolith'/'agent-fleet' rows into repos —
// tests below use a fixture name outside that seeded set so they don't
// collide with it.
func newTestPool(t *testing.T) *pgxpool.Pool {
	return dbtest.NewPool(t)
}

func TestStore_CreateGetListUpdateDelete(t *testing.T) {
	s := NewStore(newTestPool(t))
	ctx := context.Background()

	if got, err := s.Get(ctx, "test-repo"); err != nil || got != nil {
		t.Fatalf("Get before Create = (%v, %v), want (nil, nil)", got, err)
	}

	r := Repo{Name: "test-repo", URL: "https://example.com/test-repo.git", BaseBranch: "dev"}
	if err := s.Create(ctx, r); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "test-repo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || *got != r {
		t.Fatalf("Get = %+v, want %+v", got, r)
	}

	// List includes the migration's seeded repos alongside the one just
	// created (docs/adr/0030) — assert containment, not an exact count.
	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, item := range list {
		if item == r {
			found = true
		}
	}
	if !found {
		t.Fatalf("List = %+v, want it to contain %+v", list, r)
	}

	updated := Repo{Name: "test-repo", URL: "https://example.com/test-repo-v2.git", BaseBranch: "main"}
	if err := s.Update(ctx, updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err = s.Get(ctx, "test-repo")
	if err != nil || got == nil || *got != updated {
		t.Fatalf("Get after Update = (%+v, %v), want %+v", got, err, updated)
	}

	if err := s.Delete(ctx, "test-repo"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, err := s.Get(ctx, "test-repo"); err != nil || got != nil {
		t.Fatalf("Get after Delete = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestStore_CreateDuplicateName(t *testing.T) {
	s := NewStore(newTestPool(t))
	ctx := context.Background()

	r := Repo{Name: "test-repo", URL: "https://example.com/test-repo.git"}
	if err := s.Create(ctx, r); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create(ctx, r); !errors.Is(err, ErrExists) {
		t.Fatalf("second Create error = %v, want ErrExists", err)
	}
}

func TestStore_UpdateDeleteUnknownName(t *testing.T) {
	s := NewStore(newTestPool(t))
	ctx := context.Background()

	if err := s.Update(ctx, Repo{Name: "nope", URL: "https://example.com/nope.git"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Update unknown error = %v, want pgx.ErrNoRows", err)
	}
	if err := s.Delete(ctx, "nope"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Delete unknown error = %v, want pgx.ErrNoRows", err)
	}
}

func TestStore_SetOnChange(t *testing.T) {
	s := NewStore(newTestPool(t))
	ctx := context.Background()

	calls := 0
	s.SetOnChange(func() { calls++ })

	r := Repo{Name: "test-repo", URL: "https://example.com/test-repo.git"}
	if err := s.Create(ctx, r); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Update(ctx, r); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := s.Delete(ctx, "test-repo"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if calls != 3 {
		t.Fatalf("onChange called %d times, want 3", calls)
	}
}

// The bug this prevents: an SSH URL is syntactically valid, stores fine,
// and only fails ten minutes later when a dispatched task tries to clone
// — surfacing as failed_permanently with "clone/fetch failed" and no
// obvious cause. Confirmed live 2026-08-11 with infra-bootstrap.
func TestCreate_RejectsSSHURL(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newTestPool(t))

	for _, url := range []string{
		"git@github.com:MohammadBnei/infra-bootstrap.git",
		"ssh://git@github.com/MohammadBnei/infra-bootstrap.git",
	} {
		err := s.Create(ctx, Repo{Name: "ssh-repo", URL: url, BaseBranch: "main"})
		if !errors.Is(err, ErrSSHURL) {
			t.Errorf("Create(%q) = %v, want ErrSSHURL — an SSH URL can never authenticate here", url, err)
		}
	}
}

func TestCreate_AcceptsHTTPSURL(t *testing.T) {
	ctx := context.Background()
	s := NewStore(newTestPool(t))

	if err := s.Create(ctx, Repo{
		Name: "https-repo", URL: "https://github.com/MohammadBnei/x.git", BaseBranch: "main",
	}); err != nil {
		t.Fatalf("Create with an https URL should succeed, got %v", err)
	}
}
