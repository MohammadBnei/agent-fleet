//go:build integration

package sessions

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// docs/adr/0070 §6, the re-park cap: a park is a pod exit that leaves an open
// decision. A single-shot run that parks more than limits.maxParks times
// (10 unset, owner answer 1) ends: endRuns claims it, tears it down and
// sweeps it; under the cap a parked run keeps its PVC. A decision answered
// before the exit result is no park.
func TestEndRuns_ParkLimit(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := NewStore(pool)
	tr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES
		('d', 'sha1', '{}', '{"limits":{"maxTurns":10}}', true),
		('tight', 'sha1', '{}', '{"limits":{"maxTurns":10,"maxParks":2}}', true)`); err != nil {
		t.Fatal(err)
	}
	create := func(def, lifecycle string) string {
		t.Helper()
		id, err := store.Create(ctx, CreateParams{Repo: "notes", DefinitionName: def, DefinitionSHA: "sha1",
			Owner: "client:bot", Invoker: "bot", Visibility: "private", Lifecycle: lifecycle})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tr.Append(ctx, id, "human", "go", "discussion", ""); err != nil {
			t.Fatal(err)
		}
		return id
	}
	human := transcript.Author{Sub: "client:bot", Name: "client:bot"}
	answer := func(id string, seq int64) {
		t.Helper()
		if _, err := tr.AppendAuthored(ctx, id, human, `{"behavior":"deny","message":"no"}`, "permission_response", "", &seq); err != nil {
			t.Fatal(err)
		}
	}
	// parks writes n parks: each turn asks, exits with the request open, and
	// the answer arrives after the exit. The last one stays open.
	parks := func(id string, n int) {
		t.Helper()
		for i := range n {
			q, err := tr.Append(ctx, id, "agent", `{"tool":"Bash","input":{"command":"ls `+string(rune('a'+i))+`"},"nb":1}`, "permission_request", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tr.Append(ctx, id, "worker", `{"subtype":"success","consumedSeq":100000,"exit":true}`, "result", ""); err != nil {
				t.Fatal(err)
			}
			if i < n-1 {
				answer(id, q)
			}
		}
		if err := store.SetPodPhase(ctx, id, "POD_PHASE_TERMINATED", ""); err != nil {
			t.Fatal(err)
		}
	}

	ten := create("d", "single-shot")
	parks(ten, 10)
	eleven := create("d", "single-shot")
	parks(eleven, 11)
	tight := create("tight", "single-shot")
	parks(tight, 3)
	// Asked and answered inside the turn, eleven times, then exited: no park.
	live := create("d", "single-shot")
	for i := range 11 {
		q, err := tr.Append(ctx, live, "agent", `{"tool":"Bash","input":{"command":"ls `+string(rune('a'+i))+`"},"nb":1}`, "permission_request", "")
		if err != nil {
			t.Fatal(err)
		}
		answer(live, q)
		if _, err := tr.Append(ctx, live, "worker", `{"subtype":"success","consumedSeq":100000,"exit":true}`, "result", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tr.Append(ctx, live, "agent", `{"tool":"Bash","input":{"command":"last"},"nb":1}`, "permission_request", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Append(ctx, live, "worker", `{"subtype":"success","consumedSeq":100000,"exit":true}`, "result", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPodPhase(ctx, live, "POD_PHASE_TERMINATED", ""); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		id              string
		parked, overCap bool
	}{{ten, true, false}, {eleven, true, true}, {tight, true, true}, {live, true, false}} {
		parked, overCap, err := store.ParkState(ctx, tc.id)
		if err != nil || parked != tc.parked || overCap != tc.overCap {
			t.Errorf("ParkState(%s) = %v %v %v, want %v %v", tc.id, parked, overCap, err, tc.parked, tc.overCap)
		}
		ex, err := store.LastExitResult(ctx, tc.id)
		if err != nil || ex.OverParkCap != tc.overCap {
			t.Errorf("LastExitResult(%s).OverParkCap = %v %v, want %v", tc.id, ex.OverParkCap, err, tc.overCap)
		}
	}

	fake := &fakeProvisioner{}
	loop := NewLoop(store, fake, time.Minute, time.Minute, time.Hour, 14*24*time.Hour, time.Minute)
	loop.endRuns(ctx)
	ended := []string{eleven, tight}
	slices.Sort(ended)
	var got []string
	for _, c := range fake.calls {
		if len(c) > len("sweep:") && c[:len("sweep:")] == "sweep:" {
			got = append(got, c[len("sweep:"):])
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, ended) {
		t.Fatalf("swept %v (calls %v), want the runs over their cap %v", got, fake.calls, ended)
	}
	for _, id := range ended {
		if s, err := store.Get(ctx, id); err != nil || s.ArchivedAt == nil || s.SweptAt == nil {
			t.Errorf("over the cap %s: %+v %v, want archived and swept", id, s, err)
		}
		if n, err := store.ParkedRuns(ctx, "client:bot"); err != nil || n != 2 {
			t.Errorf("parked runs = %d %v, want the two under the cap", n, err)
		}
	}
	for _, id := range []string{ten, live} {
		if s, err := store.Get(ctx, id); err != nil || s.ArchivedAt != nil || s.SweptAt != nil {
			t.Errorf("under the cap %s lost its PVC: %+v %v", id, s, err)
		}
	}
	// A resident session never reads the cap.
	resident := create("tight", "resident")
	parks(resident, 3)
	if _, overCap, err := store.ParkState(ctx, resident); err != nil || overCap {
		t.Errorf("resident over its cap: %v %v", overCap, err)
	}
}
