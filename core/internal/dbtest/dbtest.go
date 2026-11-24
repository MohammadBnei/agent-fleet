//go:build integration

// Package dbtest spins up a real Postgres via testcontainers and applies
// the actual db/migrations/ directory via golang-migrate — every
// integration test in core shares this instead of hand-rolling its own
// partial-schema fixture. Fixes the repeated schema-drift failure mode
// documented in docs/adr/0030: a hand-rolled test schema can silently
// diverge from the real one, so its passing tells you nothing about
// whether the real schema still works.
package dbtest

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// migrationsURL resolves db/migrations/ relative to this source file
// rather than the caller's working directory, so every package under
// core/internal/... can call NewPool regardless of its own depth in the
// module tree.
func migrationsURL() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return "file://" + filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "db", "migrations")
}

// NewPool starts a real postgres:18 container, applies every migration in
// db/migrations/ via golang-migrate, and returns a ready pgxpool.Pool.
// Container and pool are torn down automatically via t.Cleanup.
func NewPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	// No Docker socket (the fleet's own worker pods) — see externalPool.
	if dsn := os.Getenv("AGENTFLEET_TEST_DSN"); dsn != "" {
		return externalPool(t, dsn)
	}
	connStr := containerDSN(t)
	m, err := migrate.New(migrationsURL(), connStr)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	return scopedPool(t, connStr, 0)
}

// NewMigrated is for tests of the migrations themselves: a migrator at version 'to' and a pool on the
// same database. With AGENTFLEET_TEST_DSN it works in a schema of that server (see externalSchemaDSN),
// else in a testcontainer. The pool is wide enough for a handful of concurrent writers.
func NewMigrated(t *testing.T, to uint) (*migrate.Migrate, *pgxpool.Pool) {
	t.Helper()
	var dsn string
	if env := os.Getenv("AGENTFLEET_TEST_DSN"); env != "" {
		dsn = externalSchemaDSN(t, env)
	} else {
		dsn = containerDSN(t)
	}
	m, err := migrate.New(migrationsURL(), dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	if err := m.Migrate(to); err != nil {
		t.Fatalf("migrate to %d: %v", to, err)
	}
	return m, scopedPool(t, dsn, 12)
}

// containerDSN starts a postgres:18 container, removed at cleanup.
func containerDSN(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:18",
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
	return connStr
}
