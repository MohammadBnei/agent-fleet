//go:build integration

package sessions

import (
	"context"
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

// docs/adr/0086 §2: the set is stored at create, read by VerifyLease, kept by a re-warm, and only ever narrows.
func TestTrustSet_StoredAtCreateKeptByWarmAndOnlyNarrows(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)

	// No tier given: an owner or fleet trigger, BC.
	owner, err := store.Create(ctx, CreateParams{Repo: "dream-analyst"})
	if err != nil {
		t.Fatal(err)
	}
	// An untrusted trigger whose definition names B.
	set, err := TrustSetFor(TierUntrusted, "B")
	if err != nil {
		t.Fatal(err)
	}
	insertDefinition(t, pool, "a", "sha-a", 1)
	id, err := store.Create(ctx, CreateParams{Repo: "dream-analyst", PermissionMode: "default", DefinitionName: "a", DefinitionSHA: "sha-a", TrustSet: set})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{owner: "BC", id: "AB"} {
		if got, err := store.Get(ctx, id); err != nil || got.TrustSet != want {
			t.Fatalf("Get = %+v, %v; want trust set %q", got, err, want)
		}
	}

	// Re-warm: a fresh lease each time, the same set, read by the auth lookup.
	for range 2 {
		lease, err := store.ReserveSlot(ctx, id, Caps{MaxLive: 5, MaxDefinition: 4})
		if err != nil {
			t.Fatal(err)
		}
		if ok, _, got, err := store.VerifyLease(ctx, id, lease); err != nil || !ok || got != "AB" {
			t.Fatalf("VerifyLease = %v %q %v, want the stored AB", ok, got, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE sessions SET pod_phase = 'Terminated' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}

	// Narrowing is allowed, widening is not, and an unknown letter set is refused.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET trust_set = 'AB' WHERE id = $1`, owner); err == nil || !strings.Contains(err.Error(), "only narrows") {
		t.Errorf("widening BC to AB: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET trust_set = 'ABC' WHERE id = $1`, id); err == nil || !strings.Contains(err.Error(), "only narrows") {
		t.Errorf("widening AB to ABC: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET trust_set = 'B' WHERE id = $1`, owner); err == nil {
		t.Error("a set outside the four was accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET trust_set = 'AB' WHERE id = $1`, id); err != nil {
		t.Errorf("same set: %v", err)
	}
}
