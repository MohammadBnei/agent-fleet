package db

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// embedded_schema.sql is a build-time copy of the canonical db/schema.sql
// (go:embed can't reach outside its own package directory). CI diffs the
// two to catch drift — see .github/workflows/go.yml.
//
//go:embed embedded_schema.sql
var schemaSQL string

// ApplySchema runs the idempotent schema.sql against pool. Safe to re-run.
func ApplySchema(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}
