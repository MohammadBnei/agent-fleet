package coreserver

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// Taint is the worker's taint gate (docs/adr/0082 §3–§4): it marks an outside
// read the worker is about to let run, then answers whether the session is
// tainted. The lease binds it to the caller's own session (auth.go), and the
// request has no way to say "untainted".
func (s *Server) Taint(ctx context.Context, req *agentfleetv1.TaintRequest) (*agentfleetv1.TaintResponse, error) {
	if read := req.GetOutsideRead(); read != "" {
		if err := s.sessions.MarkOutsideRead(ctx, req.GetSessionId(), read); err != nil {
			return nil, status.Errorf(codes.Internal, "Taint: %v", err)
		}
	}
	tainted, err := s.sessions.IsTainted(ctx, req.GetSessionId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Taint: %v", err)
	}
	return &agentfleetv1.TaintResponse{Tainted: tainted}, nil
}

// refuseTainted is docs/adr/0082 §4's core half: a tainted session reads and
// reports to the owner, nothing else, so the methods that act outside it
// (PublishEvent, PromptSession, Expose, RequestService) refuse it.
func (s *Server) refuseTainted(ctx context.Context, sessionID string) error {
	tainted, err := s.sessions.IsTainted(ctx, sessionID)
	if err != nil {
		return status.Errorf(codes.Internal, "check taint: %v", err)
	}
	if tainted {
		return status.Error(codes.PermissionDenied, "this session is tainted (docs/adr/0082 §4): it may only read and report to the owner")
	}
	return nil
}
