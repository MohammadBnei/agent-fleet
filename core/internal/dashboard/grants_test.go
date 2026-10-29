//go:build integration

package dashboard

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"connectrpc.com/connect"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/grants"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// docs/adr/0076 §3 end to end through RespondToPermission: core renders the
// rule from the parked request, stores it at the chosen scope, and hands the
// worker that rule in the decision; definition grants reach the definition's
// other sessions and leave them when revoked.
func TestGrants_RememberedAllow(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid)
		VALUES ('a', 'sha-a', '{}', '{"mode":{"default":"auto","allow":["auto","default"]}}', true)`); err != nil {
		t.Fatal(err)
	}
	newDef := func() string {
		id, err := store.Create(ctx, sessions.CreateParams{Repo: "dream-analyst", PermissionMode: "auto", DefinitionName: "a", DefinitionSHA: "sha-a"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	one, two := newDef(), newDef()
	repo, err := store.Create(ctx, sessions.CreateParams{Repo: "dream-analyst"})
	if err != nil {
		t.Fatal(err)
	}
	request := func(id, text string) int64 {
		seq, err := transcr.Append(ctx, id, "agent", text, "permission_request", "")
		if err != nil {
			t.Fatal(err)
		}
		return seq
	}
	g := grants.NewStore(pool)
	s := NewServer(store, nil, transcr, nil, nil, nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test", "")
	s.SetGrants(g)
	s.SetDecisions(decisions.NewStore(pool))
	respond := func(id string, seq int64, decision, scope, form string) error {
		_, err := s.RespondToPermission(ctx, connect.NewRequest(&agentfleetv1.RespondToPermissionRequest{
			SessionId: id, Seq: seq, DecisionJson: decision, Scope: scope, RuleForm: form,
		}))
		return err
	}
	code := func(err error) connect.Code { return connect.CodeOf(err) }

	edit := request(one, `{"tool":"Edit","input":{"file_path":"/workspace/notes/a.md"}}`)
	// Grantable, so the allow is also a one-time grant: its decisionId
	// rides along with the standing rule.
	if _, err := pool.Exec(ctx, `UPDATE decisions SET grantable = true WHERE session_id = $1 AND request_seq = $2`, one, edit); err != nil {
		t.Fatal(err)
	}
	prev, err := s.PreviewGrant(ctx, connect.NewRequest(&agentfleetv1.PreviewGrantRequest{SessionId: one, Seq: edit}))
	if err != nil || prev.Msg.GetExact() != "Edit(notes/a.md)" || prev.Msg.GetPrefix() != "Edit(notes/**)" {
		t.Fatalf("PreviewGrant = %v, %v", prev, err)
	}
	if err := respond(one, edit, `{"behavior":"allow","updatedInput":{"file_path":"/etc/x"}}`, grants.ScopeSession, grants.FormPrefix); err != nil {
		t.Fatalf("session grant: %v", err)
	}
	entries, _, err := transcr.ReadSince(ctx, one, edit+1, 1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("response entry: %v, %v", entries, err)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(entries[0].Text), &d); err != nil {
		t.Fatal(err)
	}
	// Core's rule rides along; the answerer's updatedInput does not.
	if d["grant"] != "Edit(notes/**)" || d["behavior"] != "allow" || d["updatedInput"] != nil || d["decisionId"] == nil {
		t.Fatalf("decision = %v", d)
	}

	bash := request(one, `{"tool":"Bash","input":{"command":"npm test"}}`)
	if err := respond(one, bash, `{"behavior":"allow"}`, grants.ScopeDefinition, grants.FormExact); err != nil {
		t.Fatalf("definition grant: %v", err)
	}
	for id, want := range map[string][]string{one: {"Bash(npm test)", "Edit(notes/**)"}, two: {"Bash(npm test)"}} {
		if got, err := g.Rules(ctx, id); err != nil || !slices.Equal(got, want) {
			t.Fatalf("Rules(%s) = %v, %v; want %v", id, got, err, want)
		}
	}

	list, err := s.ListGrants(ctx, connect.NewRequest(&agentfleetv1.ListGrantsRequest{}))
	if err != nil || len(list.Msg.GetGrants()) != 1 {
		t.Fatalf("ListGrants = %v, %v", list, err)
	}
	if _, err := s.RevokeGrant(ctx, connect.NewRequest(&agentfleetv1.RevokeGrantRequest{Id: list.Msg.GetGrants()[0].GetId()})); err != nil {
		t.Fatal(err)
	}
	if got, _ := g.Rules(ctx, two); len(got) != 0 {
		t.Fatalf("revoked grant still delivered: %v", got)
	}
	if _, err := s.RevokeGrant(ctx, connect.NewRequest(&agentfleetv1.RevokeGrantRequest{Id: list.Msg.GetGrants()[0].GetId()})); code(err) != connect.CodeNotFound {
		t.Fatalf("second revoke: %v", err)
	}

	// Refusals: a deny, a repo session, a seq that is not a request, a
	// prefix the request does not offer.
	todo := request(two, `{"tool":"TodoWrite","input":{}}`)
	if err := respond(two, todo, `{"behavior":"deny"}`, grants.ScopeSession, ""); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("deny with scope: %v", err)
	}
	if err := respond(two, todo, `{"behavior":"allow"}`, grants.ScopeSession, grants.FormPrefix); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("prefix not offered: %v", err)
	}
	if err := respond(two, todo+5, `{"behavior":"allow"}`, grants.ScopeSession, ""); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("not a request: %v", err)
	}
	// A question is answered through AnswerQuestion, never here.
	q, err := transcr.Append(ctx, two, "agent", `{"questions":[]}`, "question", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := respond(two, q, `{"behavior":"allow"}`, "", ""); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("a question through RespondToPermission: %v", err)
	}
	repoReq := request(repo, `{"tool":"Bash","input":{"command":"ls"}}`)
	if err := respond(repo, repoReq, `{"behavior":"allow"}`, grants.ScopeSession, ""); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("repo session: %v", err)
	}
	// "once" is today's answer, for any session, and only core writes
	// "grant": an answer that names its own loses it.
	if err := respond(repo, repoReq, `{"behavior":"allow","grant":"Bash"}`, grants.ScopeOnce, ""); err != nil {
		t.Fatalf("once: %v", err)
	}
	if e, _, _ := transcr.ReadSince(ctx, repo, repoReq+1, 1); len(e) != 1 || e[0].Text != `{"behavior":"allow"}` {
		t.Fatalf("once answer kept an answerer grant: %v", e)
	}

	// A prompt already answered remembers nothing: no grant is stored.
	late := request(two, `{"tool":"Bash","input":{"command":"make"}}`)
	if err := respond(two, late, `{"behavior":"deny"}`, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := respond(two, late, `{"behavior":"allow"}`, grants.ScopeDefinition, ""); code(err) != connect.CodeAlreadyExists {
		t.Fatalf("answered prompt: %v", err)
	}
	if got, _ := g.Rules(ctx, two); slices.Contains(got, "Bash(make)") {
		t.Fatalf("grant stored for an answered prompt: %v", got)
	}
}
