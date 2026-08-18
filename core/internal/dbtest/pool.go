//go:build integration

package dbtest

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// externalPool is the escape hatch for environments with no Docker socket —
// notably the fleet's own worker pods, where testcontainers cannot run at all
// and `request_service("postgres")` is the only way to get a database. Set
// AGENTFLEET_TEST_DSN to use it; CI leaves it unset and keeps using a
// container.
//
// The instance is SHARED (per repo, not per session), so every test gets its
// own schema and drops it again. Migrations still come from db/migrations/ —
// the point of docs/adr/0030 is that there is exactly one schema source, and
// an escape hatch that hand-rolled its own would defeat it.
//
// Single connections for the admin work and a capped pool for the test:
// pgxpool defaults MaxConns to the CPU count, and a shared server hands out a
// fixed max_connections — several pools at machine width is how this reports
// "connection reset by peer" rather than "too many clients".
func externalPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	// Suffix FIRST, name second: Postgres truncates identifiers at 63 bytes, so
	// appending the unique part lets the longest test names collide on a
	// deterministic schema — and this hatch exists precisely because the
	// instance is shared, where a collision is a hard 42P06 on the second run.
	schema := "test_" + strconv.FormatInt(time.Now().UnixNano()%1e9, 36) + "_" + strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '_'
		}
	}, t.Name())
	if len(schema) > 60 {
		schema = schema[:60]
	}

	exec := func(sql string) error {
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return err
		}
		defer conn.Close(ctx)
		_, err = conn.Exec(ctx, sql)
		return err
	}

	if err := exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if err := exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	scoped := dsn + sep + "search_path=" + schema

	m, err := migrate.New(migrationsURL(), scoped)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })

	cfg, err := pgxpool.ParseConfig(scoped)
	if err != nil {
		t.Fatalf("parse %s: %v", scoped, err)
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}
