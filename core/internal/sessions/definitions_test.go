//go:build integration

package sessions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

// insertDefinition stores a valid definition row with just the two paths the
// SQL here reads: limits.concurrency and mode.allow.
func insertDefinition(t *testing.T, pool *pgxpool.Pool, name, sha string, concurrency int) {
	t.Helper()
	eff := fmt.Sprintf(`{"limits":{"concurrency":%d},"mode":{"default":"default","allow":["default","plan"]}}`, concurrency)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ($1, $2, '{}', $3::jsonb, true)`,
		name, sha, eff); err != nil {
		t.Fatal(err)
	}
}

func newDefinitionSession(t *testing.T, s *Store, name, sha string) string {
	t.Helper()
	id, err := s.Create(context.Background(), CreateParams{Repo: "dream-analyst", PermissionMode: "default", DefinitionName: name, DefinitionSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A refused reservation must leave no trace: no lease, no phase.
func assertUntouched(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	var lease, phase *string
	if err := pool.QueryRow(context.Background(), `SELECT lease_id::text, pod_phase FROM sessions WHERE id = $1`, id).Scan(&lease, &phase); err != nil {
		t.Fatal(err)
	}
	if lease != nil || phase != nil {
		t.Fatalf("a refused reservation wrote lease=%v phase=%v", lease, phase)
	}
}

func TestReserveSlot_DefinitionPool(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)
	insertDefinition(t, pool, "a", "sha-a", 5)
	insertDefinition(t, pool, "b", "sha-b", 5)
	caps := Caps{MaxLive: 5, MaxDefinition: 1}

	first := newDefinitionSession(t, store, "a", "sha-a")
	if _, err := store.ReserveSlot(ctx, first, caps); err != nil {
		t.Fatal(err)
	}
	second := newDefinitionSession(t, store, "b", "sha-b")
	_, err := store.ReserveSlot(ctx, second, caps)
	if !errors.Is(err, ErrAtCapacity) || !strings.Contains(err.Error(), "definition pool full") {
		t.Fatalf("want the pool refusal, got %v", err)
	}
	assertUntouched(t, pool, second)

	// The pool is inside the global cap, not a share of it: admins keep
	// MaxLive-MaxDefinition slots for repo sessions.
	if _, err := store.ReserveSlot(ctx, newSession(t, ctx, store), caps); err != nil {
		t.Fatalf("a repo session must still reserve with the pool full: %v", err)
	}
}

func TestReserveSlot_DefinitionConcurrency(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)
	insertDefinition(t, pool, "a", "sha-a", 1)
	insertDefinition(t, pool, "b", "sha-b", 1)
	caps := Caps{MaxLive: 5, MaxDefinition: 4}

	if _, err := store.ReserveSlot(ctx, newDefinitionSession(t, store, "a", "sha-a"), caps); err != nil {
		t.Fatal(err)
	}
	again := newDefinitionSession(t, store, "a", "sha-a")
	_, err := store.ReserveSlot(ctx, again, caps)
	if !errors.Is(err, ErrAtCapacity) || !strings.Contains(err.Error(), `definition "a" at its concurrency limit (1)`) {
		t.Fatalf("got %v", err)
	}
	assertUntouched(t, pool, again)
	if _, err := store.ReserveSlot(ctx, newDefinitionSession(t, store, "b", "sha-b"), caps); err != nil {
		t.Fatalf("another definition is under its own limit: %v", err)
	}
}

func TestReserveSlot_RevokedDefinitionRow(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)
	insertDefinition(t, pool, "a", "sha-a", 1)
	id := newDefinitionSession(t, store, "a", "sha-a")
	if _, err := pool.Exec(ctx, `UPDATE agent_definitions SET valid = false, error = 'revoked'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveSlot(ctx, id, Caps{MaxLive: 5, MaxDefinition: 2}); !errors.Is(err, ErrDefinitionInvalid) {
		t.Fatalf("got %v", err)
	}
	assertUntouched(t, pool, id)
}

func TestCreate_DefinitionPinRoundTrips(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)
	insertDefinition(t, pool, "a", "sha-a", 1)
	id := newDefinitionSession(t, store, "a", "sha-a")
	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.DefinitionName == nil || *got.DefinitionName != "a" || got.DefinitionSHA == nil || *got.DefinitionSHA != "sha-a" {
		t.Fatalf("pin: %v %v", got.DefinitionName, got.DefinitionSHA)
	}
	if *got.PermissionMode != "default" {
		t.Fatalf("mode %q", *got.PermissionMode)
	}
	repo, err := store.Get(ctx, newSession(t, ctx, store))
	if err != nil || repo.DefinitionName != nil || repo.DefinitionSHA != nil {
		t.Fatalf("a repo session has no pin: %+v %v", repo, err)
	}
	// The FK: a pin to a row that does not exist is refused.
	if _, err := store.Create(ctx, CreateParams{Repo: "x", DefinitionName: "a", DefinitionSHA: "nope"}); err == nil {
		t.Fatal("want the FK to refuse an unknown (name, sha)")
	}
}

// The one mode guard both SetPermissionMode callers route through: the pinned
// version's mode.allow, and only while that version is valid.
func TestSetPermissionMode_DefinitionGuard(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)
	insertDefinition(t, pool, "a", "sha-a", 1)
	def := newDefinitionSession(t, store, "a", "sha-a")
	repo := newSession(t, ctx, store)
	modeOf := func(id string) string {
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return *got.PermissionMode
	}
	for _, tc := range []struct {
		id, mode string
		allowed  bool
	}{
		{def, "plan", true},
		{def, "auto", false},
		{def, "acceptEdits", false},
		{repo, "auto", true},
	} {
		before := modeOf(tc.id)
		err := store.SetPermissionMode(ctx, tc.id, tc.mode)
		switch {
		case tc.allowed && (err != nil || modeOf(tc.id) != tc.mode):
			t.Errorf("%s: got %v, mode %s", tc.mode, err, modeOf(tc.id))
		case !tc.allowed && (!errors.Is(err, ErrModeNotAllowed) || modeOf(tc.id) != before):
			t.Errorf("%s: want ErrModeNotAllowed and no write, got %v, mode %s", tc.mode, err, modeOf(tc.id))
		}
	}
	// A revoked version authorizes nothing, not even a mode it listed.
	if _, err := pool.Exec(ctx, `UPDATE agent_definitions SET valid = false, error = 'revoked'`); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPermissionMode(ctx, def, "default"); !errors.Is(err, ErrModeNotAllowed) {
		t.Fatalf("revoked row: got %v", err)
	}
	if err := store.SetPermissionMode(ctx, "00000000-0000-0000-0000-000000000000", "auto"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

// Contract: the definition session's INSERT and its capacity check are one
// transaction, so a refusal keeps no row.
func TestCreateDefinition_CapacityRefusalKeepsNoRow(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)
	insertDefinition(t, pool, "a", "sha-a", 1)
	caps := Caps{MaxLive: 5, MaxDefinition: 4}
	p := CreateParams{Repo: "dream-analyst", PermissionMode: "default", DefinitionName: "a", DefinitionSHA: "sha-a"}
	count := func() (n int) {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE definition_name = 'a'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	first, err := store.CreateDefinition(ctx, p, caps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveSlot(ctx, first, caps); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDefinition(ctx, p, caps); !errors.Is(err, ErrAtCapacity) || !strings.Contains(err.Error(), "concurrency limit") {
		t.Fatalf("got %v", err)
	}
	if _, err := store.CreateDefinition(ctx, p, Caps{MaxLive: 1, MaxDefinition: 0}); !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("fleet cap: got %v", err)
	}
	if n := count(); n != 1 {
		t.Fatalf("refused creates kept rows: %d sessions, want 1", n)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_definitions SET valid = false, error = 'revoked'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateDefinition(ctx, p, Caps{MaxLive: 5, MaxDefinition: 4}); !errors.Is(err, ErrDefinitionInvalid) {
		t.Fatalf("revoked: got %v", err)
	}
	if n := count(); n != 1 {
		t.Fatalf("refused create kept a row: %d", n)
	}
}
