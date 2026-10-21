//go:build integration

package agentsvc

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// noWarm is the console without its provisioner: the warm is recorded, not
// run, and archiving skips the pod teardown.
type noWarm struct {
	*dashboard.Server
	store *sessions.Store
	warms []string
}

func (n *noWarm) ArchiveSession(ctx context.Context, req *connect.Request[agentfleetv1.ArchiveSessionRequest]) (*connect.Response[agentfleetv1.ArchiveSessionResponse], error) {
	return connect.NewResponse(&agentfleetv1.ArchiveSessionResponse{}), n.store.Archive(ctx, req.Msg.GetSessionId())
}

func (n *noWarm) WarmIfIdle(_ context.Context, id string) (string, error) {
	n.warms = append(n.warms, id)
	return "pod", nil
}

// docs/adr/0066 §6-7 over real Postgres: claims, retries, wake and actors,
// context-only follow-ups, taint, and CloseThread.
func TestOpen(t *testing.T) {
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
	spec := &definitions.Spec{
		Invokers:  []string{"bot"},
		Workspace: &definitions.Workspace{Kind: "repo", Repo: "notes"},
		Model:     &definitions.Model{Default: "claude-opus-5", Allow: []string{"claude-opus-5"}},
		Mode:      &definitions.Mode{Default: "auto", Allow: []string{"auto"}},
	}
	testClients = []definitions.Client{
		{Name: "bot", Spec: definitions.ClientSpec{Actors: definitions.AllowList{Allow: []string{"MohammadBnei"}}, Definitions: []string{"d"}}},
		{Name: "other", Spec: definitions.ClientSpec{Actors: definitions.AllowList{Allow: []string{"MohammadBnei"}}}},
	}
	t.Cleanup(func() { testClients = nil })
	dash := &noWarm{store: store, Server: dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test")}
	s := &Server{definitions: fakeHeads{{Name: "d", SHA: "sha1", Effective: spec}}, dash: dash, sessions: store, transcr: transcr, hub: dashboard.NewHub()}
	bot := withPrincipal(ctx, Principal{Client: "bot"})
	other := withPrincipal(ctx, Principal{Client: "other"})

	open := func(c context.Context, key, msg string, wake, tainted bool, actor string) (*agentfleetv1.OpenResponse, error) {
		r, err := s.Open(c, connect.NewRequest(&agentfleetv1.OpenRequest{
			Definition: "d", ThreadKey: "pr-1", Message: msg, Wake: wake, Tainted: tainted,
			Interaction: "ask", Lifecycle: "resident", Visibility: "private", IdempotencyKey: key, Actor: actor,
		}))
		if err != nil {
			return nil, err
		}
		return r.Msg, nil
	}
	code := connect.CodeOf

	if _, err := open(bot, "k0", "hi", false, false, ""); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("no thread and no wake: %v", err)
	}
	if _, err := open(bot, "k0b", "hi", true, false, "stranger"); code(err) != connect.CodePermissionDenied {
		t.Fatalf("actor outside actors.allow: %v", err)
	}
	if _, err := open(other, "k0c", "hi", true, false, "MohammadBnei"); code(err) != connect.CodeNotFound {
		t.Fatalf("client that may not open d: %v", err)
	}
	first, err := open(bot, "k1", "review this", true, false, "MohammadBnei")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if len(dash.warms) != 1 {
		t.Fatalf("warms after a waking open: %v", dash.warms)
	}
	// A retry of a delivered request starts nothing and answers the same.
	again, err := open(bot, "k1", "review this", true, false, "MohammadBnei")
	if err != nil || again.GetThreadId() != first.GetThreadId() || again.GetSeq() != first.GetSeq() || len(dash.warms) != 1 {
		t.Fatalf("retry: %v %v warms=%v", again, err, dash.warms)
	}
	if _, err := open(bot, "k1", "something else", true, false, "MohammadBnei"); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("same key, different request: %v", err)
	}
	// A non-waking follow-up lands context-only on the same thread.
	follow, err := open(bot, "k2", "a comment", false, false, "")
	if err != nil || follow.GetThreadId() != first.GetThreadId() || len(dash.warms) != 1 {
		t.Fatalf("follow-up: %v %v", follow, err)
	}
	// A tainted follow-up never wakes, whatever wake says.
	if _, err := open(bot, "k3", "from a fork", true, true, "MohammadBnei"); err != nil || len(dash.warms) != 1 {
		t.Fatalf("tainted follow-up: %v warms=%v", err, dash.warms)
	}
	entries, _, _ := transcr.ReadSince(ctx, first.GetThreadId(), 0, 10)
	if len(entries) != 3 || entries[0].ContextOnly || !entries[1].ContextOnly || !entries[2].ContextOnly || !entries[2].Untrusted || entries[0].AuthorName != "client:bot" {
		t.Fatalf("entries: %+v", entries)
	}
	// The thread is the client's, and only its.
	if _, err := s.GetTranscript(other, connect.NewRequest(&agentfleetv1.AgentServiceGetTranscriptRequest{ThreadId: first.GetThreadId()})); code(err) != connect.CodeNotFound {
		t.Errorf("another client reads it: %v", err)
	}
	// A different visibility on resume is refused.
	if _, err := s.Open(bot, connect.NewRequest(&agentfleetv1.OpenRequest{Definition: "d", ThreadKey: "pr-1", Message: "x", Wake: true,
		Interaction: "ask", Lifecycle: "resident", Visibility: "shared", IdempotencyKey: "k4", Actor: "MohammadBnei"})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("visibility change on resume: %v", err)
	}
	// The definition moved on. A tainted waking message neither ends the
	// thread nor opens a new one: it lands context-only on the old thread.
	s.definitions = fakeHeads{{Name: "d", SHA: "sha2", Effective: spec}}
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES ('d', 'sha2', '{}', '{}', true)`); err != nil {
		t.Fatal(err)
	}
	warms := len(dash.warms)
	if r, err := open(bot, "k4b", "untrusted push", true, true, "MohammadBnei"); err != nil || r.GetThreadId() != first.GetThreadId() || len(dash.warms) != warms {
		t.Fatalf("tainted wake on a stale thread: %v %v warms=%v", r, err, dash.warms)
	}
	// A trusted waking Open replaces it.
	next, err := open(bot, "k5", "new push", true, false, "MohammadBnei")
	if err != nil || next.GetThreadId() == first.GetThreadId() {
		t.Fatalf("replace: %v %v", next, err)
	}
	if old, _ := store.Get(ctx, first.GetThreadId()); old.ArchivedAt == nil {
		t.Error("the replaced thread is still open")
	}
	if _, err := s.CloseThread(bot, connect.NewRequest(&agentfleetv1.CloseThreadRequest{Definition: "d", ThreadKey: "pr-1"})); err != nil {
		t.Fatalf("CloseThread: %v", err)
	}
	if _, err := s.CloseThread(bot, connect.NewRequest(&agentfleetv1.CloseThreadRequest{Definition: "d", ThreadKey: "pr-1"})); code(err) != connect.CodeNotFound {
		t.Errorf("CloseThread twice: %v", err)
	}
	// A done claim whose session was deleted answers from the claim and
	// delivers nothing again.
	if _, err := pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, first.GetThreadId()); err != nil {
		t.Fatal(err)
	}
	if _, err := open(bot, "k1", "review this", true, false, "MohammadBnei"); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("done claim, deleted session: %v", err)
	}
	// A busy key answers Aborted at once.
	lock, err := store.TryLock(ctx, "req\x00client:bot\x00k9")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(bot, "k9", "x", true, false, "MohammadBnei"); code(err) != connect.CodeAborted {
		t.Errorf("busy key: %v", err)
	}
	lock.Release()
}

// docs/adr/0066 §5: a client acts on its threads only while it and the
// definition still list each other; reading stays open.
func TestOpen_DelistedClient(t *testing.T) {
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
	spec := &definitions.Spec{
		Invokers: []string{"bot"}, Workspace: &definitions.Workspace{Kind: "repo", Repo: "notes"},
		Model: &definitions.Model{Default: "claude-opus-5", Allow: []string{"claude-opus-5"}},
		Mode:  &definitions.Mode{Default: "auto", Allow: []string{"auto"}},
	}
	testClients = []definitions.Client{{Name: "bot", Spec: definitions.ClientSpec{Actors: definitions.AllowList{Allow: []string{"MohammadBnei"}}, Definitions: []string{"d"}}}}
	t.Cleanup(func() { testClients = nil })
	dash := &noWarm{store: store, Server: dashboard.NewServer(store, nil, transcr, nil, repos.NewStore(pool), nil, nil, nil, sessions.Caps{MaxLive: 5}, nil, nil, nil, "test")}
	s := &Server{definitions: fakeHeads{{Name: "d", SHA: "sha1", Effective: spec}}, dash: dash, sessions: store, transcr: transcr, hub: dashboard.NewHub()}
	bot := withPrincipal(ctx, Principal{Client: "bot"})
	r, err := s.Open(bot, connect.NewRequest(&agentfleetv1.OpenRequest{Definition: "d", ThreadKey: "t", Message: "x", Wake: true,
		Interaction: "ask", Lifecycle: "resident", Visibility: "private", IdempotencyKey: "k", Actor: "MohammadBnei"}))
	if err != nil {
		t.Fatal(err)
	}
	id := r.Msg.GetThreadId()
	req, err := transcr.Append(ctx, id, "agent", `{"tool":"Bash","input":{"command":"ls"}}`, "permission_request", "")
	if err != nil {
		t.Fatal(err)
	}
	testClients = nil // de-listed on the next sync
	if _, err := s.RespondToPermission(bot, connect.NewRequest(&agentfleetv1.AgentServiceRespondToPermissionRequest{ThreadId: id, Seq: req, Behavior: "allow"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("de-listed client answers: %v", err)
	}
	if _, err := s.GetTranscript(bot, connect.NewRequest(&agentfleetv1.AgentServiceGetTranscriptRequest{ThreadId: id})); err != nil {
		t.Errorf("de-listed client reads: %v", err)
	}
}
