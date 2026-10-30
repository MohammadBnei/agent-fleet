//go:build integration

package coreserver

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// docs/adr/0082 §6: only an open definition session granting draft_card
// drafts, and the card it drafts is an open decision in its own thread.
func TestDraftCard_GrantedDefinitionsOnly(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES
		('carder', 'sha-c', '{}', '{"sidecarTools":["draft_card"]}', true),
		('quiet', 'sha-q', '{}', '{"sidecarTools":["send_message"]}', true)`); err != nil {
		t.Fatal(err)
	}
	carder, _ := store.Create(ctx, sessions.CreateParams{Repo: "r", PermissionMode: "default", DefinitionName: "carder", DefinitionSHA: "sha-c"})
	quiet, _ := store.Create(ctx, sessions.CreateParams{Repo: "r", PermissionMode: "default", DefinitionName: "quiet", DefinitionSHA: "sha-q"})
	repo, _ := store.Create(ctx, sessions.CreateParams{Repo: "r"})

	dec := decisions.NewStore(pool)
	tr := transcript.NewPostgresStore(pool, decisions.OnAppend)
	s := &Server{sessions: store, transcr: tr}
	req := func(id string) *agentfleetv1.DraftCardRequest {
		return &agentfleetv1.DraftCardRequest{SessionId: id, Ask: "Merge it?",
			Subject: &agentfleetv1.CardSubject{Repo: "o/r", Kind: "pr", Number: 7},
			Options: []*agentfleetv1.CardOption{
				{Label: "Yes", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}, Reversible: "yes"},
				{Label: "No", Effect: &agentfleetv1.CardEffect{Verb: "handoff"}, Reversible: "yes"},
			}}
	}
	if _, err := s.DraftCard(ctx, req(carder)); status.Code(err) != codes.Unavailable {
		t.Fatalf("no decisions wired: %v", err)
	}
	s.SetDecisions(dec)
	for _, id := range []string{quiet, repo} {
		if _, err := s.DraftCard(ctx, req(id)); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s drafted: %v", id, err)
		}
	}
	res, err := s.DraftCard(ctx, req(carder))
	if err != nil || res.GetDecisionId() == "" || res.GetExpiresAt() == "" {
		t.Fatalf("granted: %v %v", res, err)
	}
	r, err := dec.Get(ctx, res.GetDecisionId())
	if err != nil || r.Kind != decisions.Card || r.State != decisions.Open || r.SessionID != carder {
		t.Fatalf("the card's decision: %+v %v", r, err)
	}
	bad := req(carder)
	bad.Options = bad.Options[:1]
	if _, err := s.DraftCard(ctx, bad); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("one option: %v", err)
	}
	if err := store.Archive(ctx, carder); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DraftCard(ctx, req(carder)); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("an archived session drafted: %v", err)
	}
}
