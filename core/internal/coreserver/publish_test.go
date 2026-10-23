//go:build integration

package coreserver

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
)

// docs/adr/0078 §8: only a definition granting publish_event publishes, and
// core passes on the caller's definition, never one the request names.
func TestPublishEvent_GrantedDefinitionsOnly(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	store := sessions.NewStore(pool)
	if _, err := pool.Exec(ctx, `INSERT INTO agent_definitions (name, sha, spec, effective, valid) VALUES
		('pub', 'sha-p', '{}', '{"sidecarTools":["publish_event"]}', true),
		('quiet', 'sha-q', '{}', '{"sidecarTools":["send_message"]}', true)`); err != nil {
		t.Fatal(err)
	}
	pub, _ := store.Create(ctx, sessions.CreateParams{Repo: "r", PermissionMode: "default", DefinitionName: "pub", DefinitionSHA: "sha-p"})
	quiet, _ := store.Create(ctx, sessions.CreateParams{Repo: "r", PermissionMode: "default", DefinitionName: "quiet", DefinitionSHA: "sha-q"})
	repo, _ := store.Create(ctx, sessions.CreateParams{Repo: "r"})

	s := &Server{sessions: store}
	req := func(id string) *agentfleetv1.PublishEventRequest {
		return &agentfleetv1.PublishEventRequest{SessionId: id, Type: "dev.bnei.agent.x", Subject: "s", Text: "t"}
	}
	if _, err := s.PublishEvent(ctx, req(pub)); status.Code(err) != codes.Unavailable {
		t.Fatalf("no bridge wired: %v", err)
	}
	var defs []string
	s.SetPublishFunc(func(_ context.Context, _, definition, _, _, _, _ string) (string, bool, int32, error) {
		defs = append(defs, definition)
		return "e1", true, 1, nil
	})
	for _, id := range []string{quiet, repo} {
		if _, err := s.PublishEvent(ctx, req(id)); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s published: %v", id, err)
		}
	}
	res, err := s.PublishEvent(ctx, req(pub))
	if err != nil || res.GetId() != "e1" || len(defs) != 1 || defs[0] != "pub" {
		t.Fatalf("granted: %v %v %v", res, err, defs)
	}
	if err := store.Archive(ctx, pub); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishEvent(ctx, req(pub)); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("an archived session published: %v", err)
	}
}
