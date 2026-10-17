//go:build integration

package dashboard

import (
	"context"
	"net"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/grpc"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/proposals"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// docs/adr/0061. A proposal-opened session used to store no model at all, so
// the worker ran whatever the CLI defaulted to — CreateSession's default never
// reached it. Asserts the row, not the call.
func TestOpenFromProposal_StoresDefaultModelAndAutoMode(t *testing.T) {
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
	provClient, err := provisionerclient.New(lis.Addr().String())
	if err != nil {
		t.Fatalf("provisioner client: %v", err)
	}
	t.Cleanup(func() { _ = provClient.Close() })

	sessionStore := sessions.NewStore(pool)
	repoStore := repos.NewStore(pool)
	propStore := proposals.NewStore(pool)
	s := &Server{
		sessions: sessionStore, repos: repoStore, proposals: propStore,
		transcr: transcript.NewPostgresStore(pool), e2e: provClient, maxLive: 5,
	}

	if err := repoStore.Create(ctx, repos.Repo{Name: "editable-blog", URL: "https://example.invalid/x.git"}); err != nil {
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
	if got.Model == nil || *got.Model != defaultModel {
		t.Errorf("model = %v, want %q", got.Model, defaultModel)
	}
	if got.PermissionMode == nil || *got.PermissionMode != "auto" {
		t.Errorf("permission mode = %v, want auto", got.PermissionMode)
	}
}
