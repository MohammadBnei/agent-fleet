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
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// A post to a thread whose definition changed since it opened (docs/adr/0066 §6) is FailedPrecondition, keyed
// agent.definition_changed, with the words islands read before the key (kept one release).
func TestPostMessage_DefinitionChanged(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('notes', 'https://example.invalid/notes.git')`); err != nil {
		t.Fatal(err)
	}
	spec := &definitions.Spec{
		Audience:  &definitions.Audience{Group: "shared-notes"},
		Workspace: &definitions.Workspace{Kind: "repo", Repo: "notes"},
		Model:     &definitions.Model{Default: "claude-opus-5", Allow: []string{"claude-opus-5"}},
		Mode:      &definitions.Mode{Default: "auto", Allow: []string{"auto"}},
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('d', 'sha1', '{}', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	dash := dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test", "")
	at := func(sha string) *Server {
		return &Server{definitions: fakeHeads{{Name: "d", SHA: sha, Effective: spec}}, dash: dash, sessions: store, transcr: transcr, hub: dashboard.NewHub()}
	}
	id := auth.Identity{Sub: "alice", Email: "alice@bnei.dev", Groups: []string{"shared-notes"}}
	alice := withPrincipal(ctx, Principal{Member: &id})
	r, err := at("sha1").CreateThread(alice, connect.NewRequest(&agentfleetv1.CreateThreadRequest{Definition: "d", Visibility: "private", Title: "t"}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = at("sha2").PostMessage(alice, connect.NewRequest(&agentfleetv1.AgentServicePostMessageRequest{ThreadId: r.Msg.GetThread().GetId(), Text: "hi"}))
	var ce *connect.Error
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || errkeys.Of(err) != errkeys.KeyDefinitionChanged ||
		!asConnect(err, &ce) || ce.Message() != "DEFINITION_CHANGED: start a new thread" {
		t.Errorf("%v: key %q", err, errkeys.Of(err))
	}
}

func asConnect(err error, ce **connect.Error) bool {
	e, ok := err.(*connect.Error)
	*ce = e
	return ok
}
