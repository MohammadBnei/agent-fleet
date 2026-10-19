//go:build integration

package repos

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

// The migration seeds four rows with no github_id (dream-analyst,
// vos-monolith on base 'dev', agent-fleet, infra-bootstrap with cluster
// access): exactly the pre-0074 state the first sync must adopt in place.
var seeded = []GitHubRepo{
	{ID: 1, FullName: "MohammadBnei/dream-analyst", Name: "dream-analyst", CloneURL: "https://github.com/MohammadBnei/dream-analyst.git", DefaultBranch: "main"},
	{ID: 2, FullName: "MohammadBnei/vos-monolith", Name: "vos-monolith", CloneURL: "https://github.com/MohammadBnei/vos-monolith.git", DefaultBranch: "main"},
	{ID: 3, FullName: "MohammadBnei/agent-fleet", Name: "agent-fleet", CloneURL: "https://github.com/MohammadBnei/agent-fleet.git", DefaultBranch: "main"},
	{ID: 4, FullName: "MohammadBnei/infra-bootstrap", Name: "infra-bootstrap", CloneURL: "https://github.com/MohammadBnei/infra-bootstrap.git", DefaultBranch: "main"},
}

var seededOverrides = map[string]Override{
	"MohammadBnei/vos-monolith":    {ID: 2, BaseBranch: "dev"},
	"MohammadBnei/infra-bootstrap": {ID: 4, ClusterAccess: true},
}

func setup(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.NewPool(t)
	return NewStore(pool), pool
}

func runSync(t *testing.T, s *Store, gh []GitHubRepo, ov map[string]Override) int {
	t.Helper()
	n, err := s.Sync(context.Background(), append([]GitHubRepo(nil), gh...), ov)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func get(t *testing.T, s *Store, name string) *Repo {
	t.Helper()
	r, err := s.Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func exists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM repos WHERE name = $1)`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestSync_CutoverAdoptsRowsAndIsIdempotent(t *testing.T) {
	s, pool := setup(t)
	if n := runSync(t, s, seeded, seededOverrides); n != 4 {
		t.Fatalf("first sync changed %d rows, want 4", n)
	}
	if r := get(t, s, "infra-bootstrap"); r == nil || !r.ClusterAccess || r.FullName != "MohammadBnei/infra-bootstrap" {
		t.Fatalf("infra-bootstrap = %+v", r)
	}
	if r := get(t, s, "vos-monolith"); r == nil || r.BaseBranch != "dev" {
		t.Fatalf("vos-monolith = %+v", r)
	}
	if r := get(t, s, "dream-analyst"); r == nil || r.BaseBranch != "main" || r.ClusterAccess {
		t.Fatalf("dream-analyst = %+v", r)
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM repos WHERE github_id IS NOT NULL`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("github_id set on %d rows (%v), want 4: no row recreated", n, err)
	}
	if n := runSync(t, s, seeded, seededOverrides); n != 0 {
		t.Fatalf("second sync changed %d rows, want 0", n)
	}
}

// cluster_access comes only from the overrides file: dropping the entry
// revokes it, and a mismatched id grants nothing.
func TestSync_ClusterAccessOnlyFromAMatchingOverride(t *testing.T) {
	s, _ := setup(t)
	runSync(t, s, seeded, seededOverrides)
	runSync(t, s, seeded, map[string]Override{"MohammadBnei/infra-bootstrap": {ID: 99, ClusterAccess: true}})
	if r := get(t, s, "infra-bootstrap"); r.ClusterAccess {
		t.Fatal("an override with the wrong id granted cluster access")
	}
	runSync(t, s, seeded, seededOverrides)
	runSync(t, s, seeded, map[string]Override{})
	if r := get(t, s, "infra-bootstrap"); r.ClusterAccess {
		t.Fatal("an empty overrides file did not revoke cluster access")
	}
}

func TestSync_NewReposAndNameCollision(t *testing.T) {
	s, _ := setup(t)
	gh := append(append([]GitHubRepo(nil), seeded...),
		GitHubRepo{ID: 11, FullName: "opus-tech-hq/workspace", Name: "workspace", CloneURL: "https://github.com/opus-tech-hq/workspace.git", DefaultBranch: "main"},
		GitHubRepo{ID: 10, FullName: "MohammadBnei/workspace", Name: "workspace", CloneURL: "https://github.com/MohammadBnei/workspace.git", DefaultBranch: "trunk"},
	)
	runSync(t, s, gh, seededOverrides)
	if r := get(t, s, "workspace"); r == nil || r.FullName != "MohammadBnei/workspace" || r.BaseBranch != "trunk" {
		t.Fatalf("workspace = %+v, want the older repo (id 10)", r)
	}
	ov := map[string]Override{"opus-tech-hq/workspace": {ID: 11, Name: "opus-workspace"}}
	for k, v := range seededOverrides {
		ov[k] = v
	}
	runSync(t, s, gh, ov)
	if r := get(t, s, "opus-workspace"); r == nil || r.FullName != "opus-tech-hq/workspace" {
		t.Fatalf("opus-workspace = %+v", r)
	}
}

// A GitHub rename keeps the row and its fleet name (same id): only url and
// full_name move. Sessions and definitions reference the name.
func TestSync_RenameKeepsTheRowAndName(t *testing.T) {
	s, _ := setup(t)
	runSync(t, s, seeded, seededOverrides)
	gh := append([]GitHubRepo(nil), seeded...)
	gh[0] = GitHubRepo{ID: 1, FullName: "MohammadBnei/dreams", Name: "dreams", CloneURL: "https://github.com/MohammadBnei/dreams.git", DefaultBranch: "main"}
	runSync(t, s, gh, seededOverrides)
	if r := get(t, s, "dream-analyst"); r == nil || r.URL != "https://github.com/MohammadBnei/dreams.git" || r.FullName != "MohammadBnei/dreams" {
		t.Fatalf("dream-analyst = %+v", r)
	}
	if get(t, s, "dreams") != nil {
		t.Fatal("a GitHub rename renamed the fleet row")
	}
}

// Override keys match GitHub's owner/repo case-insensitively.
func TestSync_OverrideKeyCaseInsensitive(t *testing.T) {
	s, _ := setup(t)
	ov, err := ParseOverrides([]byte("repos:\n  mohammadbnei/INFRA-bootstrap: {id: 4, clusterAccess: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	runSync(t, s, seeded, ov)
	if r := get(t, s, "infra-bootstrap"); !r.ClusterAccess {
		t.Fatal("a differently-cased key did not apply")
	}
}

// A row no listed repo adopts loses a console-era grant all the same.
func TestSync_UnmatchedRowLosesClusterAccess(t *testing.T) {
	s, pool := setup(t)
	if _, err := pool.Exec(context.Background(), `INSERT INTO repos (name, url, cluster_access) VALUES ('stray', 'git@github.com:x/stray.git', true)`); err != nil {
		t.Fatal(err)
	}
	runSync(t, s, seeded, seededOverrides)
	var granted bool
	if err := pool.QueryRow(context.Background(), `SELECT cluster_access FROM repos WHERE name = 'stray'`).Scan(&granted); err != nil || granted {
		t.Fatalf("stray cluster_access = %v (%v), want false", granted, err)
	}
}

// A repo deleted and re-created at the same URL (new id) takes over its old
// row instead of being blocked by it.
func TestSync_RecreatedRepoAdoptsItsRow(t *testing.T) {
	s, _ := setup(t)
	runSync(t, s, seeded, seededOverrides)
	gh := append([]GitHubRepo(nil), seeded...)
	gh[0].ID = 100
	runSync(t, s, gh, seededOverrides)
	if r := get(t, s, "dream-analyst"); r == nil {
		t.Fatal("the re-created repo lost its row")
	}
}

func TestSync_GoneHidesThenDeletesAfterGrace(t *testing.T) {
	s, pool := setup(t)
	ctx := context.Background()
	runSync(t, s, seeded, seededOverrides)

	runSync(t, s, seeded[1:], seededOverrides) // the bot was removed from dream-analyst
	if r := get(t, s, "dream-analyst"); r != nil {
		t.Fatal("a gone repo is still visible to Get")
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range list {
		if r.Name == "dream-analyst" {
			t.Fatal("a gone repo is still listed")
		}
	}

	runSync(t, s, seeded, seededOverrides) // re-added within the grace
	if r := get(t, s, "dream-analyst"); r == nil {
		t.Fatal("re-adding the bot did not clear gone_since")
	}

	runSync(t, s, seeded[1:], seededOverrides)
	if _, err := pool.Exec(ctx, `UPDATE repos SET gone_since = now() - interval '8 days' WHERE name = 'dream-analyst'`); err != nil {
		t.Fatal(err)
	}
	var sid string
	if err := pool.QueryRow(ctx, `INSERT INTO sessions (repo) VALUES ('dream-analyst') RETURNING id`).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	runSync(t, s, seeded[1:], seededOverrides)
	if !exists(t, pool, "dream-analyst") {
		t.Fatal("deleted while an open session references it")
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, sid); err != nil {
		t.Fatal(err)
	}
	runSync(t, s, seeded[1:], seededOverrides)
	if exists(t, pool, "dream-analyst") {
		t.Fatal("not deleted after the grace with no open session")
	}
}

// A listing that lost more than maxGonePerTick repos is a token or GitHub
// problem until a human says otherwise: nothing is marked gone.
func TestSync_ShrinkGuard(t *testing.T) {
	s, pool := setup(t)
	runSync(t, s, seeded, seededOverrides)
	runSync(t, s, nil, seededOverrides)
	// An empty listing on a fleet of two is still the token, not two removals.
	if _, err := pool.Exec(context.Background(), `UPDATE repos SET gone_since = now() WHERE name IN ('agent-fleet', 'vos-monolith')`); err != nil {
		t.Fatal(err)
	}
	runSync(t, s, nil, seededOverrides)
	for _, name := range []string{"dream-analyst", "infra-bootstrap"} {
		if get(t, s, name) == nil {
			t.Fatalf("%s hidden by an empty listing", name)
		}
	}
}
