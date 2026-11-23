//go:build integration

package sessions

import (
	"context"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

func TestSetModelWritesRepoSessionsOnly(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('x', 'abc', '{}', '{}'::jsonb, true)`); err != nil {
		t.Fatal(err)
	}
	var repo, def string
	if err := pool.QueryRow(ctx, `INSERT INTO sessions (repo, description, model) VALUES ('r', 'd', 'claude-opus-5') RETURNING id::text`).Scan(&repo); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO sessions (repo, description, definition_name, definition_sha) VALUES ('r', 'd', 'x', 'abc') RETURNING id::text`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.SetModel(ctx, repo, "claude-sonnet-5"); err != nil || !ok {
		t.Fatalf("repo session: ok=%v err=%v", ok, err)
	}
	if ok, err := store.SetModel(ctx, def, "claude-sonnet-5"); err != nil || ok {
		t.Fatalf("definition session: ok=%v err=%v, want untouched", ok, err)
	}
	var m string
	if err := pool.QueryRow(ctx, `SELECT model FROM sessions WHERE id = $1`, repo).Scan(&m); err != nil || m != "claude-sonnet-5" {
		t.Fatalf("model = %q (%v)", m, err)
	}
}
