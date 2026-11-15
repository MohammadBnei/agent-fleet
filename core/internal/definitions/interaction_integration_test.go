//go:build integration

package definitions

import (
	"context"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

// docs/adr/0070 §4: a valid (name, sha) row's effective is frozen, so the
// loader's AskUserQuestion addition never rewrites a row a live pod pinned.
func TestRegistry_FrozenEffectiveSurvivesLoaderAddition(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(dbtest.NewPool(t), &fakeLister{})
	r.lister.(*fakeLister).set("sha1", map[string]string{
		"definitions/base.yaml": baseYAML,
		"definitions/aaa.yaml":    childYAML("aaa", nil),
	})
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	// As an older core wrote it: no AskUserQuestion.
	if _, err := r.pool.Exec(ctx, `UPDATE agent_definitions SET effective = jsonb_set(effective, '{sidecarTools}', '["draft_card"]') WHERE name = 'aaa'`); err != nil {
		t.Fatal(err)
	}
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var tools string
	if err := r.pool.QueryRow(ctx, `SELECT effective->>'sidecarTools' FROM agent_definitions WHERE name = 'aaa' AND sha = 'sha1'`).Scan(&tools); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, r, "aaa"); n != 1 || tools != `["draft_card"]` {
		t.Fatalf("rows=%d tools=%s", n, tools)
	}
}
