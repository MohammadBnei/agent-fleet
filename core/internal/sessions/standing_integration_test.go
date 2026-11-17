//go:build integration

package sessions

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
)

func crash(t *testing.T, pool *pgxpool.Pool, id string, seq int64, age string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, thread_key, created_at)
		VALUES ($1, $2, 'fleet', $3, 'system', gen_random_uuid()::text, 'main', now() - $4::interval)`, id, seq, CrashEntry("OOMKilled"), age); err != nil {
		t.Fatal(err)
	}
}

func due(t *testing.T, store *Store, id string) bool {
	t.Helper()
	ids, err := store.ListRewarmStanding(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(ids, id)
}

func setRestart(t *testing.T, pool *pgxpool.Pool, id, ago string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE sessions SET restart_after = now() - $2::interval WHERE id = $1`, id, ago); err != nil {
		t.Fatal(err)
	}
}

// A character with no pod is warmed again (NULL restart_after is due); a live, stopped or crash-looping one is not,
// and the back-off doubles per crash entry of the last hour (docs/adr/0084 §3).
func TestListRewarmStanding_DueBackoffAndThreeCrashes(t *testing.T) {
	ctx := context.Background()
	store, pool, id := newCharacter(t)
	if !due(t, store, id) {
		t.Fatal("a started character with no pod and a NULL restart_after is due")
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET pod_phase = 'POD_PHASE_RUNNING' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if due(t, store, id) {
		t.Fatal("a character with a live pod is not due")
	}
	// Every terminal path stamps restart_after.
	for _, phase := range []string{"POD_PHASE_TERMINATED", "POD_PHASE_CRASHED"} {
		if _, err := pool.Exec(ctx, `UPDATE sessions SET restart_after = NULL WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if err := store.SetPodPhase(ctx, id, phase, "gone"); err != nil {
			t.Fatal(err)
		}
		var stamped bool
		if err := pool.QueryRow(ctx, `SELECT restart_after IS NOT NULL FROM sessions WHERE id = $1`, id).Scan(&stamped); err != nil || !stamped {
			t.Fatalf("%s left restart_after unset (%v)", phase, err)
		}
	}
	// No crash entry: no back-off. One: 10s. Two: 20s.
	setRestart(t, pool, id, "1 second")
	if !due(t, store, id) {
		t.Fatal("no crash entries, no back-off")
	}
	crash(t, pool, id, 1, "1 minute")
	setRestart(t, pool, id, "5 seconds")
	if due(t, store, id) {
		t.Fatal("one crash entry: 10s back-off not over")
	}
	setRestart(t, pool, id, "11 seconds")
	if !due(t, store, id) {
		t.Fatal("one crash entry: 10s back-off over")
	}
	crash(t, pool, id, 2, "1 minute")
	if due(t, store, id) {
		t.Fatal("two crash entries: 11s is inside the doubled 20s back-off")
	}
	setRestart(t, pool, id, "21 seconds")
	if !due(t, store, id) {
		t.Fatal("two crash entries: 20s back-off over")
	}
	// An entry older than the hour does not count; the third inside it stops the re-warm.
	crash(t, pool, id, 3, "2 hours")
	if !due(t, store, id) {
		t.Fatal("a crash entry from two hours ago counted")
	}
	crash(t, pool, id, 4, "1 minute")
	setRestart(t, pool, id, "1 day")
	if due(t, store, id) {
		t.Fatal("the third crash in an hour must stop the re-warm")
	}
	if n, err := store.CrashesInWindow(ctx, id); err != nil || n != 3 {
		t.Fatalf("crashes in window = %d, %v; want 3", n, err)
	}
}

// Stop excludes the row from the re-warm and clears its clock; Start makes it due again.
func TestListRewarmStanding_StoppedIsNeverListed(t *testing.T) {
	ctx := context.Background()
	store, pool, id := newCharacter(t)
	setRestart(t, pool, id, "1 hour")
	if ok, err := store.MarkStandingStopped(ctx, id); err != nil || !ok {
		t.Fatalf("stop: %v %v", ok, err)
	}
	var cleared bool
	if err := pool.QueryRow(ctx, `SELECT restart_after IS NULL FROM sessions WHERE id = $1`, id).Scan(&cleared); err != nil || !cleared {
		t.Fatalf("Stop must clear restart_after (%v)", err)
	}
	if due(t, store, id) {
		t.Fatal("a stopped character was listed for re-warm")
	}
	if ok, err := store.RestartStanding(ctx, id, "sha-a", "root"); err != nil || !ok {
		t.Fatalf("start: %v %v", ok, err)
	}
	if !due(t, store, id) {
		t.Fatal("a started character is due")
	}
	// A single-shot row is not this selector's.
	other := newDefinitionSession(t, store, "a", "sha-a")
	if slices.Contains(mustList(t, store), other) {
		t.Fatal("a non-standing row was listed")
	}
}

func mustList(t *testing.T, store *Store) []string {
	t.Helper()
	ids, err := store.ListRewarmStanding(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// A character is exempt from the idle and stop-grace sweeps, but not from the owner's Stop; a startup stall is
// listed for an alert instead of a teardown (docs/adr/0084 §3).
func TestSweeps_SkipStandingRows(t *testing.T) {
	ctx := context.Background()
	store, pool, id := newCharacter(t)
	if _, err := pool.Exec(ctx, `UPDATE sessions SET pod_phase = 'POD_PHASE_RUNNING', last_active_at = now() - interval '30 days',
		updated_at = now() - interval '1 hour', activity_seen = false, stop_requested_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if ids, err := store.ListIdleSessionIDs(ctx, time.Minute); err != nil || slices.Contains(ids, id) {
		t.Fatalf("idle sweep took a character: %v %v", ids, err)
	}
	if ids, err := store.ListOverdueStopIDs(ctx, time.Minute); err != nil || slices.Contains(ids, id) {
		t.Fatalf("stop-grace sweep took a running character: %v %v", ids, err)
	}
	if ids, err := store.ListStartupStalledIDs(ctx, time.Minute); err != nil || slices.Contains(ids, id) {
		t.Fatalf("startup-stall teardown took a character: %v %v", ids, err)
	}
	if ids, err := store.ListStalledStandingIDs(ctx, time.Minute); err != nil || !slices.Contains(ids, id) {
		t.Fatalf("a stalled character must be listed for an alert: %v %v", ids, err)
	}
	// The owner's Stop still ends the pod after its grace.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET stopped_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if ids, err := store.ListOverdueStopIDs(ctx, time.Minute); err != nil || !slices.Contains(ids, id) {
		t.Fatalf("a stopped character's grace-period teardown is gone: %v %v", ids, err)
	}
	// An ordinary session is still swept.
	plain := newDefinitionSession(t, store, "a", "sha-a")
	if _, err := pool.Exec(ctx, `UPDATE sessions SET pod_phase = 'POD_PHASE_RUNNING', last_active_at = now() - interval '30 days' WHERE id = $1`, plain); err != nil {
		t.Fatal(err)
	}
	if ids, err := store.ListIdleSessionIDs(ctx, time.Minute); err != nil || !slices.Contains(ids, plain) {
		t.Fatalf("an ordinary idle session must still be swept: %v %v", ids, err)
	}
}

func TestRunningThreads(t *testing.T) {
	ctx := context.Background()
	store, pool, id := newCharacter(t)
	openThread(t, pool, id, "busy", 0)
	openThread(t, pool, id, "quiet", 0)
	put(t, pool, id, 1, "busy", "human", "discussion", "go")
	put(t, pool, id, 2, "busy", "agent", "assistant", "working")
	put(t, pool, id, 3, "quiet", "human", "discussion", "hi")
	got, err := store.RunningThreads(ctx, id)
	if err != nil || len(got) != 1 || got[0] != "busy" {
		t.Fatalf("running = %v, %v; want [busy]", got, err)
	}
	// RunningThreads reads; it never moves a crash counter.
	var n int
	if err := pool.QueryRow(ctx, `SELECT crashes FROM session_threads WHERE session_id = $1 AND thread_key = 'busy'`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("crashes = %d (%v)", n, err)
	}
}

type pinFixture struct {
	store *Store
	pool  *pgxpool.Pool
	id    string
	fake  *fakeProvisioner
	loop  *Loop
	head  definitions.Head
	alert []string
	warm  []string
}

func newPinFixture(t *testing.T, oldEff, newEff string) *pinFixture {
	t.Helper()
	store, pool, id := newCharacter(t)
	f := &pinFixture{store: store, pool: pool, id: id, fake: &fakeProvisioner{livePods: map[string]string{}}}
	put := func(sha, eff string) {
		if _, err := pool.Exec(context.Background(), `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('a', $1, '{}', $2::jsonb, true)
			ON CONFLICT (name, sha) DO UPDATE SET effective = EXCLUDED.effective`, sha, eff); err != nil {
			t.Fatal(err)
		}
	}
	put("sha-a", oldEff)
	put("sha-b", newEff)
	got, err := store.DefinitionEffective(context.Background(), "a", "sha-b")
	if err != nil {
		t.Fatal(err)
	}
	spec := *got
	spec.Lifecycle = &definitions.AllowList{Allow: []string{Standing}}
	f.head = definitions.Head{Name: "a", SHA: "sha-b", Effective: &spec}
	f.loop = NewLoop(store, f.fake, time.Minute, time.Minute, time.Hour, time.Hour, time.Minute)
	f.loop.SetStandingHead(func(string) (definitions.Head, error) { return f.head, nil })
	f.loop.SetAlert(func(_ context.Context, _ string, summary string) { f.alert = append(f.alert, summary) })
	f.loop.SetRewarm(func(_ context.Context, id string) error { f.warm = append(f.warm, id); return nil })
	return f
}

func (f *pinFixture) live(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE sessions SET pod_phase = 'POD_PHASE_RUNNING' WHERE id = $1`, f.id); err != nil {
		t.Fatal(err)
	}
}

func (f *pinFixture) pin(t *testing.T) string {
	t.Helper()
	var sha string
	if err := f.pool.QueryRow(context.Background(), `SELECT definition_sha FROM sessions WHERE id = $1`, f.id).Scan(&sha); err != nil {
		t.Fatal(err)
	}
	return sha
}

const (
	effTools  = `{"tools":{"allow":["Read"]},"lifecycle":{"allow":["standing"]}}`
	effSame   = `{"tools":{"allow":["Read"]},"lifecycle":{"allow":["standing"]},"model":{"default":"x","allow":["x"]}}`
	effWider  = `{"tools":{"allow":["Read","Bash"]},"lifecycle":{"allow":["standing"]}}`
	effNarrow = `{"tools":{"allow":[]},"lifecycle":{"allow":["standing"]}}`
)

// A change that widens nothing drains: intake stops, a running turn is waited for until its deadline, then the Job
// is deleted, the row re-pinned and the character re-warmed (docs/adr/0084 §3).
func TestRepin_DrainsARunningTurnThenReplacesThePod(t *testing.T) {
	ctx := context.Background()
	f := newPinFixture(t, effTools, effSame)
	f.live(t)
	openThread(t, f.pool, f.id, "busy", 0)
	put(t, f.pool, f.id, 1, "busy", "human", "discussion", "go")
	put(t, f.pool, f.id, 2, "busy", "agent", "assistant", "working")

	f.loop.keepStanding(ctx)
	if draining, _ := f.store.Draining(ctx, f.id); !draining {
		t.Fatal("intake must stop while a turn runs")
	}
	if len(f.fake.tornWorkers) != 0 || f.pin(t) != "sha-a" {
		t.Fatalf("a running turn was cut off before its deadline: torn %v pin %s", f.fake.tornWorkers, f.pin(t))
	}
	// The turn finishes.
	put(t, f.pool, f.id, 3, "busy", "agent", "result", `{"consumedSeq":1}`)
	f.loop.keepStanding(ctx)
	if !slices.Equal(f.fake.tornWorkers, []string{f.id}) || f.pin(t) != "sha-b" {
		t.Fatalf("after the turn: torn %v pin %s", f.fake.tornWorkers, f.pin(t))
	}
	if draining, _ := f.store.Draining(ctx, f.id); draining {
		t.Fatal("the drain must end with the re-pin")
	}
	if !slices.Equal(f.warm, []string{f.id}) {
		t.Fatalf("the re-pinned character was not re-warmed on the same tick: %v", f.warm)
	}
}

func TestRepin_TheDeadlineEndsADrain(t *testing.T) {
	ctx := context.Background()
	f := newPinFixture(t, effTools, effSame)
	f.live(t)
	openThread(t, f.pool, f.id, "busy", 0)
	put(t, f.pool, f.id, 1, "busy", "human", "discussion", "go")
	put(t, f.pool, f.id, 2, "busy", "agent", "assistant", "working")
	f.loop.keepStanding(ctx)
	if _, err := f.pool.Exec(ctx, `UPDATE sessions SET drain_started_at = now() - interval '17 minutes' WHERE id = $1`, f.id); err != nil {
		t.Fatal(err)
	}
	f.loop.keepStanding(ctx)
	if f.pin(t) != "sha-b" || len(f.fake.tornWorkers) != 1 {
		t.Fatalf("past the turn deadline the pod goes: pin %s torn %v", f.pin(t), f.fake.tornWorkers)
	}
}

// A widening is refused: the pin and the pod stay, and the owner is told once.
func TestRepin_RefusedOnAWidening(t *testing.T) {
	ctx := context.Background()
	f := newPinFixture(t, effTools, effWider)
	f.live(t)
	for range 3 {
		f.loop.keepStanding(ctx)
	}
	if f.pin(t) != "sha-a" || len(f.fake.tornWorkers) != 0 {
		t.Fatalf("a widening was re-pinned: pin %s torn %v", f.pin(t), f.fake.tornWorkers)
	}
	if len(f.alert) != 1 {
		t.Fatalf("the owner must be told once, got %v", f.alert)
	}
}

// A narrowing takes the pod down at once, without waiting for a running turn.
func TestRepin_NarrowingTearsDownAtOnce(t *testing.T) {
	ctx := context.Background()
	f := newPinFixture(t, effWider, effNarrow)
	f.live(t)
	openThread(t, f.pool, f.id, "busy", 0)
	put(t, f.pool, f.id, 1, "busy", "human", "discussion", "go")
	put(t, f.pool, f.id, 2, "busy", "agent", "assistant", "working")
	f.loop.keepStanding(ctx)
	if f.pin(t) != "sha-b" || len(f.fake.tornWorkers) != 1 {
		t.Fatalf("a narrowing waited: pin %s torn %v", f.pin(t), f.fake.tornWorkers)
	}
}

// A revoked definition takes the pod down at once and is never re-pinned.
func TestRepin_RevokedTearsDown(t *testing.T) {
	ctx := context.Background()
	f := newPinFixture(t, effTools, effSame)
	f.live(t)
	f.loop.SetStandingHead(func(string) (definitions.Head, error) { return definitions.Head{}, definitions.ErrInvalid })
	f.loop.keepStanding(ctx)
	if f.pin(t) != "sha-a" || len(f.fake.tornWorkers) != 1 || len(f.alert) != 1 {
		t.Fatalf("revoked: pin %s torn %v alert %v", f.pin(t), f.fake.tornWorkers, f.alert)
	}
}

// A failed warm stamps the clock, and a startup stall alerts once and tears nothing down.
func TestKeepStanding_FailedWarmDefersAndAStallAlertsOnce(t *testing.T) {
	ctx := context.Background()
	f := newPinFixture(t, effTools, effTools)
	f.head.SHA = "sha-a"
	f.loop.SetRewarm(func(context.Context, string) error { return context.DeadlineExceeded })
	f.loop.keepStanding(ctx)
	var stamped bool
	if err := f.pool.QueryRow(ctx, `SELECT restart_after IS NOT NULL FROM sessions WHERE id = $1`, f.id).Scan(&stamped); err != nil || !stamped {
		t.Fatalf("a failed warm left restart_after unset (%v)", err)
	}
	f.live(t)
	if _, err := f.pool.Exec(ctx, `UPDATE sessions SET activity_seen = false, updated_at = now() - interval '1 hour' WHERE id = $1`, f.id); err != nil {
		t.Fatal(err)
	}
	f.loop.enforceStartupStall(ctx)
	f.loop.stalledStanding(ctx)
	f.loop.stalledStanding(ctx)
	if len(f.fake.tornWorkers) != 0 || len(f.alert) != 1 {
		t.Fatalf("stall: torn %v alerts %v", f.fake.tornWorkers, f.alert)
	}
}

// The stall alert fires once over several full ticks: a healthy pin must not wipe its memory (docs/adr/0084 §3).
func TestKeepStanding_AStallAlertsOnceAcrossTicks(t *testing.T) {
	ctx := context.Background()
	f := newPinFixture(t, effTools, effTools)
	f.head.SHA = "sha-a"
	f.live(t)
	if _, err := f.pool.Exec(ctx, `UPDATE sessions SET activity_seen = false, updated_at = now() - interval '1 hour' WHERE id = $1`, f.id); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		f.loop.keepStanding(ctx)
		f.loop.stalledStanding(ctx)
	}
	if len(f.alert) != 1 {
		t.Fatalf("a stall alerted %d times over four ticks: %v", len(f.alert), f.alert)
	}
	// Output arrives, then a new stall: it alerts again.
	if _, err := f.pool.Exec(ctx, `UPDATE sessions SET activity_seen = true WHERE id = $1`, f.id); err != nil {
		t.Fatal(err)
	}
	f.loop.stalledStanding(ctx)
	if _, err := f.pool.Exec(ctx, `UPDATE sessions SET activity_seen = false WHERE id = $1`, f.id); err != nil {
		t.Fatal(err)
	}
	f.loop.stalledStanding(ctx)
	if len(f.alert) != 2 {
		t.Fatalf("a second stall must alert: %v", f.alert)
	}
}

// A drain whose head then widens, or is reverted, ends: intake resumes on the live pod.
func TestRepin_AWideningOrRevertMidDrainEndsTheDrain(t *testing.T) {
	ctx := context.Background()
	f := newPinFixture(t, effTools, effSame)
	f.live(t)
	openThread(t, f.pool, f.id, "busy", 0)
	put(t, f.pool, f.id, 1, "busy", "human", "discussion", "go")
	put(t, f.pool, f.id, 2, "busy", "agent", "assistant", "working")
	f.loop.keepStanding(ctx)
	if d, _ := f.store.Draining(ctx, f.id); !d {
		t.Fatal("setup: no drain")
	}
	// The head moves on to a version that widens the ceiling.
	if _, err := f.pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('a', 'sha-c', '{}', $1::jsonb, true)`, effWider); err != nil {
		t.Fatal(err)
	}
	wide, err := f.store.DefinitionEffective(ctx, "a", "sha-c")
	if err != nil {
		t.Fatal(err)
	}
	f.head = definitions.Head{Name: "a", SHA: "sha-c", Effective: wide}
	f.loop.keepStanding(ctx)
	if d, _ := f.store.Draining(ctx, f.id); d {
		t.Fatal("a widening left intake stopped")
	}
	// Revert mid-drain: back to the pinned version.
	f.head.SHA = "sha-a"
	f.head.Effective = wide
	if _, err := f.store.Drain(ctx, f.id); err != nil {
		t.Fatal(err)
	}
	f.loop.keepStanding(ctx)
	if d, _ := f.store.Draining(ctx, f.id); d || len(f.fake.tornWorkers) != 0 {
		t.Fatalf("a revert must end the drain and keep the pod: draining %v torn %v", d, f.fake.tornWorkers)
	}
}

// A failed warm backs the re-warm off like a crash does, and a refused pin is not warmed at all.
func TestKeepStanding_FailedWarmsBackOffAndARefusedPinIsNotWarmed(t *testing.T) {
	ctx := context.Background()
	f := newPinFixture(t, effTools, effTools)
	f.head.SHA = "sha-a"
	var warms int
	f.loop.SetRewarm(func(context.Context, string) error { warms++; return context.DeadlineExceeded })
	f.loop.keepStanding(ctx)
	f.loop.keepStanding(ctx) // the same instant: backed off, not retried
	if warms != 1 {
		t.Fatalf("a failed warm was retried at once: %d warms", warms)
	}
	setRestart(t, f.pool, f.id, "11 seconds") // past the first back-off (10s), not the second (20s)
	if !due(t, f.store, f.id) {
		t.Fatal("one failure: due after 10s")
	}
	f.loop.keepStanding(ctx)
	setRestart(t, f.pool, f.id, "11 seconds")
	if due(t, f.store, f.id) {
		t.Fatal("two failures: the back-off doubled to 20s")
	}
	// A widening is never warmed: only Stop and Start moves it.
	g := newPinFixture(t, effTools, effWider)
	var gw int
	g.loop.SetRewarm(func(context.Context, string) error { gw++; return nil })
	g.loop.keepStanding(ctx)
	if gw != 0 {
		t.Fatalf("a widened pin was warmed %d times", gw)
	}
}

// ADR-0084 §3 as built: the three-crash stop holds for an hour, then the character is listed again.
func TestListRewarmStanding_TheStopLapsesWhenTheCrashesAgeOut(t *testing.T) {
	store, pool, id := newCharacter(t)
	for i, age := range []string{"1 minute", "2 minutes", "3 minutes"} {
		crash(t, pool, id, int64(10+i), age)
	}
	setRestart(t, pool, id, "1 day")
	if due(t, store, id) {
		t.Fatal("three crashes in the hour must stop the re-warm")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE transcript SET created_at = now() - interval '2 hours' WHERE session_id = $1 AND "from" = 'fleet'`, id); err != nil {
		t.Fatal(err)
	}
	if !due(t, store, id) {
		t.Fatal("crashes older than the window no longer stop the re-warm")
	}
}
