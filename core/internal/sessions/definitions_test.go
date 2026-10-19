//go:build integration

package sessions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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

// An older core knows nothing of definitions (docs/adr/0064). It must neither
// MINT a definition session's lease (it would warm one as a repo session) nor
// VERIFY one (it applies no definition allowlist). It reads and writes only
// lease_id; this core keeps a definition lease in profile_lease_id.
func TestDefinitionLeaseIsInvisibleToAnOlderCore(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)
	insertDefinition(t, pool, "a", "sha-a", 1)
	def := newDefinitionSession(t, store, "a", "sha-a")
	// An older core's ReserveSlot write.
	_, err := pool.Exec(ctx, `UPDATE sessions SET lease_id = gen_random_uuid(), pod_phase = 'Pending' WHERE id = $1`, def)
	if err == nil || !strings.Contains(err.Error(), "sessions_definition_no_lease") {
		t.Fatalf("want the CHECK to refuse an older core's lease, got %v", err)
	}
	assertUntouched(t, pool, def)
	lease, err := store.ReserveSlot(ctx, def, Caps{MaxLive: 5, MaxDefinition: 4})
	if err != nil {
		t.Fatalf("this core leases a definition session: %v", err)
	}
	if ok, name, err := store.VerifyLease(ctx, def, lease); err != nil || !ok || name != "a" {
		t.Fatalf("VerifyLease = %v %q %v, want the definition's name", ok, name, err)
	}
	// An older core's VerifyLease query.
	var older bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE id = $1 AND lease_id::text = $2)`, def, lease).Scan(&older); err != nil || older {
		t.Fatalf("an older core verifies a definition lease: %v %v", older, err)
	}
	if ok, err := store.SaveAgentSessionID(ctx, def, "sdk-1", "", lease); err != nil || !ok {
		t.Fatalf("SaveAgentSessionID with the definition lease: %v %v", ok, err)
	}
	// An older core cannot revoke one either: its teardown's ClearLease nulls
	// lease_id only. The TERMINATED the provisioner reports still revokes it.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET lease_id = NULL, pod_phase = 'POD_PHASE_TERMINATED' WHERE id = $1`, def); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := store.VerifyLease(ctx, def, lease); err != nil || ok {
		t.Fatalf("a definition lease outlived its pod: %v %v", ok, err)
	}
	if ok, err := store.SaveAgentSessionID(ctx, def, "sdk-2", "", lease); err != nil || ok {
		t.Fatalf("SaveAgentSessionID with a dead pod's definition lease: %v %v", ok, err)
	}
	if lease, err = store.ReserveSlot(ctx, def, Caps{MaxLive: 5, MaxDefinition: 4}); err != nil {
		t.Fatal(err)
	}
	if err := store.ClearLease(ctx, def); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := store.VerifyLease(ctx, def, lease); err != nil || ok {
		t.Fatalf("a cleared definition lease still verifies: %v %v", ok, err)
	}
	repo := newSession(t, ctx, store)
	repoLease, err := store.ReserveSlot(ctx, repo, Caps{MaxLive: 5, MaxDefinition: 4})
	if err != nil {
		t.Fatal(err)
	}
	if ok, name, err := store.VerifyLease(ctx, repo, repoLease); err != nil || !ok || name != "" {
		t.Fatalf("a repo lease: %v %q %v", ok, name, err)
	}
	// A repo session never holds a definition lease: it would verify with no
	// allowlist applied.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET profile_lease_id = gen_random_uuid() WHERE id = $1`, repo); err == nil || !strings.Contains(err.Error(), "sessions_profile_lease_definition_only") {
		t.Fatalf("want the CHECK to refuse a repo session's definition lease, got %v", err)
	}
	if ok, _, err := store.VerifyLease(ctx, repo, "not-a-lease"); err != nil || ok {
		t.Fatalf("a wrong lease: %v %v", ok, err)
	}
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
	if err := store.SetPermissionMode(ctx, def, "default"); !errors.Is(err, ErrDefinitionInvalid) {
		t.Fatalf("revoked row: got %v", err)
	}
	if err := store.SetPermissionMode(ctx, "00000000-0000-0000-0000-000000000000", "auto"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

// The pod's SaveSessionId reports the model it ran on. For a definition
// session that report must never move the pinned model.default, or the next
// warm would launch in whatever the pod said.
func TestSaveAgentSessionID_DefinitionModelIsPinned(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)
	insertDefinition(t, pool, "a", "sha-a", 1)
	def, err := store.Create(ctx, CreateParams{Repo: "dream-analyst", Model: "claude-sonnet-5", PermissionMode: "default", DefinitionName: "a", DefinitionSHA: "sha-a"})
	if err != nil {
		t.Fatal(err)
	}
	repo := newSession(t, ctx, store)
	for id, want := range map[string]string{def: "claude-sonnet-5", repo: "claude-opus-5-5"} {
		lease, err := store.ReserveSlot(ctx, id, Caps{MaxLive: 5, MaxDefinition: 4})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.SaveAgentSessionID(ctx, id, "agent-1", "claude-opus-5-5", lease); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get(ctx, id)
		if err != nil || got.Model == nil || *got.Model != want {
			t.Errorf("%s: model %v, want %s (err %v)", id, got.Model, want, err)
		}
	}
}

// docs/adr/0065: a definition session idles out at its own limits.timeout,
// shorter or longer than the global idle timeout; a repo session still at the
// global one.
func TestListIdleSessionIDs_ADefinitionIdlesAtItsOwnTimeout(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)
	for name, timeout := range map[string]string{"short": "10m", "long": "2h"} {
		eff := fmt.Sprintf(`{"limits":{"concurrency":4,"timeout":%q},"mode":{"default":"default","allow":["default"]}}`, timeout)
		if _, err := pool.Exec(ctx,
			`INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ($1, $1, '{}', $2::jsonb, true)`,
			name, eff); err != nil {
			t.Fatal(err)
		}
	}
	idleFor := func(id string, ago string) string {
		if _, err := store.ReserveSlot(ctx, id, Caps{MaxLive: 10, MaxDefinition: 10}); err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE sessions SET pod_phase = 'POD_PHASE_RUNNING', last_active_at = now() - $2::interval WHERE id = $1`, id, ago); err != nil {
			t.Fatal(err)
		}
		return id
	}
	shortIdle := idleFor(newDefinitionSession(t, store, "short", "short"), "20 minutes")
	longIdle := idleFor(newDefinitionSession(t, store, "long", "long"), "90 minutes")
	longGone := idleFor(newDefinitionSession(t, store, "long", "long"), "3 hours")
	repoIdle := idleFor(newSession(t, ctx, store), "90 minutes")
	repoFresh := idleFor(newSession(t, ctx, store), "20 minutes")

	ids, err := store.ListIdleSessionIDs(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{shortIdle: true, longIdle: false, longGone: true, repoIdle: true, repoFresh: false} {
		if contains(ids, id) != want {
			t.Errorf("session %s idle = %v, want %v (got %v)", id, !want, want, ids)
		}
	}
}
