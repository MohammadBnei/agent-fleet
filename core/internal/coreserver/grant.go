package coreserver

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

func (s *Server) SetDecisions(d *decisions.Store) { s.decisions = d }

// ConsumeGrant spends one of the caller's one-time grants (docs/adr/0082 §5).
// session_id is the lease's own (auth.go binds it, and the provisioner may
// not call this), so a session spends only its own grants.
func (s *Server) ConsumeGrant(ctx context.Context, req *agentfleetv1.ConsumeGrantRequest) (*agentfleetv1.ConsumeGrantResponse, error) {
	if s.decisions == nil {
		return nil, status.Error(codes.Unavailable, "decisions are not configured")
	}
	if req.GetTool() == "" {
		return nil, status.Error(codes.InvalidArgument, "tool is required")
	}
	id, err := s.decisions.ConsumeGrant(ctx, req.GetSessionId(), req.GetTool(), []byte(req.GetInputJson()))
	if errors.Is(err, decisions.ErrInvalid) {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &agentfleetv1.ConsumeGrantResponse{Granted: id != "", DecisionId: id}, nil
}
