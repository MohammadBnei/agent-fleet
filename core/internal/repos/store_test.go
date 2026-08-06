//go:build integration

package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Real Postgres — mirrors tasks/store_test.go's container setup, a minimal
// subset of db/schema.sql's repos table.
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

	_, err = pool.Exec(ctx, `
		CREATE TABLE repos (
			name        TEXT PRIMARY KEY,
			url         TEXT NOT NULL,
			base_branch TEXT NOT NULL DEFAULT '',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`)
	if err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return pool
}

func TestStore_CreateGetListUpdateDelete(t *testing.T) {
	s := NewStore(newTestPool(t))
	ctx := context.Background()

	if got, err := s.Get(ctx, "dream-analyst"); err != nil || got != nil {
		t.Fatalf("Get before Create = (%v, %v), want (nil, nil)", got, err)
	}

	r := Repo{Name: "dream-analyst", URL: "https://example.com/dream-analyst.git", BaseBranch: "dev"}
	if err := s.Create(ctx, r); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "dream-analyst")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || *got != r {
		t.Fatalf("Get = %+v, want %+v", got, r)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0] != r {
		t.Fatalf("List = %+v, want [%+v]", list, r)
	}

	updated := Repo{Name: "dream-analyst", URL: "https://example.com/dream-analyst-v2.git", BaseBranch: "main"}
	if err := s.Update(ctx, updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err = s.Get(ctx, "dream-analyst")
	if err != nil || got == nil || *got != updated {
		t.Fatalf("Get after Update = (%+v, %v), want %+v", got, err, updated)
	}

	if err := s.Delete(ctx, "dream-analyst"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got, err := s.Get(ctx, "dream-analyst"); err != nil || got != nil {
		t.Fatalf("Get after Delete = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestStore_CreateDuplicateName(t *testing.T) {
	s := NewStore(newTestPool(t))
	ctx := context.Background()

	r := Repo{Name: "dream-analyst", URL: "https://example.com/dream-analyst.git"}
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

	r := Repo{Name: "dream-analyst", URL: "https://example.com/dream-analyst.git"}
	if err := s.Create(ctx, r); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Update(ctx, r); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := s.Delete(ctx, "dream-analyst"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if calls != 3 {
		t.Fatalf("onChange called %d times, want 3", calls)
	}
}
