//go:build integration

package dashboard

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/packs"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// docs/adr/0063: the console's whole pack flow through DashboardService.
func TestPacksRPCs(t *testing.T) {
	ctx := context.Background()
	s, _, _, _, pool := newDefinitionsServerPool(t)
	if _, err := s.ListPacks(ctx, connect.NewRequest(&agentfleetv1.ListPacksRequest{})); code(err) != connect.CodeUnavailable {
		t.Fatalf("ListPacks without a store = %v, want Unavailable", err)
	}
	s.SetPacks(packs.NewStore(pool))
	if _, err := pool.Exec(ctx, `UPDATE repos SET github_id = 77 WHERE name = 'dream-analyst'`); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreatePack(ctx, connect.NewRequest(&agentfleetv1.CreatePackRequest{Name: "style"})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePack(ctx, connect.NewRequest(&agentfleetv1.CreatePackRequest{Name: "style"})); code(err) != connect.CodeAlreadyExists {
		t.Fatalf("duplicate CreatePack = %v", err)
	}
	if _, err := s.CreatePack(ctx, connect.NewRequest(&agentfleetv1.CreatePackRequest{Name: "fleet"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("reserved CreatePack = %v", err)
	}
	bad := &agentfleetv1.SavePackVersionRequest{Name: "style", Text: "see @/proc/self/environ"}
	if _, err := s.SavePackVersion(ctx, connect.NewRequest(bad)); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("SavePackVersion with an import = %v", err)
	}
	res, err := s.SavePackVersion(ctx, connect.NewRequest(&agentfleetv1.SavePackVersionRequest{
		Name: "style", Text: "short notes",
		Skills: []*agentfleetv1.PackSkill{{Name: "tidy", Description: "tidy a note", Body: "cut filler"}},
	}))
	if err != nil || res.Msg.GetVersion() != 1 {
		t.Fatalf("SavePackVersion = %v, %v", res, err)
	}
	for _, a := range []*agentfleetv1.PackAttachment{{Kind: "definition", Target: "dev-dream-analyst"}, {Kind: "repo", Target: "dream-analyst"}} {
		if _, err := s.AttachPack(ctx, connect.NewRequest(&agentfleetv1.AttachPackRequest{Name: "style", Attachment: a})); err != nil {
			t.Fatalf("AttachPack %v: %v", a, err)
		}
	}
	if _, err := s.AttachPack(ctx, connect.NewRequest(&agentfleetv1.AttachPackRequest{Name: "style",
		Attachment: &agentfleetv1.PackAttachment{Kind: "definition", Target: "nope"}})); code(err) != connect.CodeNotFound {
		t.Fatalf("AttachPack to an unknown definition = %v", err)
	}

	list, err := s.ListPacks(ctx, connect.NewRequest(&agentfleetv1.ListPacksRequest{}))
	if err != nil || len(list.Msg.GetPacks()) != 1 {
		t.Fatalf("ListPacks = %v, %v", list, err)
	}
	p := list.Msg.GetPacks()[0]
	if p.GetLatestVersion() != 1 || p.GetText() != "short notes" || len(p.GetSkills()) != 1 || p.GetSkills()[0].GetDescription() != "tidy a note" {
		t.Fatalf("pack = %v", p)
	}
	// A repo attachment is stored by GitHub id and listed by fleet name.
	if len(p.GetAttachments()) != 2 || p.GetAttachments()[1].GetTarget() != "dream-analyst" {
		t.Fatalf("attachments = %v", p.GetAttachments())
	}

	if _, err := s.DetachPack(ctx, connect.NewRequest(&agentfleetv1.DetachPackRequest{Name: "style",
		Attachment: &agentfleetv1.PackAttachment{Kind: "repo", Target: "dream-analyst"}})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeletePack(ctx, connect.NewRequest(&agentfleetv1.DeletePackRequest{Name: "style"})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeletePack(ctx, connect.NewRequest(&agentfleetv1.DeletePackRequest{Name: "style"})); code(err) != connect.CodeNotFound {
		t.Fatalf("second DeletePack = %v", err)
	}
}
