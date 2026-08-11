package coreserver

import (
	"context"
	"fmt"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/thotevents"
)

// RequestThotPermission is thot's canUseTool gate (docs/adr/0035): append a
// permission_request, then block until a human answers it via the
// dashboard. Structurally identical to AskUserQuestion's ask-and-long-poll
// — deliberately, since that pattern is already proven here — but against
// thot_events instead of the task-scoped transcript.
//
// The one rule this must never break (docs/adr/0029): a decision is never
// inferred from silence. Timing out returns "pending", not "allowed"; only
// a real RespondToThotPermission call produces allowed/denied.
func (s *Server) RequestThotPermission(ctx context.Context, req *agentfleetv1.RequestThotPermissionRequest) (*agentfleetv1.RequestThotPermissionResponse, error) {
	if req.GetToolName() == "" {
		return nil, fmt.Errorf("tool_name is required")
	}

	payload := req.GetInputJson()
	if payload == "" {
		payload = "{}"
	}
	// Idempotency key deliberately empty: every canUseTool invocation is a
	// genuinely distinct prompt, even for an identical tool+input pair
	// (thot legitimately retries the same restart twice). The store mints
	// a UUID, i.e. "never deduplicated" — the correct behavior here.
	requestID, err := s.thotEvents.Append(ctx, thotevents.KindPermissionRequest, "thot", payload, "")
	if err != nil {
		return nil, fmt.Errorf("RequestThotPermission: %w", err)
	}

	timeoutMs := req.GetTimeoutMs()
	if timeoutMs <= 0 {
		timeoutMs = 60000
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)

	for {
		resp, err := s.thotEvents.FindResponse(ctx, requestID)
		if err != nil {
			return nil, fmt.Errorf("RequestThotPermission: %w", err)
		}
		if resp != nil {
			// Payload is the decision itself: "allow" or a deny reason,
			// written by RespondToThotPermission below.
			status := "denied"
			message := resp.Payload
			if resp.Payload == thotevents.DecisionAllow {
				status = "allowed"
				message = ""
			}
			return &agentfleetv1.RequestThotPermissionResponse{
				Status:    status,
				Message:   message,
				RequestId: requestID,
			}, nil
		}
		if time.Now().After(deadline) {
			return &agentfleetv1.RequestThotPermissionResponse{Status: "pending", RequestId: requestID}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (s *Server) AppendThotEvent(ctx context.Context, req *agentfleetv1.AppendThotEventRequest) (*agentfleetv1.AppendThotEventResponse, error) {
	switch req.GetKind() {
	case thotevents.KindFinding, thotevents.KindAlert, thotevents.KindAuditRun:
	default:
		// permission_request/permission_response are deliberately not
		// accepted here — they're only ever created by
		// RequestThotPermission/RespondToThotPermission, which own the
		// correlation invariant between them.
		return nil, fmt.Errorf("AppendThotEvent: kind %q is not appendable via this RPC", req.GetKind())
	}

	actor := req.GetActor()
	if actor == "" {
		actor = "thot"
	}
	id, err := s.thotEvents.Append(ctx, req.GetKind(), actor, req.GetPayload(), req.GetIdempotencyKey())
	if err != nil {
		return nil, fmt.Errorf("AppendThotEvent: %w", err)
	}
	return &agentfleetv1.AppendThotEventResponse{Id: id}, nil
}
