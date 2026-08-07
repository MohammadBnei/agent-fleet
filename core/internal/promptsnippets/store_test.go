//go:build integration

package promptsnippets

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

// Real Postgres — mirrors repos/store_test.go's container setup, a minimal
// subset of db/schema.sql's prompt_snippets table.
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
		CREATE TABLE prompt_snippets (
			id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			name       TEXT NOT NULL UNIQUE,
			text       TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
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

	list, err := s.List(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("List before Create = (%v, %v), want ([], nil)", list, err)
	}

	sn, err := s.Create(ctx, Snippet{Name: "Open a PR when done", Text: "push and open a PR"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sn.ID == "" {
		t.Fatalf("Create returned empty ID")
	}

	list, err = s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0] != sn {
		t.Fatalf("List = %+v, want [%+v]", list, sn)
	}

	got, err := s.GetByIDs(ctx, []string{sn.ID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(got) != 1 || got[0] != sn {
		t.Fatalf("GetByIDs = %+v, want [%+v]", got, sn)
	}

	updated := Snippet{ID: sn.ID, Name: "Open a PR when done (v2)", Text: "push, then gh pr create"}
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
	if err != nil || len(list) != 0 {
		t.Fatalf("List after Delete = (%v, %v), want ([], nil)", list, err)
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
