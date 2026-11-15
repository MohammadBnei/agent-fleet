//go:build integration

package transcript

import (
	"context"
	"testing"
)

// docs/adr/0070 §3: a result keyed result:<session>:<consumedSeq>:<k>.
// Distinct keys are distinct rows (the final result after a parked turn is
// not dropped as a duplicate); a retried key is one row.
func TestPostgresStore_ResultKeys(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := NewPostgresStore(pool, noHook)
	sessionID := newTestTask(t, ctx, pool)

	k1 := "result:" + sessionID + ":3:1"
	k2 := "result:" + sessionID + ":3:2"
	a, err := store.Append(ctx, sessionID, "agent", `{"subtype":"success"}`, "result", k1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Append(ctx, sessionID, "agent", `{"subtype":"success"}`, "result", k2)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := store.Append(ctx, sessionID, "agent", `{"subtype":"success"}`, "result", k1)
	if err != nil {
		t.Fatal(err)
	}
	if a == b || a != a2 {
		t.Fatalf("seqs: first %d, second %d, retried first %d", a, b, a2)
	}
	entries, _, err := store.ReadSince(ctx, sessionID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 rows, got %d", len(entries))
	}
}
