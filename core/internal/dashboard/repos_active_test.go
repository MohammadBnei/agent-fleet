//go:build integration

package dashboard

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/proposals"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// An inactive repo starts no new session from any door, and SyncRepo leaves
// its cache alone. e2e is nil: reaching the provisioner is itself the bug.
func TestReposActive(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	propStore := proposals.NewStore(pool)
	s := &Server{sessions: sessions.NewStore(pool), repos: repos.NewStore(pool), proposals: propStore}
	if _, err := pool.Exec(ctx, `INSERT INTO repos (name, url) VALUES ('blog', 'https://example.invalid/blog.git'), ('notes', 'https://example.invalid/notes.git')`); err != nil {
		t.Fatal(err)
	}

	res, err := s.SetReposActive(ctx, connect.NewRequest(&agentfleetv1.SetReposActiveRequest{Names: []string{"blog", "nope"}, Active: false}))
	if err != nil || res.Msg.GetUpdated() != 1 {
		t.Fatalf("SetReposActive = %v (%v), want 1 updated", res, err)
	}

	_, err = s.CreateSession(ctx, connect.NewRequest(&agentfleetv1.CreateSessionRequest{Repo: "blog"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("CreateSession on inactive: %v (%v), want FailedPrecondition", connect.CodeOf(err), err)
	}
	if _, err := s.CreateSession(ctx, connect.NewRequest(&agentfleetv1.CreateSessionRequest{Repo: "notes"})); err != nil {
		t.Fatalf("CreateSession on active: %v", err)
	}

	pid, _, err := propStore.Create(ctx, "blog", "audit", "k-1", "fix it", "body", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.OpenFromProposal(ctx, connect.NewRequest(&agentfleetv1.OpenFromProposalRequest{ProposalId: pid}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("OpenFromProposal on inactive: %v (%v), want FailedPrecondition", connect.CodeOf(err), err)
	}
	if p, _ := propStore.Get(ctx, pid); p == nil || p.SessionID != nil {
		t.Fatalf("refused proposal = %+v, want still open", p)
	}

	_, err = s.SyncRepo(ctx, connect.NewRequest(&agentfleetv1.SyncRepoRequest{Name: "blog"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("SyncRepo on inactive: %v (%v), want FailedPrecondition", connect.CodeOf(err), err)
	}

	list, err := s.ListRepos(ctx, connect.NewRequest(&agentfleetv1.ListReposRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var blog *agentfleetv1.Repo
	for _, r := range list.Msg.GetRepos() {
		if r.GetName() == "blog" {
			blog = r
		}
	}
	if blog == nil || blog.GetActive() || blog.GetCreatedAt() == "" {
		t.Fatalf("ListRepos blog = %v, want listed, inactive, with created_at", blog)
	}
}
