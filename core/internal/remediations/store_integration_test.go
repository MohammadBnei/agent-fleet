//go:build integration

package remediations

import (
	"context"
	"errors"
	"testing"
	"time"

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
	// The preview says the same without writing.
	svc := &Service{Store: st}
	raw := []byte(`{"action":"restart","kind":"deployment","namespace":"game","name":"a"}`)
	if err := svc.Check(ctx, "r2", raw, Bounds{}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("Check on a limited target: %v", err)
	}
	if err := svc.Check(ctx, "r2", []byte(`{"action":"restart","kind":"deployment","namespace":"game","name":"zz"}`), Bounds{}); err != nil {
		t.Fatalf("Check on a free target: %v", err)
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

func TestStore_SweepLost(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	st := NewStore(pool)
	pulled, _ := st.Insert(ctx, "r", restartOf("a"), Bounds{})
	pending, _ := st.Insert(ctx, "r", restartOf("b"), Bounds{})
	reported, _ := st.Insert(ctx, "r", restartOf("c"), Bounds{})
	for _, id := range []string{pulled, reported} {
		if _, err := st.Pull(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Report(ctx, reported, Outcome{Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if ids, err := st.SweepLost(ctx, 15*time.Minute); err != nil || len(ids) != 0 {
		t.Fatalf("fresh pull swept: %v %v", ids, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE remediations SET pulled_at = now() - interval '20 minutes'`); err != nil {
		t.Fatal(err)
	}
	ids, err := st.SweepLost(ctx, 15*time.Minute)
	if err != nil || len(ids) != 1 || ids[0] != pulled {
		t.Fatalf("sweep: %v %v", ids, err)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM remediations WHERE id = $1`, pending).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("pending touched: %s %v", state, err)
	}
	if err := st.Report(ctx, pulled, Outcome{Status: "done"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late report on a lost row: %v", err)
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

// Migration 000039: the CHECK no longer admits 'patch'.
func TestStore_RefusesPatchAction(t *testing.T) {
	st := NewStore(dbtest.NewPool(t))
	sel := Selection{Action: "patch", Kind: "deployment", Namespace: "game", Name: "a"}
	if _, err := st.Insert(context.Background(), "r", sel, Bounds{}); err == nil {
		t.Fatal("a patch row was accepted")
	}
}
