//go:build integration

package cardevents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	bridgev1 "github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1"
)

type fakePub struct {
	events []*bridgev1.CloudEvent
	fail   error
}

func (f *fakePub) Publish(_ context.Context, e *bridgev1.CloudEvent) error {
	if f.fail != nil {
		return f.fail
	}
	f.events = append(f.events, e)
	return nil
}

func draft(t *testing.T, st *decisions.Store, tr transcript.Store, session string, n int64) decisions.Record {
	t.Helper()
	opt := func(l, v, r string) *agentfleetv1.CardOption {
		return &agentfleetv1.CardOption{Label: l, Effect: &agentfleetv1.CardEffect{Verb: v}, Reversible: r}
	}
	r, err := st.Draft(context.Background(), tr, decisions.Draft{
		SessionID: session, From: "agent", Ask: "secret ask text",
		Subject: &agentfleetv1.CardSubject{Repo: "acme/web", Kind: "pr", Number: n},
		Options: []*agentfleetv1.CardOption{opt("Ship", "handoff", "partial"), opt("Close", "close_threads", "no")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCardStatesPublishOnce(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()
	tr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	st := decisions.NewStore(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('d', 'sha', '{}', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	var session string
	if err := pool.QueryRow(ctx, `INSERT INTO sessions (repo, definition_name, definition_sha, owner, invoker, thread_key, visibility)
		VALUES ('r', 'd', 'sha', 'client:x', 'x', 'k', 'private') RETURNING id`).Scan(&session); err != nil {
		t.Fatal(err)
	}
	pub := &fakePub{}
	e := New(pool, pub, true)

	first := draft(t, st, tr, session, 1)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(pub.events) != 1 {
		t.Fatalf("published %d, want the opened card", len(pub.events))
	}
	o := pub.events[0]
	if o.Type != TypePrefix+"opened" || o.Subject != "forge/acme/web/pr/1" || !strings.HasPrefix(o.Id, "card/"+first.ID+"/") || o.Actor != "machine" {
		t.Fatalf("event %+v", o)
	}
	if strings.Contains(string(o.Data), "secret") {
		t.Fatal("the ask leaked into the event")
	}
	// Nothing changed: nothing published, also from a new instance (a restart).
	if err := New(pool, pub, true).Tick(ctx); err != nil || len(pub.events) != 1 {
		t.Fatalf("republished an unchanged card: %d %v", len(pub.events), err)
	}

	// A second draft on the subject makes the first stale and opens itself: two events, in version order.
	second := draft(t, st, tr, session, 1)
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(pub.events) != 3 || pub.events[1].Type != TypePrefix+"stale" || pub.events[2].Type != TypePrefix+"opened" {
		t.Fatalf("after a supersede: %d events %v", len(pub.events), types(pub.events))
	}

	// Answered carries the chosen option.
	if _, err := pool.Exec(ctx, `UPDATE decisions SET state = 'answered', answer = '{"optionId":"2"}', answered_at = now(),
		version = nextval('decisions_version_seq') WHERE id = $1`, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	a := pub.events[len(pub.events)-1]
	var data map[string]any
	if err := json.Unmarshal(a.Data, &data); err != nil {
		t.Fatal(err)
	}
	if a.Type != TypePrefix+"answered" || data["option_id"] != "2" || data["writer_definition"] != "d" || data["drafted_by"] != "agent" {
		t.Fatalf("answered %s %v", a.Type, data)
	}

	// Parked: the bridge refuses the next one as invalid. It is not retried and does not stall.
	third := draft(t, st, tr, session, 2)
	pub.fail = connect.NewError(connect.CodeInvalidArgument, errors.New("bad"))
	n := len(pub.events)
	if err := e.Tick(ctx); err != nil || len(pub.events) != n {
		t.Fatalf("a refused event: %v", err)
	}
	pub.fail = nil
	if err := e.Tick(ctx); err != nil || len(pub.events) != n {
		t.Fatalf("a parked card was published again: %v", err)
	}
	var pending int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM decisions WHERE kind = 'card' AND state IS DISTINCT FROM bridge_state`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending %d %v", pending, err)
	}

	// Any other failure leaves the card pending for the next tick.
	if _, err := pool.Exec(ctx, `UPDATE decisions SET state = 'withdrawn', version = nextval('decisions_version_seq') WHERE id = $1`, third.ID); err != nil {
		t.Fatal(err)
	}
	pub.fail = connect.NewError(connect.CodeUnavailable, errors.New("down"))
	if err := e.Tick(ctx); err == nil {
		t.Fatal("an outage was not reported")
	}
	pub.fail = nil
	if err := e.Tick(ctx); err != nil || pub.events[len(pub.events)-1].Type != TypePrefix+"withdrawn" {
		t.Fatalf("after the outage: %v %v", err, types(pub.events))
	}
}

func types(es []*bridgev1.CloudEvent) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Type)
	}
	return out
}

// The migration marks existing cards as published: no flood at deploy.
func TestExistingCardsAreNotBackfilled(t *testing.T) {
	pool := dbtest.NewPool(t)
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM decisions WHERE kind = 'card' AND bridge_state IS NULL`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
}
