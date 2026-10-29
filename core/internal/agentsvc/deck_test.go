//go:build integration

package agentsvc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
)

// asSub stands in for the listeners: the principal is the member named by
// the X-Sub header, in the audience group unless it is "carol".
type asSub struct{}

func (asSub) principal(ctx context.Context, h http.Header) context.Context {
	sub := h.Get("X-Sub")
	groups := []string{"shared-notes"}
	switch sub {
	case "carol":
		groups = nil
	case "root":
		groups = []string{auth.RequiredGroup}
	}
	return withPrincipal(ctx, Principal{Member: &auth.Identity{Sub: sub, Email: sub + "@bnei.dev", Groups: groups}})
}

func (a asSub) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		return next(a.principal(ctx, req.Header()), req)
	}
}

func (asSub) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a asSub) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		return next(a.principal(ctx, conn.RequestHeader()), conn)
	}
}

func as[T any](sub string, msg *T) *connect.Request[T] {
	r := connect.NewRequest(msg)
	r.Header().Set("X-Sub", sub)
	return r
}

// The deck's reads see exactly the threads GetTranscript would show the
// caller, page newest first, and stream only the caller's changes.
func TestDecisionDeck(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool)
	transcr.SetAppendHook(decisions.OnAppend)
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('notes', 'https://example.invalid/notes.git')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('d', 'sha1', '{}', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	spec := &definitions.Spec{
		Audience:  &definitions.Audience{Group: "shared-notes"},
		Workspace: &definitions.Workspace{Kind: "repo", Repo: "notes"},
		Model:     &definitions.Model{Default: "claude-opus-5", Allow: []string{"claude-opus-5"}},
		Mode:      &definitions.Mode{Default: "auto", Allow: []string{"auto"}},
	}
	dash := dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test", "")
	s := &Server{definitions: fakeHeads{{Name: "d", SHA: "sha1", Effective: spec}}, dash: dash, sessions: store, transcr: transcr,
		hub: dashboard.NewHub(), decisions: decisions.NewStore(pool)}
	path, h := agentfleetv1connect.NewAgentServiceHandler(s, connect.WithInterceptors(asSub{}))
	mux := http.NewServeMux()
	mux.Handle(path, h)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	c := agentfleetv1connect.NewAgentServiceClient(hs.Client(), hs.URL)

	thread := func(vis string) string {
		r, err := c.CreateThread(ctx, as("alice", &agentfleetv1.CreateThreadRequest{Definition: "d", Visibility: vis}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.GetThread().GetId()
	}
	shared, priv := thread("shared"), thread("private")
	ask := func(id, typ, text string) int64 {
		seq, err := transcr.Append(ctx, id, "agent", text, typ, "")
		if err != nil {
			t.Fatal(err)
		}
		return seq
	}
	q := ask(shared, "question", `{"questions":[{"question":"which?"}]}`)
	ask(shared, "permission_request", `{"tool":"Bash","input":{"command":"ls"}}`)
	ask(priv, "question", `{"questions":[]}`)
	planSeq := ask(priv, "permission_request", `{"tool":"ExitPlanMode","input":{"plan":"p"}}`)
	if _, err := transcr.AppendAuthored(ctx, shared, transcript.Author{Sub: "bob", Name: "bob@bnei.dev"}, `{"which?":"a"}`, "answer", "", &q); err != nil {
		t.Fatal(err)
	}

	list := func(sub string, req *agentfleetv1.ListDecisionsRequest) ([]*agentfleetv1.Decision, error) {
		r, err := c.ListDecisions(ctx, as(sub, req))
		if err != nil {
			return nil, err
		}
		return r.Msg.GetDecisions(), nil
	}
	for sub, want := range map[string]int{"alice": 4, "bob": 2, "carol": 0, "root": 4} {
		if got, err := list(sub, &agentfleetv1.ListDecisionsRequest{}); err != nil || len(got) != want {
			t.Errorf("%s sees %d decisions (%v), want %d", sub, len(got), err, want)
		}
	}
	open := []agentfleetv1.DecisionState{agentfleetv1.DecisionState_DECISION_STATE_OPEN}
	if got, _ := list("alice", &agentfleetv1.ListDecisionsRequest{States: open}); len(got) != 3 {
		t.Errorf("alice's open: %d, want 3", len(got))
	}
	plans, _ := list("alice", &agentfleetv1.ListDecisionsRequest{Kinds: []agentfleetv1.DecisionKind{agentfleetv1.DecisionKind_DECISION_KIND_PLAN}})
	if len(plans) != 1 || plans[0].GetThreadId() != priv || plans[0].GetRequestSeq() != planSeq || plans[0].GetTool() != "ExitPlanMode" ||
		plans[0].GetAsk() != "p" || plans[0].GetState() != agentfleetv1.DecisionState_DECISION_STATE_OPEN || plans[0].GetCreatedAt() == "" {
		t.Errorf("alice's plans: %v", plans)
	}
	answered, _ := list("bob", &agentfleetv1.ListDecisionsRequest{States: []agentfleetv1.DecisionState{agentfleetv1.DecisionState_DECISION_STATE_ANSWERED}})
	if len(answered) != 1 || answered[0].GetAnswerJson() != `{"which?": "a"}` || answered[0].GetAnsweredBy() != "bob@bnei.dev" ||
		answered[0].GetAnsweredAt() == "" || answered[0].GetKind() != agentfleetv1.DecisionKind_DECISION_KIND_QUESTION {
		t.Errorf("bob's answered: %v", answered)
	}
	if got, _ := list("root", &agentfleetv1.ListDecisionsRequest{ThreadId: shared}); len(got) != 2 {
		t.Errorf("root on one thread: %d, want 2", len(got))
	}
	for name, req := range map[string]*agentfleetv1.ListDecisionsRequest{
		"unknown state": {States: []agentfleetv1.DecisionState{99}},
		"unknown kind":  {Kinds: []agentfleetv1.DecisionKind{agentfleetv1.DecisionKind_DECISION_KIND_UNSPECIFIED}},
		"bad token":     {PageToken: "nope"},
	} {
		if _, err := list("alice", req); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Pages of one walk alice's four, newest first.
	whole, _ := list("alice", &agentfleetv1.ListDecisionsRequest{})
	var paged []string
	tok := ""
	for range 10 {
		r, err := c.ListDecisions(ctx, as("alice", &agentfleetv1.ListDecisionsRequest{PageSize: 1, PageToken: tok}))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range r.Msg.GetDecisions() {
			paged = append(paged, d.GetId())
		}
		if tok = r.Msg.GetNextPageToken(); tok == "" {
			break
		}
	}
	if len(paged) != len(whole) {
		t.Fatalf("paged %v, whole %d", paged, len(whole))
	}
	for i := range whole {
		if paged[i] != whole[i].GetId() {
			t.Errorf("page %d: %s, want %s", i, paged[i], whole[i].GetId())
		}
	}

	get := func(sub, id string) error {
		_, err := c.GetDecision(ctx, as(sub, &agentfleetv1.GetDecisionRequest{Id: id}))
		return err
	}
	if err := get("alice", plans[0].GetId()); err != nil {
		t.Errorf("alice gets her plan: %v", err)
	}
	for sub, id := range map[string]string{"bob": plans[0].GetId(), "carol": answered[0].GetId(), "alice": "nope"} {
		if err := get(sub, id); connect.CodeOf(err) != connect.CodeNotFound {
			t.Errorf("%s get %s: %v", sub, id, err)
		}
	}

	// Bob's stream: the baseline, then only the change on the thread he sees.
	decisions.PollEvery = 20 * time.Millisecond
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.WatchDecisions(sctx, as("bob", &agentfleetv1.WatchDecisionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() || len(stream.Msg().GetChangedIds()) != 0 || stream.Msg().GetVersion() == 0 {
		t.Fatalf("baseline: %v %v", stream.Msg(), stream.Err())
	}
	base := stream.Msg().GetVersion()
	ask(priv, "question", `{"questions":[]}`)
	time.Sleep(100 * time.Millisecond) // a poll passes with only alice's change
	seen := ask(shared, "question", `{"questions":[]}`)
	if !stream.Receive() {
		t.Fatal(stream.Err())
	}
	got := stream.Msg()
	mine, _ := list("bob", &agentfleetv1.ListDecisionsRequest{ThreadId: shared})
	if len(got.GetChangedIds()) != 1 || got.GetChangedIds()[0] != mine[0].GetId() || mine[0].GetRequestSeq() != seen || got.GetVersion() <= base+1 {
		t.Errorf("bob's change: %v, want only %s", got, mine[0].GetId())
	}

	// An archived thread's decisions leave the deck.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, priv); err != nil {
		t.Fatal(err)
	}
	if got, _ := list("alice", &agentfleetv1.ListDecisionsRequest{}); len(got) != 3 {
		t.Errorf("after archiving: %d, want shared's 3", len(got))
	}
	if err := get("alice", plans[0].GetId()); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("get on an archived thread: %v", err)
	}
}
