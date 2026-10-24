package coreserver

import (
	"context"
	"errors"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/MohammadBnei/agent-fleet/core/internal/bridgeclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/events"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// PublishFunc is dispatch.Publish bound to its pool and bridge client,
// injected like warm: the publish rules live with the dispatcher, which
// reads what they write.
type PublishFunc func(ctx context.Context, sessionID, definition, typ, subject, text, key string) (id string, trusted bool, hops int32, err error)

func (s *Server) SetPublishFunc(f PublishFunc) { s.publish = f }

// PublishEvent hands work to whoever owns or subscribes to the event
// (docs/adr/0078 §8). Only a definition session whose pinned spec grants
// publish_event may call it: the tool is registered in every sidecar, so
// this check, not the worker's, is the grant.
func (s *Server) PublishEvent(ctx context.Context, req *agentfleetv1.PublishEventRequest) (*agentfleetv1.PublishEventResponse, error) {
	if s.publish == nil {
		return nil, status.Error(codes.Unavailable, "the event bridge is not wired")
	}
	t, err := s.sessions.Get(ctx, req.GetSessionId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "session %s: %v", req.GetSessionId(), err)
	}
	if t.DefinitionName == nil || t.ArchivedAt != nil {
		return nil, status.Error(codes.PermissionDenied, "only an open definition session may publish events")
	}
	spec, _, err := s.agentSpecOf(ctx, req.GetSessionId())
	if err != nil {
		return nil, err
	}
	if spec == nil || !slices.Contains(spec.GetSidecarTools(), "publish_event") {
		return nil, status.Error(codes.PermissionDenied, "this definition does not grant publish_event")
	}
	id, trusted, hops, err := s.publish(ctx, req.GetSessionId(), *t.DefinitionName, req.GetType(), req.GetSubject(), req.GetText(), req.GetIdempotencyKey())
	switch {
	case errors.Is(err, events.ErrPublishRefused):
		return nil, status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, bridgeclient.ErrNotConfigured):
		return nil, status.Error(codes.Unavailable, err.Error())
	case err != nil:
		return nil, status.Errorf(codes.Unavailable, "publish: %v", err)
	}
	return &agentfleetv1.PublishEventResponse{Id: id, Trusted: trusted, Hops: hops}, nil
}
