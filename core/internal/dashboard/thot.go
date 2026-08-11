package dashboard

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/thotevents"
)

// ListThotEvents is the dashboard's cursor read over thot's activity
// stream, plus whatever permission requests are still unanswered. The
// pending list is returned alongside (rather than made a second round
// trip) so a browser refresh mid-prompt re-renders the live card instead
// of dropping it — the same "a dropped message during a live permission
// decision isn't recoverable" concern docs/adr/0013 raised for the
// transcript.
func (s *Server) ListThotEvents(ctx context.Context, req *connect.Request[agentfleetv1.ListThotEventsRequest]) (*connect.Response[agentfleetv1.ListThotEventsResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = defaultListLimit
	}

	events, nextID, err := s.thotEvents.ReadSince(ctx, req.Msg.GetSinceId(), limit)
	if err != nil {
		slog.Error("dashboard ListThotEvents", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	pending, err := s.thotEvents.PendingRequests(ctx, limit)
	if err != nil {
		slog.Error("dashboard ListThotEvents pending", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&agentfleetv1.ListThotEventsResponse{
		Events:  thotEventsToProto(events),
		NextId:  nextID,
		Pending: thotEventsToProto(pending),
	}), nil
}

// RespondToThotPermission is the *only* way a thot permission decision is
// ever made (docs/adr/0035): a real, structured, human-authored call.
// thot's Discord channel is notify-only and deliberately has no listener,
// so there is no second path a "yeah go ahead" could arrive through.
func (s *Server) RespondToThotPermission(ctx context.Context, req *connect.Request[agentfleetv1.RespondToThotPermissionRequest]) (*connect.Response[agentfleetv1.RespondToThotPermissionResponse], error) {
	payload := req.Msg.GetMessage()
	if req.Msg.GetAllow() {
		payload = thotevents.DecisionAllow
	} else if payload == "" {
		payload = "denied by human"
	}

	if _, err := s.thotEvents.AppendReply(
		ctx, thotevents.KindPermissionResponse, "human", payload, uuid.NewString(), req.Msg.GetRequestId(),
	); err != nil {
		slog.Error("dashboard RespondToThotPermission", "requestId", req.Msg.GetRequestId(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.RespondToThotPermissionResponse{Status: "answered"}), nil
}

func thotEventsToProto(events []thotevents.Event) []*agentfleetv1.ThotEvent {
	out := make([]*agentfleetv1.ThotEvent, 0, len(events))
	for _, e := range events {
		out = append(out, &agentfleetv1.ThotEvent{
			Id:        e.ID,
			Kind:      e.Kind,
			Actor:     e.Actor,
			Payload:   e.Payload,
			ReplyTo:   e.ReplyTo,
			CreatedAt: e.CreatedAt.Format(time.RFC3339),
		})
	}
	return out
}
