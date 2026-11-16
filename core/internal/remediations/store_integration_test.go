//go:build integration

package remediations

import (
	"context"
	"errors"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
)

func restartOf(name string) Selection {
	return Selection{Action: ActionRestart, Kind: "deployment", Namespace: "game", Name: name}
}

func TestStore_RateLimits(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	st := NewStore(pool)

	if _, err := st.Insert(ctx, "r1", restartOf("a"), Bounds{}); err != nil {
		t.Fatal(err)
	}
	// Same target inside 10 min, even from another responder.
	if _, err := st.Insert(ctx, "r2", restartOf("a"), Bounds{}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("same target: %v", err)
	}
	// Aged past 10 min it passes again.
	if _, err := pool.Exec(ctx, `UPDATE remediations SET created_at = now() - interval '11 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(ctx, "r1", restartOf("a"), Bounds{}); err != nil {
		t.Fatalf("after the window: %v", err)
	}
	// Six per responder per hour: r1 has 2 in the hour; four more reach 6.
	for _, n := range []string{"b", "c", "d", "e"} {
		if _, err := st.Insert(ctx, "r1", restartOf(n), Bounds{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Insert(ctx, "r1", restartOf("f"), Bounds{}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("seventh in the hour: %v", err)
	}
	if _, err := st.Insert(ctx, "r2", restartOf("f"), Bounds{}); err != nil {
		t.Fatalf("another responder: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE remediations SET created_at = now() - interval '61 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(ctx, "r1", restartOf("g"), Bounds{}); err != nil {
		t.Fatalf("after the hour: %v", err)
	}
}

func TestStore_SingleUse(t *testing.T) {
	ctx := context.Background()
	st := NewStore(dbtest.NewPool(t))
	n := 2
	id, err := st.Insert(ctx, "r", Selection{Action: ActionScale, Kind: "deployment", Namespace: "game", Name: "a", Replicas: &n}, Bounds{Min: 1, Max: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Report(ctx, id, Outcome{Status: "done"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("report before pull: %v", err)
	}
	row, err := st.Pull(ctx, id)
	if err != nil || row.Replicas != 2 || row.MinReplicas != 1 || row.MaxReplicas != 3 || row.Action != ActionScale {
		t.Fatalf("pull: %+v %v", row, err)
	}
	if _, err := st.Pull(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second pull: %v", err)
	}
	if err := st.Report(ctx, id, Outcome{Status: "maybe"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad status: %v", err)
	}
	if err := st.Report(ctx, id, Outcome{Status: "done", Detail: "scaled", Inverse: []string{"scale"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Report(ctx, id, Outcome{Status: "refused"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second report: %v", err)
	}
	if _, err := st.Pull(ctx, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}
