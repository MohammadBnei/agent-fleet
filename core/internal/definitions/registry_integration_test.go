//go:build integration

package definitions

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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

	if _, err := r.Resolve("good"); !errors.Is(err, errNotSynced) {
		t.Fatalf("before the first sync: want errNotSynced, got %v", err)
	}
	if _, syncErrs := r.List(); len(syncErrs) != 1 || !strings.Contains(syncErrs[0], "not synced yet") {
		t.Fatalf("an unsynced list must say so: %v", syncErrs)
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

	// An invalid HEAD never falls back to an older version: that version may
	// be the wider one a narrowing edit (with a mistake in it) replaced.
	t.Run("an invalid HEAD fails closed, a revert reuses its sha", func(t *testing.T) {
		lister.set("sha4", map[string]string{
			"definitions/base.yaml": baseYAML,
			"definitions/good.yaml": childYAML("good", map[string]string{"mode": "{ default: plan, allow: [plan, auto] }"}),
		})
		if err := r.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if h, err := r.Resolve("good"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "skips canUseTool") {
			t.Fatalf("want ErrInvalid, never the older sha3: %+v %v", h, err)
		}
		lister.set("sha5", map[string]string{
			"definitions/base.yaml": baseYAML,
			"definitions/good.yaml": childYAML("good", nil),
		})
		if err := r.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if h, err := r.Resolve("good"); err != nil || h.SHA != "sha1" {
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
		if _, syncErrs := r.List(); !strings.Contains(strings.Join(syncErrs, "|"), "last sync failed, showing the last good one: provisioner down") {
			t.Fatalf("a stale list must say so: %v", syncErrs)
		}
	})

	// The same (name, sha) judged differently (a core rollout overlap): a
	// valid row's content, which sessions may pin, is never rewritten.
	t.Run("a valid row at the same sha is only ever invalidated", func(t *testing.T) {
		effOf := func() (bool, string) {
			var valid bool
			var allow string
			if err := pool.QueryRow(ctx, `SELECT valid, effective->'mode'->>'allow' FROM agent_definitions WHERE name='good' AND sha='sha1'`).Scan(&valid, &allow); err != nil {
				t.Fatal(err)
			}
			return valid, allow
		}
		_, before := effOf()
		lister.set("sha1", map[string]string{
			"definitions/base.yaml": baseYAML,
			"definitions/good.yaml": childYAML("good", map[string]string{"mode": "{ default: default, allow: [default, dontAsk] }"}),
		})
		if err := r.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if valid, allow := effOf(); !valid || allow != before {
			t.Fatalf("valid content rewritten in place: valid=%v allow=%s, want %s", valid, allow, before)
		}
		lister.set("sha1", map[string]string{
			"definitions/base.yaml": baseYAML,
			"definitions/good.yaml": childYAML("good", map[string]string{"mode": "{ default: auto, allow: [auto] }"}),
		})
		if err := r.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if valid, allow := effOf(); valid || allow != before {
			t.Fatalf("want invalidated with effective kept: valid=%v allow=%s", valid, allow)
		}
		if _, err := r.Resolve("good"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("want ErrInvalid: %v", err)
		}
		// An invalid row is not launchable, so a valid judgement replaces it.
		lister.set("sha1", map[string]string{
			"definitions/base.yaml": baseYAML,
			"definitions/good.yaml": good,
		})
		if err := r.Sync(ctx); err != nil {
			t.Fatal(err)
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
			t.Fatalf("HEAD's row stays current: %+v", h)
		}
	})

	// A newer core (rollout overlap, rollback) may store fields this one does
	// not know. Revoking that row would make the newer core re-insert at its
	// sha and strand every session pinned to it.
	t.Run("a row this core cannot decode is left alone", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO agent_definitions (name, sha, spec, effective, valid)
			SELECT 'good', 'sha9', spec, effective || '{"fromANewerCore": 1}', true
			  FROM agent_definitions WHERE name = 'good' AND sha = 'sha1'`); err != nil {
			t.Fatal(err)
		}
		if err := r.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		var valid bool
		if err := pool.QueryRow(ctx, `SELECT valid FROM agent_definitions WHERE name='good' AND sha='sha9'`).Scan(&valid); err != nil || !valid {
			t.Fatalf("a newer core's row was revoked: valid=%v err=%v", valid, err)
		}
	})

	// A deleted definition's rows stay for their pinned sessions, and so
	// must still answer to a tightened ceiling (SetPermissionMode reads valid).
	t.Run("re-judge reaches names no longer in git", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO agent_definitions (name, sha, spec, effective, valid)
			SELECT 'gone', 'g1', spec, jsonb_set(effective, '{mode}', '{"default":"auto","allow":["auto"]}'), true
			  FROM agent_definitions WHERE name = 'good' AND sha = 'sha1'`); err != nil {
			t.Fatal(err)
		}
		if err := r.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		var valid bool
		if err := pool.QueryRow(ctx, `SELECT valid FROM agent_definitions WHERE name='gone'`).Scan(&valid); err != nil || valid {
			t.Fatalf("want the unlisted row revoked: valid=%v err=%v", valid, err)
		}
	})
}

// One bad file must cost one invalid row, never the whole sync: a NUL reaching
// the upsert would roll back every definition's row, every 60s.
func TestRegistry_SyncSurvivesANUL(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(dbtest.NewPool(t), &fakeLister{})
	lister := r.lister.(*fakeLister)
	lister.set("sha1", map[string]string{
		"definitions/base.yaml": baseYAML,
		"definitions/good.yaml": childYAML("good", nil),
		"definitions/nul.yaml":  childYAML("nul", map[string]string{"systemPrompt": `{ append: "x\0y" }`}),
		"definitions/key.yaml":  childYAML("key", map[string]string{"git": `{ mode: pr, identity: { name: a, email: a@b.c, "\0": 1 } }`}),
	})
	if err := r.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := r.Resolve("good"); err != nil {
		t.Fatalf("good: %v", err)
	}
	for _, n := range []string{"nul", "key"} {
		if _, err := r.Resolve(n); !errors.Is(err, ErrInvalid) || rowCount(t, r, n) != 1 {
			t.Fatalf("%s: want one stored invalid row, got %v", n, err)
		}
	}
}

// A known, persisted state is logged once, not every 60s.
func TestRegistry_LogsOnlyChanges(t *testing.T) {
	ctx := context.Background()
	r := NewRegistry(dbtest.NewPool(t), &fakeLister{})
	r.lister.(*fakeLister).set("sha1", map[string]string{
		"definitions/base.yaml": baseYAML,
		"definitions/bad.yaml":  childYAML("bad", map[string]string{"mode": "{ default: auto, allow: [auto] }"}),
		"definitions/oops.yml":  "x",
	})
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "definitions: invalid") || !strings.Contains(out, "definitions: file error") {
		t.Fatalf("first sync must log both: %s", out)
	}
	buf.Reset()
	if err := r.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("an unchanged sync logged: %s", buf.String())
	}
}
