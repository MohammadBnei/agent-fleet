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
	r, err := s.RenderContext(ctx, KindRepo, "wedding-2026", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Files) != 1 || r.Files[0].Body != "v2" || r.Versions[0] != "style@2" {
		t.Fatalf("RenderContext = %+v, want style@2", r)
	}

	// A session still on the old name renders without the repo's packs.
	r, err = s.RenderContext(ctx, KindRepo, "wedding", nil, nil)
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
	r, _ = s.RenderContext(ctx, KindDefinition, "dev-notes", nil, nil)
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
