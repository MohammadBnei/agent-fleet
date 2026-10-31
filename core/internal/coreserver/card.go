package coreserver

import (
	"context"
	"errors"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// DraftCard puts a choice in front of the owner (docs/adr/0082 §6). Only an
// open definition session whose pinned spec grants draft_card may call it:
// like publish_event, the tool is registered in every sidecar, so this check
// is the grant. It does not wait; the owner's choice arrives later as an
// answer in the thread.
func (s *Server) DraftCard(ctx context.Context, req *agentfleetv1.DraftCardRequest) (*agentfleetv1.DraftCardResponse, error) {
	if s.decisions == nil {
		return nil, status.Error(codes.Unavailable, "decisions are not configured")
	}
	t, err := s.sessions.Get(ctx, req.GetSessionId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "session %s: %v", req.GetSessionId(), err)
	}
	if t.DefinitionName == nil || t.ArchivedAt != nil {
		return nil, status.Error(codes.PermissionDenied, "only an open definition session may draft cards")
	}
	spec, _, err := s.agentSpecOf(ctx, req.GetSessionId())
	if err != nil {
		return nil, err
	}
	if spec == nil || !slices.Contains(spec.GetSidecarTools(), "draft_card") {
		return nil, status.Error(codes.PermissionDenied, "this definition does not grant draft_card")
	}
	r, err := s.decisions.Draft(ctx, s.transcr, decisions.Draft{
		SessionID: req.GetSessionId(), From: "agent", IdempotencyKey: req.GetIdempotencyKey(),
		Ask: req.GetAsk(), Subject: req.GetSubject(), Options: req.GetOptions(), WriteOwn: req.GetWriteOwn(),
	})
	if err != nil {
		return nil, draftCardErr(err)
	}
	s.announceBlocked(ctx, req.GetSessionId(), "drafted a card")
	// onCard always writes a card's expires_at, so Draft's row has one.
	return &agentfleetv1.DraftCardResponse{DecisionId: r.ID, ExpiresAt: r.ExpiresAt.UTC().Format(time.RFC3339)}, nil
}

// draftCardErr is Draft's error as the sidecar relays it. The budget's
// message starts with its key: the sidecar hands the agent the words only,
// and the key is what tells it to stop drafting rather than fix the card.
func draftCardErr(err error) error {
	switch {
	case errors.Is(err, decisions.ErrCardBudget):
		return errkeys.Status(codes.ResourceExhausted, errkeys.KeyCardBudget,
			string(errkeys.KeyCardBudget)+": this definition drafted its 10 cards of the last 24 hours; wait before drafting another")
	case errors.Is(err, decisions.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return status.Errorf(codes.Internal, "draft card: %v", err)
}
