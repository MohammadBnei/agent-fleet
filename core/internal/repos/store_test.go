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

// active is the console's to write: SetActive touches only listed, non-gone
// rows, a sync tick never resets it, and List/Get still return inactive rows.
func TestSetActive(t *testing.T) {
	s, pool := setup(t)
	ctx := context.Background()
	runSync(t, s, seeded, seededOverrides)
	if r := get(t, s, "dream-analyst"); !r.Active || r.CreatedAt.IsZero() {
		t.Fatalf("a synced repo = %+v, want active with created_at", r)
	}
	if _, err := pool.Exec(ctx, `UPDATE repos SET gone_since = now() WHERE name = 'agent-fleet'`); err != nil {
		t.Fatal(err)
	}
	n, err := s.SetActive(ctx, []string{"dream-analyst", "vos-monolith", "agent-fleet", "nope"}, false)
	if err != nil || n != 2 {
		t.Fatalf("SetActive = %d (%v), want 2: gone and unknown names are skipped", n, err)
	}
	if n, err := s.SetActive(ctx, []string{"dream-analyst", "infra-bootstrap"}, false); err != nil || n != 1 {
		t.Fatalf("SetActive = %d (%v), want 1: a row already inactive is not a change", n, err)
	}
	var goneActive bool
	if err := pool.QueryRow(ctx, `SELECT active FROM repos WHERE name = 'agent-fleet'`).Scan(&goneActive); err != nil || !goneActive {
		t.Fatalf("gone row active = %v (%v), want untouched", goneActive, err)
	}

	runSync(t, s, seeded, seededOverrides)
	if r := get(t, s, "dream-analyst"); r == nil || r.Active {
		t.Fatalf("dream-analyst after sync = %+v, want still inactive", r)
	}
	if r := get(t, s, "agent-fleet"); !r.Active {
		t.Fatal("a gone repo was deactivated")
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inactive := 0
	for _, r := range list {
		if !r.Active {
			inactive++
		}
	}
	if inactive != 3 {
		t.Fatalf("List carries %d inactive rows, want 3", inactive)
	}
}

func TestInactive(t *testing.T) {
	s, _ := setup(t)
	ctx := context.Background()
	runSync(t, s, seeded, seededOverrides)
	if _, err := s.SetActive(ctx, []string{"dream-analyst"}, false); err != nil {
		t.Fatal(err)
	}
	if !s.Inactive(ctx, "dream-analyst") || s.Inactive(ctx, "agent-fleet") || s.Inactive(ctx, "nope") {
		t.Fatal("Inactive must be true only for a known, switched-off repo")
	}
}

// FORGE=github to forgejo: the listing's ids and host are all new, so each
// row is carried by owner/repo (its name, active and sessions stay), a
// Forgejo id equal to a GitHub row's (ukubi-ci's 1, dream-analyst's) neither
// steals that row nor trips github_id's UNIQUE, and the repos left on GitHub
// are hidden even past the shrink guard's cap.
func TestSync_ForgeSwitchCarriesRowsByOwnerRepo(t *testing.T) {
	s, pool := setup(t)
	ctx := context.Background()
	gh := append([]GitHubRepo(nil), seeded...)
	for i, n := range []string{"a", "b", "c"} {
		gh = append(gh, GitHubRepo{ID: int64(20 + i), FullName: "opus-tech-hq/" + n, Name: n, CloneURL: "https://github.com/opus-tech-hq/" + n + ".git", DefaultBranch: "main"})
	}
	runSync(t, s, gh, seededOverrides)
	if _, err := s.SetActive(ctx, []string{"dream-analyst"}, false); err != nil {
		t.Fatal(err)
	}
	// A host-less row holding Forgejo dream-analyst's id must not take it.
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url, github_id) VALUES ('local', '/tmp/origin', 15)`); err != nil {
		t.Fatal(err)
	}
	// An scp-form remote is carried like an https one.
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('wird', 'git@github.com:MohammadBnei/wird.git')`); err != nil {
		t.Fatal(err)
	}

	fj := func(id int64, name, branch string) GitHubRepo {
		return GitHubRepo{ID: id, FullName: "MohammadBnei/" + name, Name: name, CloneURL: "https://git.bnei.dev/MohammadBnei/" + name + ".git", DefaultBranch: branch}
	}
	list := []GitHubRepo{fj(1, "ukubi-ci", "main"), fj(7, "wird", "main"), fj(15, "dream-analyst", "main"), fj(16, "vos-monolith", "main"), fj(18, "agent-fleet", "main"), fj(19, "infra-bootstrap", "main")}
	ov := map[string]Override{"MohammadBnei/vos-monolith": {ID: 16, BaseBranch: "dev"}, "MohammadBnei/infra-bootstrap": {ID: 19, ClusterAccess: true}}
	runSync(t, s, list, ov)

	for _, g := range list {
		r := get(t, s, g.Name)
		if r == nil || r.URL != g.CloneURL || r.FullName != g.FullName {
			t.Fatalf("%s = %+v, want it on %s", g.Name, r, g.CloneURL)
		}
	}
	if r := get(t, s, "dream-analyst"); r.Active {
		t.Fatal("the carried row lost its console state (active)")
	}
	if r := get(t, s, "vos-monolith"); r.BaseBranch != "dev" {
		t.Fatalf("vos-monolith base = %q, want the override's dev", r.BaseBranch)
	}
	if r := get(t, s, "infra-bootstrap"); !r.ClusterAccess {
		t.Fatal("infra-bootstrap lost cluster access under a matching Forgejo-id override")
	}
	var id int64
	if err := pool.QueryRow(ctx, `SELECT github_id FROM repos WHERE name = 'dream-analyst'`).Scan(&id); err != nil || id != 15 {
		t.Fatalf("dream-analyst id = %d (%v), want Forgejo's 15", id, err)
	}
	for _, n := range []string{"a", "b", "c"} {
		if get(t, s, n) != nil {
			t.Fatalf("%s, left on GitHub, is still visible", n)
		}
	}
	if n := runSync(t, s, list, ov); n != 0 {
		t.Fatalf("second Forgejo sync changed %d rows, want 0", n)
	}
}

func forgejoOf(gh []GitHubRepo) []GitHubRepo {
	out := make([]GitHubRepo, len(gh))
	for i, g := range gh {
		out[i] = GitHubRepo{ID: int64(100 + i), FullName: g.FullName, Name: g.Name, CloneURL: "https://git.bnei.dev/" + g.FullName + ".git", DefaultBranch: "main"}
	}
	return out
}

// A switch tick whose listing is short (one repo: ListRepos drops pushable,
// archived, fork and mirror repos without a word) hides none of the rows its
// owner still has: they count against the shrink guard like any removal.
func TestSync_ForgeSwitchShortListingHidesNothing(t *testing.T) {
	s, _ := setup(t)
	runSync(t, s, seeded, seededOverrides)
	fj := forgejoOf(seeded)
	runSync(t, s, fj[:1], nil)
	if r := get(t, s, fj[0].Name); r == nil || r.URL != fj[0].CloneURL {
		t.Fatalf("%s = %+v, want it carried", fj[0].Name, r)
	}
	for _, g := range seeded[1:] {
		if r := get(t, s, g.Name); r == nil || r.URL != g.CloneURL {
			t.Fatalf("%s = %+v, want it still visible on GitHub", g.Name, r)
		}
	}
}

// Under the cap, an owner's unlisted rows are hidden like any removal, and
// the next full listing carries them back by owner/repo (gone rows too),
// inside the 7-day grace; the tick after changes nothing.
func TestSync_ForgeSwitchShortListingRecovers(t *testing.T) {
	s, _ := setup(t)
	runSync(t, s, seeded, seededOverrides)
	fj := forgejoOf(seeded)
	runSync(t, s, fj[:2], nil)
	if get(t, s, fj[2].Name) != nil {
		t.Fatal("a repo the short listing left out is still visible under the cap")
	}
	runSync(t, s, fj, nil)
	for _, g := range fj {
		if r := get(t, s, g.Name); r == nil || r.URL != g.CloneURL {
			t.Fatalf("%s = %+v after the full listing, want it back on %s", g.Name, r, g.CloneURL)
		}
	}
	if n := runSync(t, s, fj, nil); n != 0 {
		t.Fatalf("the tick after recovery changed %d rows, want 0", n)
	}
}

// Under FORGE=github, a row on an odd host (www.github.com, GHE) of a listed
// owner is still under the shrink guard: three at once hide nothing.
func TestSync_OddHostRowsStayUnderTheGuard(t *testing.T) {
	s, pool := setup(t)
	for _, n := range []string{"odd1", "odd2", "odd3"} {
		if _, err := pool.Exec(context.Background(), `INSERT INTO repos (name, url) VALUES ($1, $2)`, n, "https://www.github.com/MohammadBnei/"+n+".git"); err != nil {
			t.Fatal(err)
		}
	}
	runSync(t, s, seeded, seededOverrides)
	for _, n := range []string{"odd1", "odd2", "odd3"} {
		if get(t, s, n) == nil {
			t.Fatalf("%s hidden outside the shrink guard", n)
		}
	}
}

// Identity across a switch is owner/repo, nothing less: another owner's repo
// of the same name gets a row of its own and never the old one, and the same
// owner/repo carries the row with its fleet name even with no override.
func TestSync_ForgeSwitchCarriesOnlyTheSameOwnerRepo(t *testing.T) {
	s, _ := setup(t)
	opus := GitHubRepo{ID: 11, FullName: "opus-tech-hq/workspace", Name: "workspace", CloneURL: "https://github.com/opus-tech-hq/workspace.git", DefaultBranch: "main"}
	ov := map[string]Override{"opus-tech-hq/workspace": {ID: 11, Name: "opus-workspace"}}
	runSync(t, s, append(append([]GitHubRepo(nil), seeded...), opus), ov)
	fj := forgejoOf(seeded)
	other := GitHubRepo{ID: 50, FullName: "MohammadBnei/workspace", Name: "workspace", CloneURL: "https://git.bnei.dev/MohammadBnei/workspace.git", DefaultBranch: "main"}
	runSync(t, s, append(append([]GitHubRepo(nil), fj...), other), nil)
	if r := get(t, s, "workspace"); r == nil || r.FullName != other.FullName {
		t.Fatalf("workspace = %+v, want another owner's repo on a row of its own", r)
	}
	if get(t, s, "opus-workspace") != nil {
		t.Fatal("opus-workspace, its owner absent from the listing, is still visible")
	}
	moved := GitHubRepo{ID: 51, FullName: "opus-tech-hq/workspace", Name: "workspace", CloneURL: "https://git.bnei.dev/opus-tech-hq/workspace.git", DefaultBranch: "main"}
	runSync(t, s, append(append([]GitHubRepo(nil), fj...), other, moved), nil)
	if r := get(t, s, "opus-workspace"); r == nil || r.URL != moved.CloneURL {
		t.Fatalf("opus-workspace = %+v, want it carried to %s under its fleet name", r, moved.CloneURL)
	}
}

// The never-migrated backlog (155 active rows of a listed owner left on
// GitHub) stays visible above its own cap and never holds the shrink guard:
// a real removal on Forgejo still goes through.
func TestSync_ForeignBacklogDoesNotBlockNativeRemovals(t *testing.T) {
	s, pool := setup(t)
	ctx := context.Background()
	runSync(t, s, seeded, seededOverrides)
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) SELECT 'b' || i, 'https://github.com/MohammadBnei/b' || i || '.git' FROM generate_series(1, 155) i`); err != nil {
		t.Fatal(err)
	}
	fj := forgejoOf(seeded)
	runSync(t, s, fj, nil)
	runSync(t, s, fj[1:], nil)
	if get(t, s, fj[0].Name) != nil {
		t.Fatal("a Forgejo removal was blocked by the GitHub backlog")
	}
	for _, n := range []string{"b1", "b155"} {
		if get(t, s, n) == nil {
			t.Fatalf("%s, active and above the cap, was hidden", n)
		}
	}
}

// An idle row of the previous forge (inactive, no open session) is hidden
// outside any cap; an inactive one an open session references is busy.
func TestSync_IdleForeignRowsHiddenBusyOnesKept(t *testing.T) {
	s, pool := setup(t)
	ctx := context.Background()
	runSync(t, s, seeded, seededOverrides)
	for _, n := range []string{"idle1", "idle2", "idle3", "used1", "used2", "used3"} {
		if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url, active) VALUES ($1, $2, false)`, n, "https://github.com/MohammadBnei/"+n+".git"); err != nil {
			t.Fatal(err)
		}
		if n[:4] == "used" {
			if _, err := pool.Exec(ctx, `INSERT INTO sessions (repo) VALUES ($1)`, n); err != nil {
				t.Fatal(err)
			}
		}
	}
	runSync(t, s, forgejoOf(seeded), nil)
	for _, n := range []string{"idle1", "idle2", "idle3"} {
		if get(t, s, n) != nil {
			t.Fatalf("%s, idle on the previous forge, is still visible", n)
		}
	}
	for _, n := range []string{"used1", "used2", "used3"} {
		if get(t, s, n) == nil {
			t.Fatalf("%s, with an open session and above the cap, was hidden", n)
		}
	}
}

// A GitHub listing marks no row foreign: an odd-host row of an owner the
// listing lacks stays under the shrink guard, and byID still matches.
func TestSync_GitHubListingMarksNoRowForeign(t *testing.T) {
	s, pool := setup(t)
	ctx := context.Background()
	for _, n := range []string{"ghe1", "ghe2", "ghe3"} {
		if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ($1, $2)`, n, "https://ghe.example/other/"+n+".git"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url, github_id) VALUES ('moved', 'https://ghe.example/someone/moved.git', 77)`); err != nil {
		t.Fatal(err)
	}
	gh := append(append([]GitHubRepo(nil), seeded...), GitHubRepo{ID: 77, FullName: "someone/renamed", Name: "renamed", CloneURL: "https://github.com/someone/renamed.git", DefaultBranch: "main"})
	runSync(t, s, gh, seededOverrides)
	if r := get(t, s, "moved"); r == nil || r.URL != "https://github.com/someone/renamed.git" {
		t.Fatalf("moved = %+v, want its row matched by id", r)
	}
	for _, n := range []string{"ghe1", "ghe2", "ghe3"} {
		if get(t, s, n) == nil {
			t.Fatalf("%s hidden outside the shrink guard under a GitHub listing", n)
		}
	}
}
