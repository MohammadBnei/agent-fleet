//go:build integration

package sessionevents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/MohammadBnei/agent-fleet/core/internal/bridgeclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/cardevents"
	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/metrics"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	bridgev1 "github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1"
)

// fakePub records events; fail picks an error per event.
type fakePub struct {
	events []*bridgev1.CloudEvent
	fail   func(*bridgev1.CloudEvent) error
}

func (f *fakePub) Publish(_ context.Context, e *bridgev1.CloudEvent) error {
	if f.fail != nil {
		if err := f.fail(e); err != nil {
			return err
		}
	}
	f.events = append(f.events, e)
	return nil
}

func (f *fakePub) types() string {
	var got []string
	for _, ev := range f.events {
		got = append(got, ev.Type[len(TypePrefix):])
	}
	return strings.Join(got, ",")
}

func newSession(t *testing.T, pool *pgxpool.Pool, repo string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `INSERT INTO sessions (repo, title) VALUES ($1, 't') RETURNING id::text`, repo).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func result(t *testing.T, pool *pgxpool.Pool, id string, seq int64, at string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, created_at)
		VALUES ($1::uuid, $2::bigint, 'worker', '{"isError":true,"durationMs":5}', 'result', 'k' || $2::bigint::text, `+at+`)`, id, seq); err != nil {
		t.Fatal(err)
	}
}

func exec(t *testing.T, pool *pgxpool.Pool, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), q, args...); err != nil {
		t.Fatal(err)
	}
}

func TestOldRowsNotPublishedNewOnesAre(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()
	old := newSession(t, pool, "old")
	result(t, pool, old, 1, "now()")
	// Pretend the columns are new: reset them and run the backfill migration itself over the old rows.
	exec(t, pool, `UPDATE sessions SET bridge_state = NULL, bridge_turn_seq = -1`)
	sql, err := os.ReadFile("../../../db/migrations/000045_session_bridge_backfill.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(t, pool, string(sql))
	pub := &fakePub{}
	e := New(pool, pub, true)
	if err := e.Tick(ctx); err != nil || len(pub.events) != 0 {
		t.Fatalf("old rows published: %v %v", err, pub.events)
	}
	// A result on an old open session after deploy still flows, with no opened before it.
	result(t, pool, old, 2, "now()")
	if err := e.Tick(ctx); err != nil || pub.types() != "turn_ended" {
		t.Fatalf("old session's new turn: %v %q", err, pub.types())
	}
	// A new session: a result at seq 0 is sent, then ended, once each across ticks.
	pub.events = nil
	fresh := newSession(t, pool, "fresh")
	result(t, pool, fresh, 0, "now()")
	exec(t, pool, `UPDATE sessions SET archived_at = now() WHERE id = $1`, fresh)
	for range 2 {
		if err := e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if pub.types() != "opened,turn_ended,ended" {
		t.Fatalf("events %q", pub.types())
	}
	if pub.events[1].Id != "session/"+fresh+"/turn/0" {
		t.Fatalf("turn id %q", pub.events[1].Id)
	}
}

func TestNothingAfterEnded(t *testing.T) {
	pool := dbtest.NewPool(t)
	id := newSession(t, pool, "r")
	result(t, pool, id, 1, "now() - interval '1 minute'")
	exec(t, pool, `UPDATE sessions SET archived_at = now() WHERE id = $1`, id)
	result(t, pool, id, 2, "now() + interval '1 minute'") // written after the archive
	pub := &fakePub{}
	e := New(pool, pub, true)
	for range 2 {
		if err := e.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if pub.types() != "opened,turn_ended,ended" { // ended last, and turn 2 never
		t.Fatalf("events %q", pub.types())
	}
}

func TestRefusalParksAndRetriesLater(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()
	id := newSession(t, pool, "r")
	result(t, pool, id, 1, "now()")
	refuse := true
	pub := &fakePub{fail: func(e *bridgev1.CloudEvent) error {
		if refuse && strings.HasSuffix(e.Type, "turn_ended") {
			return connect.NewError(connect.CodePermissionDenied, errors.New("no"))
		}
		return nil
	}}
	e := New(pool, pub, true)
	refused := testutil.ToFloat64(metrics.SessionEventsRefused)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if testutil.ToFloat64(metrics.SessionEventsRefused) != refused+1 || testutil.ToFloat64(metrics.SessionEventsParked) != 1 {
		t.Fatal("refusal or parked gauge not counted")
	}
	var parked bool
	if err := pool.QueryRow(ctx, `SELECT bridge_parked_at IS NOT NULL FROM sessions WHERE id = $1`, id).Scan(&parked); err != nil || !parked {
		t.Fatalf("park not recorded: %v %v", parked, err)
	}
	n := len(pub.events)
	if err := e.Tick(ctx); err != nil || len(pub.events) != n || testutil.ToFloat64(metrics.SessionEventsRefused) != refused+1 {
		t.Fatal("a parked session was tried again at once")
	}
	refuse = false
	base := e.now()
	e.now = func() time.Time { return base.Add(ParkRetry + time.Minute) }
	if err := e.Tick(ctx); err != nil || !strings.HasSuffix(pub.types(), "turn_ended") {
		t.Fatalf("not retried after the park window: %v %q", err, pub.types())
	}
	if err := pool.QueryRow(ctx, `SELECT bridge_parked_at IS NOT NULL FROM sessions WHERE id = $1`, id).Scan(&parked); err != nil || parked {
		t.Fatal("park not cleared after success")
	}
}

func TestBridgeDownStopsTickAndKeepsState(t *testing.T) {
	pool := dbtest.NewPool(t)
	id := newSession(t, pool, "r")
	pub := &fakePub{fail: func(*bridgev1.CloudEvent) error { return connect.NewError(connect.CodeUnavailable, errors.New("down")) }}
	e := New(pool, pub, true)
	err := e.Tick(context.Background())
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("error swallowed: %v", err)
	}
	var state *string
	if err := pool.QueryRow(context.Background(), `SELECT bridge_state FROM sessions WHERE id = $1`, id).Scan(&state); err != nil || state != nil {
		t.Fatalf("state moved without an accepted publish: %v %v", state, err)
	}
}

func TestOneBadSessionDoesNotBlockOthers(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()
	bad := newSession(t, pool, "bad")
	good := newSession(t, pool, "good")
	calls := 0
	pub := &fakePub{fail: func(e *bridgev1.CloudEvent) error {
		if strings.Contains(e.Subject, bad) {
			calls++
			return errors.New("odd")
		}
		return nil
	}}
	e := New(pool, pub, true)
	base := e.now()
	clock := base
	e.now = func() time.Time { return clock }
	if err := e.Tick(ctx); err != nil {
		t.Fatalf("one bad session stopped the tick: %v", err)
	}
	if len(pub.events) != 1 || !strings.Contains(pub.events[0].Subject, good) || calls != 1 {
		t.Fatalf("events %v calls %d", pub.events, calls)
	}
	if err := e.Tick(ctx); err != nil || calls != 1 {
		t.Fatalf("no backoff: the bad session was tried again at once (%d calls)", calls)
	}
	clock = base.Add(cardevents.BackoffMin + time.Second)
	if err := e.Tick(ctx); err != nil || calls != 2 {
		t.Fatalf("not retried after its backoff (%d calls)", calls)
	}
	clock = clock.Add(cardevents.BackoffMin + time.Second) // the backoff doubled: still waiting
	if err := e.Tick(ctx); err != nil || calls != 2 {
		t.Fatalf("backoff did not grow (%d calls)", calls)
	}
}

func TestHardDeleteCountsLost(t *testing.T) {
	pool := dbtest.NewPool(t)
	id := newSession(t, pool, "r")
	e := New(pool, &fakePub{}, true)
	if err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := testutil.ToFloat64(metrics.SessionEventsLost)
	if err := sessions.NewStore(pool).Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if testutil.ToFloat64(metrics.SessionEventsLost) != before+1 {
		t.Fatal("a deleted open session was not counted lost")
	}
}

func TestRefusalCodesPark(t *testing.T) {
	for _, code := range []connect.Code{connect.CodeInvalidArgument, connect.CodePermissionDenied} {
		t.Run(code.String(), func(t *testing.T) {
			pool := dbtest.NewPool(t)
			id := newSession(t, pool, "r")
			pub := &fakePub{fail: func(*bridgev1.CloudEvent) error { return connect.NewError(code, errors.New("no")) }}
			e := New(pool, pub, true)
			if err := e.Tick(context.Background()); err != nil {
				t.Fatalf("a refusal is not a tick error: %v", err)
			}
			var parked bool
			if err := pool.QueryRow(context.Background(), `SELECT bridge_parked_at IS NOT NULL FROM sessions WHERE id = $1`, id).Scan(&parked); err != nil || !parked {
				t.Fatalf("%v did not park: %v %v", code, parked, err)
			}
		})
	}
}

func TestBridgeWideErrorsStopTheTick(t *testing.T) {
	for name, perr := range map[string]error{
		"unauthenticated": connect.NewError(connect.CodeUnauthenticated, errors.New("x")),
		"not configured":  bridgeclient.ErrNotConfigured,
		"deadline":        connect.NewError(connect.CodeDeadlineExceeded, errors.New("x")),
		"canceled":        connect.NewError(connect.CodeCanceled, errors.New("x")),
		"ctx deadline":    context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			pool := dbtest.NewPool(t)
			for range 3 {
				newSession(t, pool, "r")
			}
			calls := 0
			pub := &fakePub{fail: func(*bridgev1.CloudEvent) error { calls++; return perr }}
			e := New(pool, pub, true)
			if err := e.Tick(context.Background()); err == nil || calls != 1 {
				t.Fatalf("err %v after %d publishes: the tick should stop at the first", err, calls)
			}
		})
	}
}

func TestHungBridgeEndsTickAfterOneTimeout(t *testing.T) {
	pool := dbtest.NewPool(t)
	for range 30 {
		newSession(t, pool, "r")
	}
	old := cardevents.PublishTimeout
	cardevents.PublishTimeout = 200 * time.Millisecond
	t.Cleanup(func() { cardevents.PublishTimeout = old })
	calls := 0
	e := New(pool, &hungPub{calls: &calls}, true)
	start := time.Now()
	err := e.Tick(context.Background())
	if err == nil || calls != 1 || time.Since(start) > 700*time.Millisecond {
		t.Fatalf("err %v, %d publishes, %v: a hung bridge must cost one timeout", err, calls, time.Since(start))
	}
}

type hungPub struct{ calls *int }

func (h *hungPub) Publish(ctx context.Context, _ *bridgev1.CloudEvent) error {
	*h.calls++
	<-ctx.Done()
	return ctx.Err()
}

func TestFailuresResetAndPrune(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()
	a := newSession(t, pool, "a")
	b := newSession(t, pool, "b")
	failing := map[string]bool{a: true, b: true}
	pub := &fakePub{fail: func(e *bridgev1.CloudEvent) error {
		for id, f := range failing {
			if f && strings.Contains(e.Subject, id) {
				return errors.New("odd")
			}
		}
		return nil
	}}
	e := New(pool, pub, true)
	clock := e.now()
	e.now = func() time.Time { return clock }
	if err := e.Tick(ctx); err != nil || len(e.fails) != 2 {
		t.Fatalf("fails %v %v", e.fails, err)
	}
	failing[a] = false
	if err := sessions.NewStore(pool).Delete(ctx, b); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Hour)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.fails) != 0 {
		t.Fatalf("a success must clear its failure and a gone session must be pruned: %v", e.fails)
	}
}

func TestGlobalBackoffIsUsed(t *testing.T) {
	pool := dbtest.NewPool(t)
	newSession(t, pool, "r")
	var mu sync.Mutex
	calls := 0
	pub := &fakePub{fail: func(*bridgev1.CloudEvent) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}}
	e := New(pool, pub, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx, 10*time.Millisecond); close(done) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("%d publishes in 300ms of a down bridge: Run must back off", calls)
	}
}

func TestPagesPastOneBatchBeforeEnded(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()
	id := newSession(t, pool, "r")
	exec(t, pool, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, created_at)
		SELECT $1::uuid, g, 'w', '{}', 'result', 'k' || g, now() - interval '1 hour' FROM generate_series(1, 150) g`, id)
	exec(t, pool, `UPDATE sessions SET archived_at = now() WHERE id = $1`, id)
	pub := &fakePub{}
	e := New(pool, pub, true)
	if err := e.Tick(ctx); err != nil || strings.HasSuffix(pub.types(), ",ended") {
		t.Fatalf("ended went out before every turn: %v", err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(pub.types(), ",")
	if len(got) != 152 || got[0] != "opened" || got[len(got)-1] != "ended" || strings.Count(pub.types(), "turn_ended") != 150 {
		t.Fatalf("%d events, first %s last %s", len(got), got[0], got[len(got)-1])
	}
	for i, ev := range pub.events[1:151] {
		if want := fmt.Sprintf("session/%s/turn/%d", id, i+1); ev.Id != want {
			t.Fatalf("event %d is %s, want %s (seq order)", i, ev.Id, want)
		}
	}
}

// created_at is the transaction's start: an append begun before the archive can commit after ended went out.
func TestLateCommitAfterEndedIsNotPublished(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()
	id := newSession(t, pool, "r")
	late, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := late.Exec(ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key)
		VALUES ($1::uuid, 1, 'w', '{}', 'result', 'late')`, id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // the archive's transaction starts after the append's
	exec(t, pool, `UPDATE sessions SET archived_at = now() WHERE id = $1`, id)
	pub := &fakePub{}
	e := New(pool, pub, true)
	if err := e.Tick(ctx); err != nil || pub.types() != "opened,ended" {
		t.Fatalf("%v %q", err, pub.types())
	}
	if err := late.Commit(ctx); err != nil { // its created_at is before archived_at, but ended is out
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil || pub.types() != "opened,ended" {
		t.Fatalf("a turn went out after ended: %v %q", err, pub.types())
	}
}
