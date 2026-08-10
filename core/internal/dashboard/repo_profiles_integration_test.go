//go:build integration

package dashboard

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/repoprofiles"
)

// TestServer_RepoProfileCRUD exercises the full RPC->store->proto round
// trip for docs/adr/0034's dashboard-editable environment recipes — same
// Postgres-backed setup as TestServer_CreateTask_* above, since these RPCs
// go straight to repoprofiles.Store, a concrete Postgres store, not a nil
// fake.
func TestServer_RepoProfileCRUD(t *testing.T) {
	pool := newTestPool(t)
	profileStore := repoprofiles.NewStore(pool)
	s := NewServer(nil, nil, nil, nil, profileStore, nil, nil, nil, nil, 5, nil)
	ctx := context.Background()

	// "agent-fleet" is one of the three repos seeded by db/migrations/
	// (docs/adr/0030) — repo_profiles.repo_name FKs to repos(name).
	createReq := connect.NewRequest(&agentfleetv1.CreateRepoProfileRequest{
		RepoName: "agent-fleet",
		Name:     "test-profile",
		StartCmd: "bun run dev",
		ToolKeys: []string{"go-toolchain", "buf"},
		ServiceIngredients: []*agentfleetv1.ServiceIngredient{
			{Key: "postgres", ScopeMode: agentfleetv1.ScopeMode_SCOPE_MODE_TASK_SCOPED},
		},
	})
	createResp, err := s.CreateRepoProfile(ctx, createReq)
	if err != nil {
		t.Fatalf("CreateRepoProfile: %v", err)
	}
	got := createResp.Msg.GetProfile()
	if got.GetRepoName() != "agent-fleet" || got.GetName() != "test-profile" || got.GetStartCmd() != "bun run dev" {
		t.Errorf("CreateRepoProfile response = %+v", got)
	}
	if len(got.GetToolKeys()) != 2 {
		t.Errorf("CreateRepoProfile tool_keys = %v, want 2 entries", got.GetToolKeys())
	}
	if len(got.GetServiceIngredients()) != 1 || got.GetServiceIngredients()[0].GetScopeMode() != agentfleetv1.ScopeMode_SCOPE_MODE_TASK_SCOPED {
		t.Errorf("CreateRepoProfile service_ingredients = %+v", got.GetServiceIngredients())
	}

	// Duplicate create -> AlreadyExists.
	if _, err := s.CreateRepoProfile(ctx, createReq); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("duplicate CreateRepoProfile error = %v, want CodeAlreadyExists", err)
	}

	// List includes it.
	listResp, err := s.ListRepoProfiles(ctx, connect.NewRequest(&agentfleetv1.ListRepoProfilesRequest{RepoName: "agent-fleet"}))
	if err != nil {
		t.Fatalf("ListRepoProfiles: %v", err)
	}
	found := false
	for _, p := range listResp.Msg.GetProfiles() {
		if p.GetName() == "test-profile" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListRepoProfiles = %+v, want it to contain test-profile", listResp.Msg.GetProfiles())
	}

	// Update replaces ingredients wholesale.
	updateReq := connect.NewRequest(&agentfleetv1.UpdateRepoProfileRequest{
		RepoName: "agent-fleet",
		Name:     "test-profile",
		StartCmd: "bun run build",
		ToolKeys: []string{"golangci-lint"},
		ServiceIngredients: []*agentfleetv1.ServiceIngredient{
			{Key: "redis", ScopeMode: agentfleetv1.ScopeMode_SCOPE_MODE_POD_SCOPED},
		},
	})
	updateResp, err := s.UpdateRepoProfile(ctx, updateReq)
	if err != nil {
		t.Fatalf("UpdateRepoProfile: %v", err)
	}
	got = updateResp.Msg.GetProfile()
	if got.GetStartCmd() != "bun run build" || len(got.GetToolKeys()) != 1 || got.GetToolKeys()[0] != "golangci-lint" {
		t.Errorf("UpdateRepoProfile response = %+v", got)
	}
	if len(got.GetServiceIngredients()) != 1 || got.GetServiceIngredients()[0].GetKey() != "redis" || got.GetServiceIngredients()[0].GetScopeMode() != agentfleetv1.ScopeMode_SCOPE_MODE_POD_SCOPED {
		t.Errorf("UpdateRepoProfile service_ingredients = %+v", got.GetServiceIngredients())
	}

	// Update of an unknown profile -> NotFound.
	unknownReq := connect.NewRequest(&agentfleetv1.UpdateRepoProfileRequest{RepoName: "agent-fleet", Name: "nope"})
	if _, err := s.UpdateRepoProfile(ctx, unknownReq); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("UpdateRepoProfile unknown error = %v, want CodeNotFound", err)
	}

	// Delete, then confirm it's gone.
	deleteResp, err := s.DeleteRepoProfile(ctx, connect.NewRequest(&agentfleetv1.DeleteRepoProfileRequest{RepoName: "agent-fleet", Name: "test-profile"}))
	if err != nil {
		t.Fatalf("DeleteRepoProfile: %v", err)
	}
	if deleteResp.Msg.GetStatus() != "deleted" {
		t.Errorf("DeleteRepoProfile status = %q, want %q", deleteResp.Msg.GetStatus(), "deleted")
	}
	if _, err := s.DeleteRepoProfile(ctx, connect.NewRequest(&agentfleetv1.DeleteRepoProfileRequest{RepoName: "agent-fleet", Name: "test-profile"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("second DeleteRepoProfile error = %v, want CodeNotFound", err)
	}
}

func TestServer_CreateRepoProfile_MissingFields(t *testing.T) {
	pool := newTestPool(t)
	profileStore := repoprofiles.NewStore(pool)
	s := NewServer(nil, nil, nil, nil, profileStore, nil, nil, nil, nil, 5, nil)

	req := connect.NewRequest(&agentfleetv1.CreateRepoProfileRequest{RepoName: "", Name: "x"})
	if _, err := s.CreateRepoProfile(context.Background(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("CreateRepoProfile error = %v, want CodeInvalidArgument", err)
	}
}
