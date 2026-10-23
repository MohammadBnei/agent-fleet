//go:build integration

package dashboard

import (
	"context"
	"net"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/grpc"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/proposals"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// docs/adr/0061, amended by docs/adr/0077: with no CLAUDE_MODEL a
// proposal-opened session stores no model, the worker passes none, and Claude
// Code picks — the same as a dashboard-created one. Asserts the row, not the call.
func TestOpenFromProposal_StoresNoModelAndAutoMode(t *testing.T) {
	t.Setenv("CLAUDE_MODEL", "")
	ctx := context.Background()
	pool := dbtest.NewPool(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	gs := grpc.NewServer()
	agentfleetv1.RegisterProvisionerServiceServer(gs, &recordingProvisioner{})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	provClient, err := provisionerclient.New(lis.Addr().String(), "")
	if err != nil {
		t.Fatalf("provisioner client: %v", err)
	}
	t.Cleanup(func() { _ = provClient.Close() })

	sessionStore := sessions.NewStore(pool)
	repoStore := repos.NewStore(pool)
	propStore := proposals.NewStore(pool)
	s := &Server{
		sessions: sessionStore, repos: repoStore, proposals: propStore,
		transcr: transcript.NewPostgresStore(pool), e2e: provClient, caps: sessions.Caps{MaxLive: 5},
	}

	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('editable-blog', 'https://example.invalid/x.git')`); err != nil {
		t.Fatalf("repo: %v", err)
	}
	pid, _, err := propStore.Create(ctx, "editable-blog", "audit", "k-1", "fix the thing", "body", "")
	if err != nil {
		t.Fatalf("proposal: %v", err)
	}

	res, err := s.OpenFromProposal(ctx, connect.NewRequest(&agentfleetv1.OpenFromProposalRequest{ProposalId: pid}))
	if err != nil {
		t.Fatalf("OpenFromProposal: %v", err)
	}
	got, err := sessionStore.Get(ctx, res.Msg.GetSession().GetId())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Model != nil {
		t.Errorf("model = %q, want NULL (Claude Code's default)", *got.Model)
	}
	if got.PermissionMode == nil || *got.PermissionMode != "auto" {
		t.Errorf("permission mode = %v, want auto", got.PermissionMode)
	}
}

// docs/adr/0078 §5: an event proposal opens its definition's thread as the
// clicking admin, through the injected opener, and only once.
func TestOpenFromProposal_EventProposal(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	sessionStore := sessions.NewStore(pool)
	propStore := proposals.NewStore(pool)
	s := &Server{sessions: sessionStore, proposals: propStore}
	pid, _, err := propStore.CreateEvent(ctx, "notes", "event:d:t", "title", "body", "", proposals.Event{Definition: "d", ThreadKey: "t"})
	if err != nil {
		t.Fatal(err)
	}
	req := connect.NewRequest(&agentfleetv1.OpenFromProposalRequest{ProposalId: pid})
	if _, err := s.OpenFromProposal(auth.WithIdentity(ctx, auth.Identity{Sub: "sub-1"}), req); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("no opener: %v", err)
	}
	var thread string
	if err := pool.QueryRow(ctx, `INSERT INTO sessions (repo) VALUES ('notes') RETURNING id`).Scan(&thread); err != nil {
		t.Fatal(err)
	}
	var subs []string
	s.SetEventOpener(func(_ context.Context, p *proposals.Proposal, sub string) (string, error) {
		subs = append(subs, sub+" "+p.Event.Definition+" "+p.Body)
		return thread, nil
	})
	if _, err := s.OpenFromProposal(ctx, req); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("no identity: %v", err)
	}
	res, err := s.OpenFromProposal(auth.WithIdentity(ctx, auth.Identity{Sub: "sub-1"}), req)
	if err != nil || res.Msg.GetSession().GetId() != thread || len(subs) != 1 || subs[0] != "sub-1 d body" {
		t.Fatalf("open: %v %v %v", res, err, subs)
	}
	if _, err := s.OpenFromProposal(auth.WithIdentity(ctx, auth.Identity{Sub: "sub-2"}), req); connect.CodeOf(err) != connect.CodeFailedPrecondition || len(subs) != 1 {
		t.Fatalf("second click: %v %v", err, subs)
	}
}

// An event folded in while the admin's open is in flight is delivered too:
// the proposal is marked only once the delivered body is the current one.
func TestOpenFromProposal_EventFoldedDuringOpen(t *testing.T) {
	ctx := auth.WithIdentity(context.Background(), auth.Identity{Sub: "sub-1"})
	pool := dbtest.NewPool(t)
	propStore := proposals.NewStore(pool)
	s := &Server{sessions: sessions.NewStore(pool), proposals: propStore}
	pid, _, err := propStore.CreateEvent(ctx, "notes", "event:d:t", "title", "[e1] first", "", proposals.Event{Definition: "d", ThreadKey: "t"})
	if err != nil {
		t.Fatal(err)
	}
	var thread string
	if err := pool.QueryRow(ctx, `INSERT INTO sessions (repo) VALUES ('notes') RETURNING id`).Scan(&thread); err != nil {
		t.Fatal(err)
	}
	var bodies []string
	s.SetEventOpener(func(ctx context.Context, p *proposals.Proposal, _ string) (string, error) {
		bodies = append(bodies, p.Body)
		if len(bodies) == 1 {
			if _, err := propStore.FoldEvent(ctx, "notes", "event:d:t", "[e2]", "[e2] second", false); err != nil {
				return "", err
			}
		}
		return thread, nil
	})
	if _, err := s.OpenFromProposal(ctx, connect.NewRequest(&agentfleetv1.OpenFromProposalRequest{ProposalId: pid})); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[1] != "[e1] first\n\n[e2] second" {
		t.Fatalf("delivered %q", bodies)
	}
	if p, _ := propStore.Get(ctx, pid); p.SessionID == nil || *p.SessionID != thread {
		t.Fatalf("proposal not opened: %+v", p)
	}
}
