package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// StandingOnly is the refusal of every path that would open a standing definition other than Start
// (docs/adr/0084 §2): nil when spec is not standing.
func StandingOnly(spec *definitions.Spec) error {
	if spec == nil || spec.Lifecycle == nil || !slices.Contains(spec.Lifecycle.Allow, sessions.Standing) {
		return nil
	}
	return errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyStandingOnly, errors.New("this definition is a standing character: an admin's Start opens it"))
}

// StartStanding makes a character's one session, or resumes a stopped one with a fresh SDK session and the current
// definition version (docs/adr/0084 §2). It provisions nothing, as CreateSession does not.
func (s *Server) StartStanding(ctx context.Context, req *connect.Request[agentfleetv1.StartStandingRequest]) (*connect.Response[agentfleetv1.StartStandingResponse], error) {
	name := req.Msg.GetDefinition()
	if s.definitions == nil {
		return nil, errkeys.Connect(connect.CodeUnavailable, errkeys.KeyInternal, errors.New("defined agents are not configured"))
	}
	by := adminSub(ctx)
	if by == "" {
		return nil, errkeys.Connect(connect.CodeUnauthenticated, errkeys.KeyInternal, errors.New("no identity on the request: a character is started by its admin"))
	}
	head, err := s.definitions.Resolve(name)
	switch {
	case errors.Is(err, definitions.ErrUnknown):
		return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, err)
	case errors.Is(err, definitions.ErrInvalid):
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, err)
	case err != nil:
		return nil, errkeys.Connect(connect.CodeUnavailable, errkeys.KeyInternal, err)
	}
	if StandingOnly(head.Effective) == nil {
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, fmt.Errorf("definition %q is not standing", name))
	}

	row, err := s.sessions.StandingByDefinition(ctx, name)
	switch {
	case errors.Is(err, sessions.ErrNotFound):
		principal := sessions.StandingPrefix + name
		row, err = s.createSession(ctx, &agentfleetv1.CreateSessionRequest{Title: &name}, &head, sessions.CreateParams{
			Owner: "client:" + principal, Visibility: "private", Lifecycle: sessions.Standing,
			Invoker: principal, ThreadKey: "main", StartedBy: by, TrustSet: sessions.TrustFor(sessions.TierFleet),
		})
		if err != nil {
			return nil, err
		}
	case err != nil:
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	default:
		if row.StoppedAt == nil {
			return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, fmt.Errorf("%q is already started", name))
		}
		if ok, err := s.sessions.RestartStanding(ctx, row.ID, head.SHA, by); err != nil {
			return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		} else if !ok {
			return nil, errkeys.Connect(connect.CodeAborted, errkeys.KeyInternal, errors.New("the character changed under this Start; retry"))
		}
		if row, err = s.sessions.Get(ctx, row.ID); err != nil {
			return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		}
	}
	slog.InfoContext(ctx, "dashboard StartStanding", "definition", name, "sessionId", row.ID, "by", by)
	return connect.NewResponse(&agentfleetv1.StartStandingResponse{Session: SessionToProto(*row)}), nil
}

// StopStanding marks a character stopped (docs/adr/0084 §2): the row takes no pod until Start, and a live pod gets
// StopSession's cooperative abort and its grace-period teardown.
func (s *Server) StopStanding(ctx context.Context, req *connect.Request[agentfleetv1.StopStandingRequest]) (*connect.Response[agentfleetv1.StopStandingResponse], error) {
	name := req.Msg.GetDefinition()
	row, err := s.sessions.StandingByDefinition(ctx, name)
	if errors.Is(err, sessions.ErrNotFound) {
		return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, fmt.Errorf("%q was never started", name))
	}
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	// Marked before the abort entry: the mark is what keeps a racing message from warming it again.
	ok, err := s.sessions.MarkStandingStopped(ctx, row.ID)
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	if !ok {
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, fmt.Errorf("%q is already stopped", name))
	}
	if _, err := s.transcr.Append(ctx, row.ID, "human", "stopped by "+adminSub(ctx), "abort", uuid.NewString()); err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	slog.InfoContext(ctx, "dashboard StopStanding", "definition", name, "sessionId", row.ID)
	return connect.NewResponse(&agentfleetv1.StopStandingResponse{}), nil
}

func adminSub(ctx context.Context) string {
	id, _ := auth.FromContext(ctx)
	if id.Sub == "" {
		return id.Email // a cookie minted before Sub existed
	}
	return id.Sub
}
