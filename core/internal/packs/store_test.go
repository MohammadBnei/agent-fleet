//go:build integration

package packs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

func TestStore_VersionsAttachRender(t *testing.T) {
	pool := dbtest.NewPool(t)
	s := NewStore(pool)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url, github_id) VALUES ('wedding', 'https://github.com/o/wedding-2026', 42), ('legacy', 'https://github.com/o/legacy', NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, effective, valid) VALUES ('dev-notes', 'abc', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, "style", "platform-admins"); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, "repo", "platform-admins"); err == nil {
		t.Fatal("Create took the reserved name repo")
	}
	if err := s.Create(ctx, "style", "platform-admins"); !errors.Is(err, ErrExists) {
		t.Fatalf("second Create = %v, want ErrExists", err)
	}
	for _, text := range []string{"v1", "v2"} {
		if _, err := s.SaveVersion(ctx, Version{Pack: "style", Text: text}, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SaveVersion(ctx, Version{Pack: "style", Text: "@/proc/self/environ"}, "admin"); err == nil {
		t.Fatal("SaveVersion accepted an @ import")
	}
	if _, err := s.SaveVersion(ctx, Version{Pack: "nope"}, "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SaveVersion unknown pack = %v, want ErrNotFound", err)
	}

	if err := s.Attach(ctx, KindRepo, "wedding", "style", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach(ctx, KindRepo, "legacy", "style", "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Attach to a repo without github_id = %v, want ErrNotFound", err)
	}

	// The attachment follows the GitHub id through an override rename.
	if _, err := pool.Exec(ctx, `UPDATE repos SET name = 'wedding-2026' WHERE name = 'wedding'`); err != nil {
		t.Fatal(err)
	}
	r, err := s.RenderContext(ctx, KindRepo, "wedding-2026")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Files) != 1 || r.Files[0].Body != "v2" || r.Versions[0] != "style@2" {
		t.Fatalf("RenderContext = %+v, want style@2", r)
	}

	// A session still on the old name renders without the repo's packs.
	r, err = s.RenderContext(ctx, KindRepo, "wedding")
	if err != nil || len(r.Files) != 0 || len(r.Skipped) != 1 {
		t.Fatalf("RenderContext old name = %+v, %v", r, err)
	}

	// Attaching past the composed cap is refused, and leaves nothing behind.
	if err := s.Create(ctx, "big", "platform-admins"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"big", "style"} {
		if _, err := s.SaveVersion(ctx, Version{Pack: p, Text: strings.Repeat("x", MaxText/2+1)}, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Attach(ctx, KindDefinition, "dev-notes", "style", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach(ctx, KindDefinition, "dev-notes", "big", "admin"); err == nil {
		t.Fatal("Attach over the cap succeeded")
	}
	if err := s.Attach(ctx, KindDefinition, "dev-nots", "style", "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Attach to an unknown definition = %v, want ErrNotFound", err)
	}
	// A bigger version of an attached pack is refused when it overflows a target.
	if err := s.Detach(ctx, KindRepo, "wedding-2026", "style"); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach(ctx, KindRepo, "wedding-2026", "big", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, "tiny", "platform-admins"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveVersion(ctx, Version{Pack: "tiny", Text: "t"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach(ctx, KindRepo, "wedding-2026", "tiny", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveVersion(ctx, Version{Pack: "big", Text: strings.Repeat("y", MaxText)}, "admin"); err == nil {
		t.Fatal("SaveVersion pushed an attached target over the cap")
	}
	if err := s.Detach(ctx, KindRepo, "wedding-2026", "big"); err != nil {
		t.Fatal(err)
	}
	r, _ = s.RenderContext(ctx, KindDefinition, "dev-notes")
	if len(r.Versions) != 1 {
		t.Fatalf("after a refused Attach, Versions = %v", r.Versions)
	}

	if err := s.Detach(ctx, KindDefinition, "dev-notes", "style"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "style"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "big" || list[0].Latest != 1 {
		t.Fatalf("List = %+v, %v", list, err)
	}
}

func TestStore_ReviewedPacks(t *testing.T) {
	pool := dbtest.NewPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url, github_id) VALUES ('wedding', 'https://github.com/o/wedding-2026', 42)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, effective, valid) VALUES ('dev-notes', 'abc', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	paths := func(r Rendered) string {
		var p []string
		for _, f := range r.Files {
			p = append(p, f.Path)
		}
		return strings.Join(p, ",")
	}

	files := map[string][]byte{
		"definitions/a.yaml":  []byte("a"),
		listedMarker:          nil,
		"packs/fleet/PACK.md": []byte("fleet rules"),
		"packs/repo/PACK.md":  []byte("repo rules"),
	}
	if bad := s.SyncReviewed(ctx, files, "1111111aaaa"); len(bad) != 0 {
		t.Fatal(bad)
	}
	r, err := s.RenderContext(ctx, KindRepo, "wedding")
	if err != nil || paths(r) != ".claude/rules/80-repo.md,.claude/rules/90-fleet.md" {
		t.Fatalf("repo render = %v, %v", paths(r), err)
	}
	if got := strings.Join(r.Versions, ","); got != "reviewed:fleet@1111111,reviewed:repo@1111111" {
		t.Fatalf("Versions = %s", got)
	}
	r, err = s.RenderContext(ctx, KindDefinition, "dev-notes")
	if err != nil || paths(r) != ".claude/rules/90-fleet.md" {
		t.Fatalf("definition render = %v, %v (no repo pack on a definition)", paths(r), err)
	}

	// An unchanged pack keeps its sha across a new release commit.
	if bad := s.SyncReviewed(ctx, files, "2222222bbbb"); len(bad) != 0 {
		t.Fatal(bad)
	}
	if r, _ = s.RenderContext(ctx, KindRepo, "wedding"); strings.Join(r.Versions, ",") != "reviewed:fleet@1111111,reviewed:repo@1111111" {
		t.Fatalf("an unchanged pack moved: %v", r.Versions)
	}

	// No marker (an older provisioner): nothing changes.
	if bad := s.SyncReviewed(ctx, map[string][]byte{"definitions/a.yaml": []byte("a")}, "3333333cccc"); len(bad) != 0 {
		t.Fatal(bad)
	}
	if r, _ = s.RenderContext(ctx, KindRepo, "wedding"); len(r.Files) != 2 {
		t.Fatalf("an older provisioner's listing dropped the reviewed packs: %v", paths(r))
	}

	// A broken fleet pack keeps its last good content and says why; a pack
	// gone from a marked listing is removed.
	broken := map[string][]byte{listedMarker: nil, "packs/fleet/PACK.md": []byte("new"), "packs/fleet/run.sh": []byte("x")}
	if bad := s.SyncReviewed(ctx, broken, "4444444dddd"); len(bad) != 1 {
		t.Fatalf("bad = %v", bad)
	}
	r, _ = s.RenderContext(ctx, KindRepo, "wedding")
	if paths(r) != ".claude/rules/90-fleet.md" || r.Files[0].Body != "fleet rules" || len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0], "run.sh") {
		t.Fatalf("after a broken sync: %v %+v %v", paths(r), r.Files, r.Skipped)
	}
	// Past staleAfter the note says stale as well.
	if _, err := pool.Exec(ctx, `UPDATE reviewed_packs SET synced_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if r, _ = s.RenderContext(ctx, KindDefinition, "dev-notes"); len(r.Skipped) != 2 || !strings.Contains(r.Skipped[1], "stale") {
		t.Fatalf("Skipped = %v", r.Skipped)
	}

	// The cap check at attach counts the reviewed bytes too.
	if _, err := pool.Exec(ctx, `UPDATE reviewed_packs SET text = repeat('f', $1) WHERE name = 'fleet'`, MaxText-10); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, "style", "platform-admins"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveVersion(ctx, Version{Pack: "style", Text: strings.Repeat("s", 20)}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach(ctx, KindDefinition, "dev-notes", "style", "admin"); err == nil {
		t.Fatal("Attach ignored the reviewed fleet pack's bytes")
	}
	if err := s.Create(ctx, "fleet", "platform-admins"); err == nil {
		t.Fatal("Create took a reviewed name")
	}
}
