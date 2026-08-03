// Package grpcserver implements E2eProvisionerService — the one internal
// gRPC surface in the fleet, called by fleet-core's /e2e-kill handler
// instead of that handler writing e2e_sessions directly (see docs/adr/0013).
package grpcserver

import (
	"context"

	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/db"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// SessionStore is the narrow slice of db.Store this server needs — a
// hand-written fake satisfies it in tests, no live Postgres required.
type SessionStore interface {
	RequestKill(ctx context.Context, taskID string) (bool, error)
	GetActiveSessionForTask(ctx context.Context, taskID string) (*db.Session, error)
}

type Server struct {
	agentfleetv1.UnimplementedE2EProvisionerServiceServer
	store SessionStore
}

func New(store SessionStore) *Server {
	return &Server{store: store}
}

func (s *Server) KillE2ESession(ctx context.Context, req *agentfleetv1.KillE2ESessionRequest) (*agentfleetv1.KillE2ESessionResponse, error) {
	killed, err := s.store.RequestKill(ctx, req.GetTaskId())
	if err != nil {
		return nil, err
	}
	return &agentfleetv1.KillE2ESessionResponse{Killed: killed}, nil
}

func (s *Server) GetE2ESessionStatus(ctx context.Context, req *agentfleetv1.GetE2ESessionStatusRequest) (*agentfleetv1.GetE2ESessionStatusResponse, error) {
	sess, err := s.store.GetActiveSessionForTask(ctx, req.GetTaskId())
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return &agentfleetv1.GetE2ESessionStatusResponse{}, nil
	}
	previewURL := ""
	if sess.IngressPath != nil {
		previewURL = *sess.IngressPath
	}
	return &agentfleetv1.GetE2ESessionStatusResponse{Status: sess.Status, PreviewUrl: previewURL}, nil
}
