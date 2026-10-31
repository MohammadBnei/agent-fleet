//go:build integration

package agentsvc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
)

// docs/adr/0082 §6 over real Postgres: a card is an admin's to answer; a
// handoff warms and appends, a writer that cannot resume gets the answer
// recorded and "not delivered", a stale card is keyed, and close_threads
// archives every open thread on the subject, whatever its case.
func TestAnswerDecision_Card(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('notes', 'https://example.invalid/notes.git')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('d', 'sha1', '{}', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	dash := &noWarm{store: store, Server: dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test", "")}
	decs := decisions.NewStore(pool)
	s := &Server{definitions: fakeHeads{{Name: "d", SHA: "sha1"}}, dash: dash, sessions: store, transcr: transcr,
		hub: dashboard.NewHub(), decisions: decs}
	path, h := agentfleetv1connect.NewAgentServiceHandler(s, connect.WithInterceptors(asSub{}))
	mux := http.NewServeMux()
	mux.Handle(path, h)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	c := agentfleetv1connect.NewAgentServiceClient(hs.Client(), hs.URL)

	thread := func(key string) string {
		id, err := store.Create(ctx, sessions.CreateParams{Repo: "notes", DefinitionName: "d", DefinitionSHA: "sha1",
			Owner: "client:bridge", Visibility: "private", Invoker: "bridge", ThreadKey: key})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	option := func(label, verb, rev string) *agentfleetv1.CardOption {
		return &agentfleetv1.CardOption{Label: label, Effect: &agentfleetv1.CardEffect{Verb: verb}, Reversible: rev}
	}
	draft := func(writer string, number int64, opts ...*agentfleetv1.CardOption) string {
		r, err := decs.Draft(ctx, transcr, decisions.Draft{SessionID: writer, From: "system", Ask: "Close its threads?",
			Subject: &agentfleetv1.CardSubject{Repo: "o/r", Kind: "pr", Number: number}, Options: opts, WriteOwn: true})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	answer := func(sub string, m *agentfleetv1.AnswerDecisionRequest) (*agentfleetv1.AnswerDecisionResponse, error) {
		r, err := c.AnswerDecision(ctx, as(sub, m))
		if err != nil {
			return nil, err
		}
		return r.Msg, nil
	}
	state := func(id string) string {
		r, _ := decs.Get(ctx, id)
		return r.State
	}
	code := connect.CodeOf
	keep, closeAll := option("Keep them open", "handoff", "yes"), option("Close the threads", "close_threads", "no")

	writer := thread("github/o/r/pr/1")
	card := draft(writer, 1, closeAll, keep)
	if g, err := c.GetDecision(ctx, as("root", &agentfleetv1.GetDecisionRequest{Id: card})); err != nil ||
		g.Msg.GetDecision().GetCard().GetWriterDefinition() != "d" || len(g.Msg.GetDecision().GetCard().GetOptions()) != 2 {
		t.Fatalf("the deck's card: %v, %v", g, err)
	}
	if _, err := answer("alice", &agentfleetv1.AnswerDecisionRequest{DecisionId: card, OptionId: "2"}); code(err) != connect.CodePermissionDenied && code(err) != connect.CodeNotFound {
		t.Errorf("a member answering a card: %v", err)
	}
	if _, err := answer("root", &agentfleetv1.AnswerDecisionRequest{DecisionId: card, Behavior: "withdraw"}); code(err) != connect.CodeInvalidArgument {
		t.Errorf("withdrawing a card: %v", err)
	}
	if _, err := answer("root", &agentfleetv1.AnswerDecisionRequest{DecisionId: card, OptionId: "2", OwnText: "x"}); code(err) != connect.CodeInvalidArgument {
		t.Errorf("both: %v", err)
	}
	// A handoff warms the writer, then appends.
	res, err := answer("root", &agentfleetv1.AnswerDecisionRequest{DecisionId: card, OptionId: "2"})
	if err != nil || !res.GetDelivered() || state(card) != decisions.Answered || len(dash.warms) != 1 || dash.warms[0] != writer {
		t.Fatalf("handoff: %v %v %s warms %v", res, err, state(card), dash.warms)
	}
	e, _, _ := transcr.ReadSince(ctx, writer, res.GetSeq(), 1)
	if len(e) != 1 || e[0].Type != "answer" || !strings.Contains(e[0].Text, "owner chose option 2 of card "+card) {
		t.Errorf("the handoff's answer: %+v", e)
	}

	// A writer that cannot resume: recorded, not delivered.
	writer2 := thread("github/o/r/pr/2")
	card2 := draft(writer2, 2, keep, closeAll)
	dash.warmErr = connect.NewError(connect.CodeFailedPrecondition, decisions.CannotResume(errors.New("definition changed")))
	res, err = answer("root", &agentfleetv1.AnswerDecisionRequest{DecisionId: card2, OwnText: "leave it"})
	if err != nil || res.GetDelivered() || res.GetNotDeliveredReason() != "definition changed" || state(card2) != decisions.Answered {
		t.Errorf("undeliverable: %v %v %s", res, err, state(card2))
	}
	dash.warmErr = nil

	// A newer card on the subject makes the older one stale.
	writer3 := thread("github/o/r/pr/3")
	old := draft(writer3, 3, keep, closeAll)
	newer := draft(writer3, 3, closeAll, keep)
	if _, err := answer("root", &agentfleetv1.AnswerDecisionRequest{DecisionId: old, OptionId: "1"}); errkeys.Of(err) != errkeys.KeyDecisionStale {
		t.Errorf("a stale card: %v key %q", err, errkeys.Of(err))
	}

	// close_threads: every open thread on the subject, any case, any invoker.
	other := thread("github/O/R/pr/3")
	elsewhere := thread("github/o/r/pr/4")
	// An Open holding one thread's lock for a moment: the close waits for it
	// rather than leaving that thread open behind an answered card.
	held, err := store.TryLock(ctx, threadLock("client:bridge", "d", "github/O/R/pr/3"))
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(500 * time.Millisecond); held.Release() }()
	if _, err := answer("root", &agentfleetv1.AnswerDecisionRequest{DecisionId: newer, OptionId: "1"}); err != nil {
		t.Fatalf("close_threads: %v", err)
	}
	if state(newer) != decisions.Answered {
		t.Errorf("the card after close_threads: %s, want answered (the answer lands before the archive)", state(newer))
	}
	for id, archived := range map[string]bool{writer3: true, other: true, elsewhere: false} {
		got, _ := store.Get(ctx, id)
		if (got.ArchivedAt != nil) != archived {
			t.Errorf("thread %s archived %v, want %v", id, got.ArchivedAt != nil, archived)
		}
	}
	// The answered card stays listed though close_threads archived its writer.
	l, err := c.ListDecisions(ctx, as("root", &agentfleetv1.ListDecisionsRequest{}))
	if err != nil || !slices.ContainsFunc(l.Msg.GetDecisions(), func(d *agentfleetv1.Decision) bool { return d.GetId() == newer }) {
		t.Errorf("the closed card on an archived writer is not listed: %v", err)
	}
}
