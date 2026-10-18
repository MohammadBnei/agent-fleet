//go:build integration

package definitions

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

type fakeLister struct {
	files map[string][]byte
	sha   string
	err   error
}

func (f *fakeLister) ListDefinitionFiles(context.Context) (map[string][]byte, string, error) {
	return f.files, f.sha, f.err
}

func (f *fakeLister) set(sha string, files map[string]string) {
	f.sha, f.err = sha, nil
	f.files = map[string][]byte{}
	for p, c := range files {
		f.files[p] = []byte(c)
	}
}

func rowCount(t *testing.T, r *Registry, name string) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_definitions WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRegistry_Sync(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	lister := &fakeLister{}
	r := NewRegistry(pool, lister)

	if _, err := r.Resolve("good"); !errors.Is(err, ErrNotSynced) {
		t.Fatalf("before the first sync: want ErrNotSynced, got %v", err)
	}

	good := childYAML("good", nil)
	badMode := childYAML("bad", map[string]string{"mode": "{ default: auto, allow: [auto] }"})
	lister.set("sha1", map[string]string{
		"definitions/base.yaml": baseYAML,
		"definitions/good.yaml": good,
		"definitions/bad.yaml":  badMode,
		"definitions/oops.yml":  "x",
	})
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	t.Run("valid and invalid are both stored", func(t *testing.T) {
		h, err := r.Resolve("good")
		if err != nil || h.SHA != "sha1" || h.Effective.Mode.Default != "default" {
			t.Fatalf("good: %+v %v", h, err)
		}
		if _, err := r.Resolve("bad"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "skips canUseTool") {
			t.Fatalf("bad: %v", err)
		}
		var valid bool
		var msg *string
		if err := pool.QueryRow(ctx, `SELECT valid, error FROM agent_definitions WHERE name='bad'`).Scan(&valid, &msg); err != nil || valid || msg == nil || *msg == "" {
			t.Fatalf("the invalid row must be stored with its error: valid=%v err=%v %v", valid, msg, err)
		}
		if rowCount(t, r, "base") != 0 {
			t.Fatal("abstract definitions get no row")
		}
		heads, syncErrs := r.List()
		if len(heads) != 2 {
			t.Fatalf("heads: %+v", heads)
		}
		if len(syncErrs) != 1 || !strings.Contains(syncErrs[0], "oops.yml") {
			t.Fatalf("a bad path must surface in sync_errors: %v", syncErrs)
		}
	})

	t.Run("same content at a new sha adds no row", func(t *testing.T) {
		lister.sha = "sha2"
		if err := r.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if n := rowCount(t, r, "good"); n != 1 {
			t.Fatalf("rows: %d", n)
		}
		if h, _ := r.Resolve("good"); h.SHA != "sha1" {
			t.Fatalf("the pin names the commit where the content first appeared: %s", h.SHA)
		}
	})

	t.Run("changed content adds a row and becomes current", func(t *testing.T) {
		lister.set("sha3", map[string]string{
			"definitions/base.yaml": baseYAML,
			"definitions/good.yaml": childYAML("good", map[string]string{"mode": "{ default: plan, allow: [plan] }"}),
		})
		if err := r.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if h, _ := r.Resolve("good"); h.SHA != "sha3" {
			t.Fatalf("current: %s", h.SHA)
		}
		if _, err := r.Resolve("bad"); !errors.Is(err, ErrUnknown) {
			t.Fatalf("a file removed from git must be unknown: %v", err)
		}
		if rowCount(t, r, "bad") != 1 {
			t.Fatal("its row stays for pinned sessions")
		}
	})

	t.Run("latest valid: a typo keeps the previous version, a revert reuses its sha", func(t *testing.T) {
		lister.set("sha4", map[string]string{
			"definitions/base.yaml": baseYAML,
			"definitions/good.yaml": childYAML("good", map[string]string{"mode": "{ default: plan, allow: [plan, auto] }"}),
		})
		if err := r.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		h, err := r.Resolve("good")
		if err != nil || h.SHA != "sha3" || !strings.Contains(h.NewerErr, "skips canUseTool") {
			t.Fatalf("want sha3 with newer_error, got %+v %v", h, err)
		}
		lister.set("sha5", map[string]string{
			"definitions/base.yaml": baseYAML,
			"definitions/good.yaml": childYAML("good", nil),
		})
		if err := r.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if h, _ := r.Resolve("good"); h.SHA != "sha1" || h.NewerErr != "" {
			t.Fatalf("a revert reuses the old row: %+v", h)
		}
		if n := rowCount(t, r, "good"); n != 3 {
			t.Fatalf("rows: %d (sha1, sha3, sha4)", n)
		}
	})

	t.Run("a lister error keeps the last known good state", func(t *testing.T) {
		lister.err = errors.New("provisioner down")
		if err := r.Sync(ctx); err == nil {
			t.Fatal("want the error")
		}
		if h, err := r.Resolve("good"); err != nil || h.SHA != "sha1" {
			t.Fatalf("%+v %v", h, err)
		}
	})

	t.Run("re-judge revokes a stored row the ceiling now rejects", func(t *testing.T) {
		// A row an older, looser core stored as valid.
		if _, err := pool.Exec(ctx, `
			INSERT INTO agent_definitions (name, sha, spec, effective, valid, error, synced_at)
			SELECT 'good', 'sha0', spec,
			       jsonb_set(effective, '{mode}', '{"default":"auto","allow":["auto"]}'), true, NULL,
			       now() + interval '1 hour'
			  FROM agent_definitions WHERE name = 'good' AND sha = 'sha1'`); err != nil {
			t.Fatal(err)
		}
		lister.err = nil
		if err := r.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		var valid bool
		var msg *string
		if err := pool.QueryRow(ctx, `SELECT valid, error FROM agent_definitions WHERE name='good' AND sha='sha0'`).Scan(&valid, &msg); err != nil {
			t.Fatal(err)
		}
		if valid || msg == nil || !strings.Contains(*msg, "skips canUseTool") {
			t.Fatalf("want the row flipped invalid: valid=%v err=%v", valid, msg)
		}
		if h, _ := r.Resolve("good"); h.SHA != "sha1" {
			t.Fatalf("current falls back to the next valid row: %+v", h)
		}
	})
}
