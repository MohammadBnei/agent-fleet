//go:build integration

package agentsvc

import (
	"context"
	"testing"

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
)

// Decisions and control over real Postgres (docs/adr/0066 §4, 0067 §2).
// AnswerQuestion's success path warms, which needs a provisioner, so only its
// refusals are checked here.
func TestDecisions(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('notes', 'https://example.invalid/notes.git')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid)
		VALUES ('d', 'sha1', '{}', '{"mode":{"default":"auto","allow":["auto","plan"]},"limits":{"concurrency":1}}', true)`); err != nil {
		t.Fatal(err)
	}
	spec := &definitions.Spec{
		Audience:  &definitions.Audience{Group: "shared-notes"},
		Workspace: &definitions.Workspace{Kind: "repo", Repo: "notes"},
		Model:     &definitions.Model{Default: "claude-opus-5", Allow: []string{"claude-opus-5"}},
		Mode:      &definitions.Mode{Default: "auto", Allow: []string{"auto", "plan"}},
	}
	dash := dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test", "")
	s := &Server{definitions: fakeHeads{{Name: "d", SHA: "sha1", Effective: spec}}, dash: dash, sessions: store, transcr: transcr, hub: dashboard.NewHub()}
	as := func(sub string, groups ...string) context.Context {
		id := auth.Identity{Sub: sub, Email: sub + "@bnei.dev", Groups: groups}
		return withPrincipal(ctx, Principal{Member: &id})
	}
	alice, bob := as("alice", "shared-notes"), as("bob", "shared-notes")
	thread := func(vis string) string {
		r, err := s.CreateThread(alice, connect.NewRequest(&agentfleetv1.CreateThreadRequest{Definition: "d", Visibility: vis}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.GetThread().GetId()
	}
	shared, priv := thread("shared"), thread("private")
	entry := func(id, text, typ string) int64 {
		seq, err := transcr.Append(ctx, id, "agent", text, typ, "")
		if err != nil {
			t.Fatal(err)
		}
		return seq
	}
	bash := entry(shared, `{"tool":"Bash","input":{"command":"ls"}}`, "permission_request")
	plan := entry(shared, `{"tool":"ExitPlanMode","input":{"plan":"x"}}`, "permission_request")
	question := entry(shared, `{"questions":[]}`, "question")
	privReq := entry(priv, `{"tool":"Bash","input":{"command":"ls"}}`, "permission_request")

	respond := func(c context.Context, id string, seq int64, behavior string) error {
		_, err := s.RespondToPermission(c, connect.NewRequest(&agentfleetv1.AgentServiceRespondToPermissionRequest{ThreadId: id, Seq: seq, Behavior: behavior}))
		return err
	}
	code := connect.CodeOf

	if err := respond(bob, priv, privReq, "allow"); code(err) != connect.CodeNotFound {
		t.Errorf("bob on alice's private: %v", err)
	}
	if err := respond(bob, shared, plan, "allow"); code(err) != connect.CodeInvalidArgument {
		t.Errorf("a plan through RespondToPermission: %v", err)
	}
	if err := respond(bob, shared, question, "allow"); code(err) != connect.CodeInvalidArgument {
		t.Errorf("a question through RespondToPermission: %v", err)
	}
	if err := respond(bob, shared, bash, "maybe"); code(err) != connect.CodeInvalidArgument {
		t.Errorf("bad behavior: %v", err)
	}
	if err := respond(bob, shared, bash, "allow"); err != nil {
		t.Fatalf("bob on shared: %v", err)
	}
	if err := respond(alice, shared, bash, "deny"); code(err) != connect.CodeAlreadyExists {
		t.Errorf("second answer: %v", err)
	}
	e, _, _ := transcr.ReadSince(ctx, shared, bash+1, 50)
	var resp *transcript.Entry
	for i := range e {
		if e[i].Type == "permission_response" && e[i].ReplyTo != nil && *e[i].ReplyTo == bash {
			resp = &e[i]
		}
	}
	if resp == nil || resp.Text != `{"behavior":"allow"}` || resp.AuthorName != "bob@bnei.dev" {
		t.Errorf("recorded answer: %+v", resp)
	}
	// Entry.mine: the caller's own entries, never the agent's or another
	// member's.
	mine := func(c context.Context) map[int64]bool {
		r, err := s.GetTranscript(c, connect.NewRequest(&agentfleetv1.AgentServiceGetTranscriptRequest{ThreadId: shared}))
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]bool{}
		for _, e := range r.Msg.GetEntries() {
			out[e.GetSeq()] = e.GetMine()
		}
		return out
	}
	if m := mine(bob); !m[resp.Seq] || m[bash] {
		t.Errorf("bob's view: %v", m)
	}
	if m := mine(alice); m[resp.Seq] {
		t.Errorf("alice sees bob's answer as hers: %v", m)
	}

	for _, a := range []string{"[1]", "null"} {
		if _, err := s.AnswerQuestion(bob, connect.NewRequest(&agentfleetv1.AgentServiceAnswerQuestionRequest{ThreadId: shared, Seq: question, AnswersJson: a})); code(err) != connect.CodeInvalidArgument {
			t.Errorf("answers %s: %v", a, err)
		}
	}
	// An answered question refuses before warming (no provisioner here, so a
	// warm would fail with something else).
	answeredQ := entry(shared, `{"questions":[]}`, "question")
	aq := answeredQ
	if _, err := transcr.AppendAuthored(ctx, shared, transcript.Author{Sub: "alice"}, `{}`, "answer", "", &aq); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnswerQuestion(bob, connect.NewRequest(&agentfleetv1.AgentServiceAnswerQuestionRequest{ThreadId: shared, Seq: answeredQ, AnswersJson: "{}"})); code(err) != connect.CodeAlreadyExists {
		t.Errorf("answered question: %v", err)
	}

	approve := func(c context.Context, mode string) error {
		_, err := s.ApprovePlan(c, connect.NewRequest(&agentfleetv1.ApprovePlanRequest{ThreadId: shared, Seq: plan, Mode: mode}))
		return err
	}
	if err := approve(bob, "auto"); code(err) != connect.CodePermissionDenied {
		t.Errorf("bob switching alice's mode: %v", err)
	}
	if err := approve(alice, "acceptEdits"); code(err) != connect.CodeInvalidArgument {
		t.Errorf("mode outside mode.allow: %v", err)
	}
	// A plan someone else already denied: no mode switch, no success.
	denied := entry(shared, `{"tool":"ExitPlanMode","input":{"plan":"y"}}`, "permission_request")
	if err := respond(bob, shared, denied, "deny"); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("plan via RespondToPermission: %v", err)
	}
	dq := denied
	if _, err := transcr.AppendAuthored(ctx, shared, transcript.Author{Sub: "bob"}, `{"behavior":"deny"}`, "permission_response", "", &dq); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApprovePlan(alice, connect.NewRequest(&agentfleetv1.ApprovePlanRequest{ThreadId: shared, Seq: denied, Mode: "plan"})); code(err) != connect.CodeAlreadyExists {
		t.Errorf("approve a denied plan: %v", err)
	}
	if got, _ := store.Get(ctx, shared); deref(got.PermissionMode, "") != "auto" {
		t.Errorf("mode switched for a closed plan: %v", deref(got.PermissionMode, ""))
	}
	if err := approve(alice, "plan"); err != nil {
		t.Fatalf("alice approves into plan: %v", err)
	}
	if got, _ := store.Get(ctx, shared); deref(got.PermissionMode, "") != "plan" {
		t.Errorf("mode after approve: %v", deref(got.PermissionMode, ""))
	}

	setConfig := func(c context.Context, mode, model string) error {
		_, err := s.SetConfig(c, connect.NewRequest(&agentfleetv1.SetConfigRequest{ThreadId: shared, Mode: mode, Model: model}))
		return err
	}
	if err := setConfig(bob, "auto", ""); code(err) != connect.CodePermissionDenied {
		t.Errorf("bob SetConfig: %v", err)
	}
	if err := setConfig(alice, "", "claude-opus-5"); code(err) != connect.CodeUnimplemented {
		t.Errorf("model change: %v", err)
	}
	if err := setConfig(alice, "auto", ""); err != nil {
		t.Errorf("alice SetConfig: %v", err)
	}

	stop := func(c context.Context) error {
		_, err := s.Stop(c, connect.NewRequest(&agentfleetv1.StopRequest{ThreadId: shared}))
		return err
	}
	if err := stop(bob); code(err) != connect.CodePermissionDenied {
		t.Errorf("bob Stop: %v", err)
	}
	if err := stop(alice); err != nil {
		t.Errorf("alice Stop: %v", err)
	}
	if r, err := s.GetStatus(bob, connect.NewRequest(&agentfleetv1.GetStatusRequest{ThreadId: shared})); err != nil || r.Msg.GetStatus().GetState() != "idle" {
		t.Errorf("GetStatus: %v, %v", r, err)
	}

	// A tainted thread is configured by admins only, owner included.
	if _, err := pool.Exec(ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, context_only, untrusted)
		VALUES ($1, 1000, 'human', 'event', 'discussion', 'taint', true, true)`, shared); err != nil {
		t.Fatal(err)
	}
	if err := setConfig(alice, "auto", ""); code(err) != connect.CodePermissionDenied {
		t.Errorf("owner SetConfig on tainted: %v", err)
	}
	if err := setConfig(as("root", auth.RequiredGroup), "auto", ""); err != nil {
		t.Errorf("admin SetConfig on tainted: %v", err)
	}

	// A headless thread takes no answers.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET interaction = 'deny' WHERE id = $1`, priv); err != nil {
		t.Fatal(err)
	}
	if err := respond(alice, priv, privReq, "allow"); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("headless thread: %v", err)
	}
}
