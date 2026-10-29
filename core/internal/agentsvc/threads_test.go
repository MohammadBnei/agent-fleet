//go:build integration

package agentsvc

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// docs/adr/0067 §2 over real Postgres: who sees, lists and may post to a
// thread. Warming is out of scope here (no provisioner), so PostMessage is
// checked only up to its refusals.
func TestThreads_Visibility(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool)
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
	heads := fakeHeads{{Name: "d", SHA: "sha1", Effective: spec}}
	dash := dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test", "")
	s := &Server{definitions: heads, dash: dash, sessions: store, transcr: transcr, hub: dashboard.NewHub()}

	as := func(sub string, groups ...string) context.Context {
		id := auth.Identity{Sub: sub, Email: sub + "@bnei.dev", Groups: groups}
		return withPrincipal(ctx, Principal{Member: &id})
	}
	alice, bob, eve, admin := as("alice", "shared-notes"), as("bob", "shared-notes"), as("eve", "other"), as("root", auth.RequiredGroup)

	create := func(c context.Context, vis string) (string, error) {
		r, err := s.CreateThread(c, connect.NewRequest(&agentfleetv1.CreateThreadRequest{Definition: "d", Visibility: vis, Title: vis}))
		if err != nil {
			return "", err
		}
		return r.Msg.GetThread().GetId(), nil
	}
	priv, err := create(alice, "private")
	if err != nil {
		t.Fatalf("alice private: %v", err)
	}
	shared, err := create(alice, "shared")
	if err != nil {
		t.Fatalf("alice shared: %v", err)
	}
	if _, err := create(eve, "shared"); connect.CodeOf(err) != connect.CodeNotFound || errkeys.Of(err) != errkeys.KeyDefinitionUnavailable {
		t.Fatalf("eve outside the audience: %v", err)
	}
	if _, err := create(alice, "public"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad visibility: %v", err)
	}
	// A console session of the same definition: owner-less, admin-only.
	console, err := store.Create(ctx, sessions.CreateParams{Repo: "notes", DefinitionName: "d", DefinitionSHA: "sha1"})
	if err != nil {
		t.Fatal(err)
	}

	list := func(c context.Context) map[string]bool {
		r, err := s.ListThreads(c, connect.NewRequest(&agentfleetv1.ListThreadsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, th := range r.Msg.GetThreads() {
			out[th.GetId()] = th.GetMine()
		}
		return out
	}
	if l := list(alice); len(l) != 2 || !l[priv] || !l[shared] {
		t.Errorf("alice lists %v", l)
	}
	if l := list(bob); len(l) != 1 || l[shared] {
		t.Errorf("bob lists %v (want only the shared one, not his)", l)
	}
	if l := list(eve); len(l) != 0 {
		t.Errorf("eve lists %v", l)
	}
	if l := list(admin); len(l) != 3 {
		t.Errorf("admin lists %v (want all three, the console one too)", l)
	}

	read := func(c context.Context, id string) error {
		_, err := s.GetTranscript(c, connect.NewRequest(&agentfleetv1.AgentServiceGetTranscriptRequest{ThreadId: id}))
		return err
	}
	for _, c := range []struct {
		name string
		ctx  context.Context
		id   string
		want connect.Code
	}{
		{"bob reads alice's private", bob, priv, connect.CodeNotFound},
		{"bob reads shared", bob, shared, 0},
		{"eve reads shared", eve, shared, connect.CodeNotFound},
		{"alice reads console", alice, console, connect.CodeNotFound},
		{"admin reads console", admin, console, 0},
		{"random id", alice, "00000000-0000-0000-0000-000000000000", connect.CodeNotFound},
		{"not a uuid", alice, "x", connect.CodeNotFound},
	} {
		if got := connect.CodeOf(read(c.ctx, c.id)); got != c.want && !(c.want == 0 && got == connect.CodeUnknown && read(c.ctx, c.id) == nil) {
			t.Errorf("%s: %v, want %v", c.name, read(c.ctx, c.id), c.want)
		}
	}

	post := func(c context.Context, id string) error {
		_, err := s.PostMessage(c, connect.NewRequest(&agentfleetv1.AgentServicePostMessageRequest{ThreadId: id, Text: "hi"}))
		return err
	}
	if err := post(bob, priv); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("bob posts to alice's private: %v", err)
	}
	// A tainted shared thread is admins' only.
	if _, err := pool.Exec(ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, context_only, untrusted)
		VALUES ($1, 0, 'human', 'event', 'discussion', 'e1', true, true)`, shared); err != nil {
		t.Fatal(err)
	}
	if err := post(bob, shared); connect.CodeOf(err) != connect.CodePermissionDenied || errkeys.Of(err) != errkeys.KeyThreadUntrusted {
		t.Errorf("bob posts to tainted shared: %v", err)
	}
	// The definition moved on: the thread is not silently carried over.
	s.definitions = fakeHeads{{Name: "d", SHA: "sha2", Effective: spec}}
	if err := post(alice, priv); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("definition changed: %v", err)
	}
}

// AppendAuthored records who wrote an entry, and ReadSince gives it back.
func TestThreads_Authorship(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	transcr := transcript.NewPostgresStore(pool)
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO sessions (repo, description) VALUES ('r', 'd') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := transcr.AppendAuthored(ctx, id, transcript.Author{Sub: "alice", Name: "alice@bnei.dev"}, "hi", "discussion", "", nil); err != nil {
		t.Fatal(err)
	}
	var sub string
	if err := pool.QueryRow(ctx, `SELECT author_sub FROM transcript WHERE session_id = $1`, id).Scan(&sub); err != nil || sub != "alice" {
		t.Fatalf("author_sub = %q, %v", sub, err)
	}
	e, _, err := transcr.ReadSince(ctx, id, 0, 10)
	if err != nil || len(e) != 1 || e[0].AuthorName != "alice@bnei.dev" || e[0].From != "human" {
		t.Fatalf("ReadSince = %+v, %v", e, err)
	}
}
