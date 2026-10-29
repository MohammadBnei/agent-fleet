//go:build integration

package agentsvc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"connectrpc.com/connect"

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

// AnswerDecision through a Connect handler and the authz table: an admin in
// the console answers any decision it can see, console sessions included; a
// member answers by canAnswer, only on the threads it can see, and may
// neither pick a plan's mode on a thread it does not own nor remember an
// approval for the whole definition.
func TestAnswerDecision(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('notes', 'https://example.invalid/notes.git')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid)
		VALUES ('d', 'sha1', '{}', '{"mode":{"default":"auto","allow":["auto","plan"]}}', true)`); err != nil {
		t.Fatal(err)
	}
	spec := &definitions.Spec{
		Audience:  &definitions.Audience{Group: "shared-notes"},
		Workspace: &definitions.Workspace{Kind: "repo", Repo: "notes"},
		Model:     &definitions.Model{Default: "claude-opus-5", Allow: []string{"claude-opus-5"}},
		Mode:      &definitions.Mode{Default: "auto", Allow: []string{"auto", "plan"}},
	}
	dash := &noWarm{store: store, Server: dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test", "")}
	decs := decisions.NewStore(pool)
	s := &Server{definitions: fakeHeads{{Name: "d", SHA: "sha1", Effective: spec}}, dash: dash, sessions: store, transcr: transcr,
		hub: dashboard.NewHub(), decisions: decs}
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
	console, err := store.Create(ctx, sessions.CreateParams{Repo: "notes"})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(id, typ, text string) string {
		seq, err := transcr.Append(ctx, id, "agent", text, typ, "")
		if err != nil {
			t.Fatal(err)
		}
		r, err := decs.Request(ctx, id, seq)
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	const bash = `{"tool":"Bash","input":{"command":"ls"}}`
	answer := func(sub string, m *agentfleetv1.AnswerDecisionRequest) (*transcript.Entry, error) {
		r, err := c.AnswerDecision(ctx, as(sub, m))
		if err != nil {
			return nil, err
		}
		d, _ := decs.Get(ctx, m.GetDecisionId())
		e, _, _ := transcr.ReadSince(ctx, d.SessionID, r.Msg.GetSeq(), 1)
		return &e[0], nil
	}
	code := connect.CodeOf

	consoleReq := ask(console, "permission_request", bash)
	if _, err := answer("alice", &agentfleetv1.AnswerDecisionRequest{DecisionId: consoleReq, Behavior: "allow"}); code(err) != connect.CodeNotFound {
		t.Errorf("a member on a console session: %v", err)
	}
	if e, err := answer("root", &agentfleetv1.AnswerDecisionRequest{DecisionId: consoleReq, Behavior: "allow"}); err != nil || e.Text != `{"behavior":"allow"}` || e.AuthorName != "root@bnei.dev" {
		t.Errorf("admin on a console session: %+v, %v", e, err)
	}
	if _, err := answer("root", &agentfleetv1.AnswerDecisionRequest{DecisionId: consoleReq, Behavior: "deny"}); code(err) != connect.CodeAlreadyExists {
		t.Errorf("answered twice: %v", err)
	}

	privReq := ask(priv, "permission_request", bash)
	if _, err := answer("bob", &agentfleetv1.AnswerDecisionRequest{DecisionId: privReq, Behavior: "allow"}); code(err) != connect.CodeNotFound {
		t.Errorf("bob on alice's private thread: %v", err)
	}
	for _, id := range []string{"not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
		if _, err := answer("root", &agentfleetv1.AnswerDecisionRequest{DecisionId: id, Behavior: "allow"}); code(err) != connect.CodeNotFound {
			t.Errorf("decision %s: %v", id, err)
		}
	}

	sharedReq := ask(shared, "permission_request", bash)
	if _, err := answer("carol", &agentfleetv1.AnswerDecisionRequest{DecisionId: sharedReq, Behavior: "allow"}); code(err) != connect.CodeNotFound {
		t.Errorf("carol, outside the audience: %v", err)
	}
	if _, err := answer("bob", &agentfleetv1.AnswerDecisionRequest{DecisionId: sharedReq, Behavior: "allow", Scope: "definition"}); code(err) != connect.CodePermissionDenied {
		t.Errorf("bob remembering for the definition: %v", err)
	}
	if _, err := answer("bob", &agentfleetv1.AnswerDecisionRequest{DecisionId: sharedReq, Behavior: "maybe"}); code(err) != connect.CodeInvalidArgument {
		t.Errorf("behavior maybe: %v", err)
	}
	if e, err := answer("bob", &agentfleetv1.AnswerDecisionRequest{DecisionId: sharedReq, Behavior: "deny", Message: "no"}); err != nil || e.Text != `{"behavior":"deny","message":"no"}` {
		t.Errorf("bob on the shared thread: %+v, %v", e, err)
	}

	plan := ask(shared, "permission_request", `{"tool":"ExitPlanMode","input":{"plan":"x"}}`)
	if _, err := answer("bob", &agentfleetv1.AnswerDecisionRequest{DecisionId: plan, Behavior: "allow", Mode: "auto"}); code(err) != connect.CodePermissionDenied {
		t.Errorf("bob picking alice's mode: %v", err)
	}
	if _, err := answer("alice", &agentfleetv1.AnswerDecisionRequest{DecisionId: plan, Behavior: "allow", Mode: "plan"}); err != nil {
		t.Errorf("alice approving into plan: %v", err)
	}
	if got, _ := store.Get(ctx, shared); deref(got.PermissionMode, "") != "plan" {
		t.Errorf("mode after the approval: %s", deref(got.PermissionMode, ""))
	}
	// No mode switches nothing: bob, who may not name a mode, cannot get
	// one (auto is mode.allow's first) by leaving it out.
	plain := ask(shared, "permission_request", `{"tool":"ExitPlanMode","input":{"plan":"y"}}`)
	if e, err := answer("bob", &agentfleetv1.AnswerDecisionRequest{DecisionId: plain, Behavior: "allow"}); err != nil || e.Text != `{"behavior":"allow"}` {
		t.Errorf("bob approving with no mode: %+v, %v", e, err)
	}
	if got, _ := store.Get(ctx, shared); deref(got.PermissionMode, "") != "plan" {
		t.Errorf("mode after a plain approval: %s, want plan", deref(got.PermissionMode, ""))
	}

	// canSee but not canAnswer: a headless thread takes no answers.
	headless := thread("shared")
	headReq := ask(headless, "permission_request", bash)
	if _, err := pool.Exec(ctx, `UPDATE sessions SET interaction = 'deny' WHERE id = $1`, headless); err != nil {
		t.Fatal(err)
	}
	if _, err := answer("bob", &agentfleetv1.AnswerDecisionRequest{DecisionId: headReq, Behavior: "allow"}); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("bob on a headless thread: %v", err)
	}
	// An archived session's decisions are gone from the deck, for an admin too.
	if err := store.Archive(ctx, headless); err != nil {
		t.Fatal(err)
	}
	if _, err := answer("root", &agentfleetv1.AnswerDecisionRequest{DecisionId: headReq, Behavior: "allow"}); code(err) != connect.CodeNotFound {
		t.Errorf("admin on an archived session: %v", err)
	}

	q := ask(shared, "question", `{"questions":[{"question":"which?"}]}`)
	if _, err := answer("bob", &agentfleetv1.AnswerDecisionRequest{DecisionId: q, AnswersJson: "[1]"}); code(err) != connect.CodeInvalidArgument {
		t.Errorf("answers not an object: %v", err)
	}
	if e, err := answer("bob", &agentfleetv1.AnswerDecisionRequest{DecisionId: q, AnswersJson: `{"which?":"a"}`}); err != nil || e.Type != "answer" || e.Text != `{"which?":"a"}` {
		t.Errorf("bob answering the question: %+v, %v", e, err)
	}
	if !slices.Equal(dash.warms, []string{shared}) {
		t.Errorf("warms = %v: only the question warms", dash.warms)
	}

	// docs/adr/0082 §5: an interrupted decision says so, and "withdraw" closes
	// it, the owner's or an admin's, with nothing appended and nothing warmed.
	w := ask(shared, "permission_request", `{"tool":"Bash","input":{"command":"w"},"nb":1}`)
	if _, err := transcr.Append(ctx, shared, "human", "stop", "interrupt", ""); err != nil {
		t.Fatal(err)
	}
	if g, err := c.GetDecision(ctx, as("alice", &agentfleetv1.GetDecisionRequest{Id: w})); err != nil || !g.Msg.GetDecision().GetInterrupted() {
		t.Errorf("interrupted decision: %v, %v", g, err)
	}
	latest, _ := transcr.LatestSeq(ctx, shared)
	withdraw := func(sub, id string) error {
		_, err := c.AnswerDecision(ctx, as(sub, &agentfleetv1.AnswerDecisionRequest{DecisionId: id, Behavior: "withdraw"}))
		return err
	}
	if err := withdraw("bob", w); code(err) != connect.CodePermissionDenied {
		t.Errorf("bob withdrawing alice's decision: %v", err)
	}
	if err := withdraw("alice", w); err != nil {
		t.Errorf("alice withdrawing: %v", err)
	}
	if d, _ := decs.Get(ctx, w); d.State != decisions.Withdrawn {
		t.Errorf("after withdraw: %s", d.State)
	}
	if after, _ := transcr.LatestSeq(ctx, shared); after != latest || !slices.Equal(dash.warms, []string{shared}) {
		t.Errorf("withdraw appended (%d -> %d) or warmed (%v)", latest, after, dash.warms)
	}
	if err := withdraw("alice", w); code(err) != connect.CodeAlreadyExists {
		t.Errorf("withdrawn twice: %v", err)
	}
	if err := withdraw("root", ask(shared, "question", `{"questions":[{"question":"why?"}]}`)); err != nil {
		t.Errorf("an admin withdrawing: %v", err)
	}
}
