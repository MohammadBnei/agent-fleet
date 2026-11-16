//go:build integration

package definitions

import (
	"context"
	"strings"
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
		"definitions/aaa.yaml":  childYAML("aaa", nil),
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
	// Rollout: the same definition at a new sha is a new row with the
	// addition; the pinned sha1 row keeps its effective.
	r.lister.(*fakeLister).set("sha2", map[string]string{
		"definitions/base.yaml": baseYAML,
		"definitions/aaa.yaml":  childYAML("aaa", nil),
	})
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var old, fresh string
	if err := r.pool.QueryRow(ctx, `SELECT effective->>'sidecarTools' FROM agent_definitions WHERE name = 'aaa' AND sha = 'sha1'`).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if err := r.pool.QueryRow(ctx, `SELECT effective->>'sidecarTools' FROM agent_definitions WHERE name = 'aaa' AND sha = 'sha2'`).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, r, "aaa"); n != 2 || old != `["draft_card"]` || !strings.Contains(fresh, "AskUserQuestion") {
		t.Fatalf("rows=%d old=%s new=%s", n, old, fresh)
	}
}

// docs/adr/0070 slice 12: a row an older core wrote still holds interaction.
// It is still judged under this core's ceiling: revoked when outside it,
// valid when inside, never left alone as a newer core's row.
func TestRegistry_RetiredInteractionKeyStillJudged(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(dbtest.NewPool(t), &fakeLister{})
	r.lister.(*fakeLister).set("sha1", map[string]string{
		"definitions/base.yaml": baseYAML,
		"definitions/aaa.yaml":  childYAML("aaa", nil),
	})
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `
		INSERT INTO agent_definitions (name, sha, spec, effective, valid)
		SELECT name, 'old-in', spec, effective || '{"interaction":{"allow":["ask"]}}', true
		  FROM agent_definitions WHERE name = 'aaa' AND sha = 'sha1'
		UNION ALL
		SELECT name, 'old-out', spec, jsonb_set(effective || '{"interaction":{"allow":["ask"]}}',
		       '{mode}', '{"default":"bypassPermissions","allow":["bypassPermissions"]}'), true
		  FROM agent_definitions WHERE name = 'aaa' AND sha = 'sha1'`); err != nil {
		t.Fatal(err)
	}
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for sha, want := range map[string]bool{"old-in": true, "old-out": false} {
		var valid bool
		var msg *string
		if err := r.pool.QueryRow(ctx, `SELECT valid, error FROM agent_definitions WHERE name = 'aaa' AND sha = $1`, sha).Scan(&valid, &msg); err != nil {
			t.Fatal(err)
		}
		if valid != want || (!want && (msg == nil || !strings.Contains(*msg, "revoked by the current ceiling"))) {
			t.Fatalf("%s: valid=%v err=%v, want valid=%v", sha, valid, msg, want)
		}
	}
}
