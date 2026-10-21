//go:build integration

package agentsvc

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
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
	dash := dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test")
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
	// docs/adr/0067 §6: members start no threads, so alice's are made as an
	// admin would leave them: owned by her.
	if _, err := create(alice, "shared"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("a member starts a thread: %v", err)
	}
	if _, err := create(admin, "private"); err != nil {
		t.Fatalf("admin starts a thread: %v", err)
	}
	own := func(vis string) string {
		id, err := store.Create(ctx, sessions.CreateParams{Repo: "notes", DefinitionName: "d", DefinitionSHA: "sha1",
			Owner: "alice", Visibility: vis, Interaction: "ask", Lifecycle: "resident"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	priv, shared := own("private"), own("shared")
	if _, err := create(admin, "public"); connect.CodeOf(err) != connect.CodeInvalidArgument {
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
	if l := list(admin); len(l) != 4 {
		t.Errorf("admin lists %v (want all four: alice's two, its own, the console one)", l)
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
	if err := post(bob, shared); connect.CodeOf(err) != connect.CodePermissionDenied {
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

// docs/adr/0067 §6 over real Postgres: a steward locks from an island, a
// member is then refused; past the wake window a member cannot boot the pod
// and only a steward may. Warming itself needs a provisioner, so a steward's
// Wake and post are checked only up to their refusals.
func TestThreads_LockAndWake(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('notes', 'https://example.invalid/notes.git')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('d', 'sha1', '{}', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	island := "https://notes.bnei.dev"
	spec := &definitions.Spec{
		Audience:  &definitions.Audience{Group: "shared-notes"},
		Workspace: &definitions.Workspace{Kind: "repo", Repo: "notes"},
		Embed:     &definitions.Embed{Origins: []string{island}},
	}
	dash := dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test")
	s := &Server{definitions: fakeHeads{{Name: "d", SHA: "sha1", Effective: spec}}, dash: dash, sessions: store, transcr: transcr, hub: dashboard.NewHub()}
	as := func(sub string, groups ...string) context.Context {
		id := auth.Identity{Sub: sub, Email: sub + "@bnei.dev", Groups: groups}
		return withPrincipal(ctx, Principal{Member: &id, Origin: island})
	}
	bob, steward := as("bob", "shared-notes"), as("root", "shared-notes", auth.RequiredGroup)
	code := connect.CodeOf

	id, err := store.Create(ctx, sessions.CreateParams{Repo: "notes", DefinitionName: "d", DefinitionSHA: "sha1",
		Owner: "bob", Visibility: "shared", Interaction: "ask", Lifecycle: "resident"})
	if err != nil {
		t.Fatal(err)
	}
	lock := func(c context.Context, locked bool) error {
		_, err := s.Lock(c, connect.NewRequest(&agentfleetv1.LockRequest{ThreadId: id, Locked: locked}))
		return err
	}
	post := func(c context.Context) error {
		_, err := s.PostMessage(c, connect.NewRequest(&agentfleetv1.AgentServicePostMessageRequest{ThreadId: id, Text: "hi"}))
		return err
	}
	listed := func(c context.Context) (*agentfleetv1.ListThreadsResponse, *agentfleetv1.Thread) {
		r, err := s.ListThreads(c, connect.NewRequest(&agentfleetv1.ListThreadsRequest{}))
		if err != nil || len(r.Msg.GetThreads()) != 1 {
			t.Fatalf("list: %v, %v", r, err)
		}
		return r.Msg, r.Msg.GetThreads()[0]
	}

	if err := lock(bob, true); code(err) != connect.CodePermissionDenied {
		t.Errorf("the owner, not a steward, locks: %v", err)
	}
	if err := lock(steward, true); err != nil {
		t.Fatalf("steward locks from an island: %v", err)
	}
	if r, th := listed(bob); r.GetSteward() || th.GetLockedBy() != "root@bnei.dev" {
		t.Errorf("bob's list: steward=%v locked_by=%q", r.GetSteward(), th.GetLockedBy())
	}
	if r, _ := listed(steward); !r.GetSteward() {
		t.Error("a steward's list does not say so")
	}
	if err := post(bob); code(err) != connect.CodePermissionDenied {
		t.Errorf("bob posts to a locked thread: %v", err)
	}
	if err := lock(steward, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if _, th := listed(bob); th.GetLockedBy() != "" {
		t.Errorf("still locked by %q", th.GetLockedBy())
	}

	if _, err := pool.Exec(ctx, `UPDATE sessions SET last_active_at = now() - interval '25 hours' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := post(bob); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("bob wakes a thread idle 25h: %v", err)
	}
	if _, err := s.Wake(bob, connect.NewRequest(&agentfleetv1.WakeRequest{ThreadId: id})); code(err) != connect.CodePermissionDenied {
		t.Errorf("bob calls Wake: %v", err)
	}
}
