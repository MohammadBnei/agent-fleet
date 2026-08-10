//go:build integration

package repoprofiles

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

// dbtest.NewPool applies the real db/migrations/ (docs/adr/0030), which
// seeds dream-analyst/"e2e", vos-monolith/"e2e", agent-fleet/"lint" rows
// (docs/adr/0034) — tests below use a fixture profile name outside that
// seeded set so they don't collide with it. repo_name must reference an
// existing repos row (FK) — "agent-fleet" is one of the three seeded repos.
const testRepo = "agent-fleet"

func newTestPool(t *testing.T) *pgxpool.Pool {
	return dbtest.NewPool(t)
}

func sortedCopy(s []string) []string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}

func TestStore_CreateGetListUpdateDelete(t *testing.T) {
	s := NewStore(newTestPool(t))
	ctx := context.Background()

	if got, err := s.Get(ctx, testRepo, "test-profile"); err != nil || got != nil {
		t.Fatalf("Get before Create = (%v, %v), want (nil, nil)", got, err)
	}

	p := Profile{
		RepoName: testRepo,
		Name:     "test-profile",
		StartCmd: "bun run dev",
		Tools:    []string{"go-toolchain", "buf"},
		Services: []ServiceIngredient{
			{Key: "postgres", ScopeMode: "task-scoped"},
			{Key: "redis", ScopeMode: "pod-scoped"},
		},
	}
	id, err := s.Create(ctx, p)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("Create returned empty id")
	}

	got, err := s.Get(ctx, testRepo, "test-profile")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get = nil, want profile")
	}
	if got.StartCmd != p.StartCmd {
		t.Errorf("StartCmd = %q, want %q", got.StartCmd, p.StartCmd)
	}
	if !reflect.DeepEqual(sortedCopy(got.Tools), sortedCopy(p.Tools)) {
		t.Errorf("Tools = %v, want %v", got.Tools, p.Tools)
	}
	if len(got.Services) != len(p.Services) {
		t.Errorf("Services = %+v, want %+v", got.Services, p.Services)
	}

	// List includes the migration's seeded profiles for testRepo (the
	// "lint" profile) alongside the one just created — assert containment.
	list, err := s.List(ctx, testRepo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, item := range list {
		if item.Name == "test-profile" {
			found = true
		}
	}
	if !found {
		t.Fatalf("List = %+v, want it to contain %q", list, "test-profile")
	}

	updated := Profile{
		RepoName: testRepo,
		Name:     "test-profile",
		StartCmd: "bun run build && bun run start",
		Tools:    []string{"bun-toolchain"},
		Services: []ServiceIngredient{{Key: "postgres", ScopeMode: "repo-scoped"}},
	}
	if err := s.Update(ctx, updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err = s.Get(ctx, testRepo, "test-profile")
	if err != nil || got == nil {
		t.Fatalf("Get after Update = (%+v, %v)", got, err)
	}
	if got.StartCmd != updated.StartCmd {
		t.Errorf("StartCmd after Update = %q, want %q", got.StartCmd, updated.StartCmd)
	}
	if !reflect.DeepEqual(got.Tools, updated.Tools) {
		t.Errorf("Tools after Update = %v, want %v", got.Tools, updated.Tools)
	}
	if !reflect.DeepEqual(got.Services, updated.Services) {
		t.Errorf("Services after Update = %+v, want %+v", got.Services, updated.Services)
	}

	if err := s.Delete(ctx, testRepo, "test-profile"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, err := s.Get(ctx, testRepo, "test-profile"); err != nil || got != nil {
		t.Fatalf("Get after Delete = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestStore_CreateDuplicateName(t *testing.T) {
	s := NewStore(newTestPool(t))
	ctx := context.Background()

	p := Profile{RepoName: testRepo, Name: "dup-profile"}
	if _, err := s.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(ctx, p); !errors.Is(err, ErrExists) {
		t.Fatalf("second Create error = %v, want ErrExists", err)
	}
}

func TestStore_UpdateDeleteUnknownName(t *testing.T) {
	s := NewStore(newTestPool(t))
	ctx := context.Background()

	if err := s.Update(ctx, Profile{RepoName: testRepo, Name: "nope"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Update unknown error = %v, want pgx.ErrNoRows", err)
	}
	if err := s.Delete(ctx, testRepo, "nope"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Delete unknown error = %v, want pgx.ErrNoRows", err)
	}
}

func TestStore_SetOnChange(t *testing.T) {
	s := NewStore(newTestPool(t))
	ctx := context.Background()

	calls := 0
	s.SetOnChange(func() { calls++ })

	p := Profile{RepoName: testRepo, Name: "onchange-profile"}
	if _, err := s.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Update(ctx, p); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := s.Delete(ctx, testRepo, "onchange-profile"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if calls != 3 {
		t.Fatalf("onChange called %d times, want 3", calls)
	}
}

func TestStore_CascadeDeleteOnRepoDeletion(t *testing.T) {
	pool := newTestPool(t)
	s := NewStore(pool)
	ctx := context.Background()

	// A throwaway repo (not one of the three seeded/FK-required ones) so
	// deleting it here doesn't disturb other tests sharing the seeded set.
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('cascade-test-repo', 'https://example.com/x.git')`); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	p := Profile{RepoName: "cascade-test-repo", Name: "e2e", Services: []ServiceIngredient{{Key: "postgres", ScopeMode: "task-scoped"}}}
	if _, err := s.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM repos WHERE name = 'cascade-test-repo'`); err != nil {
		t.Fatalf("delete repo: %v", err)
	}

	got, err := s.Get(ctx, "cascade-test-repo", "e2e")
	if err != nil || got != nil {
		t.Fatalf("Get after cascading repo delete = (%v, %v), want (nil, nil)", got, err)
	}
}
