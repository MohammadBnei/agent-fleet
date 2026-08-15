package dashboard

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/scheduledaudits"
)

// CRUD over thot's periodic cluster checks — a direct copy of the
// ListRepos/CreateRepo/UpdateRepo/DeleteRepo handler shape, including its
// error mapping, since this is the same "dashboard-editable entity, live
// refresh, no redeploy" pattern docs/adr/0028 established.

func (s *Server) ListScheduledAudits(ctx context.Context, _ *connect.Request[agentfleetv1.ListScheduledAuditsRequest]) (*connect.Response[agentfleetv1.ListScheduledAuditsResponse], error) {
	audits, err := s.audits.List(ctx)
	if err != nil {
		slog.Error("dashboard ListScheduledAudits", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*agentfleetv1.ScheduledAudit, 0, len(audits))
	for _, a := range audits {
		out = append(out, auditToProto(a))
	}
	return connect.NewResponse(&agentfleetv1.ListScheduledAuditsResponse{Audits: out}), nil
}

func (s *Server) CreateScheduledAudit(ctx context.Context, req *connect.Request[agentfleetv1.CreateScheduledAuditRequest]) (*connect.Response[agentfleetv1.CreateScheduledAuditResponse], error) {
	name, prompt, interval := req.Msg.GetName(), req.Msg.GetPrompt(), req.Msg.GetIntervalSeconds()
	if name == "" || prompt == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name and prompt are required"))
	}
	// Mirrors the CHECK constraint rather than trusting the DB error, so
	// the user gets a real message instead of a raw constraint violation.
	if interval < 60 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("interval_seconds must be at least 60"))
	}

	a, err := s.audits.Create(ctx, name, prompt, interval)
	if err != nil {
		if errors.Is(err, scheduledaudits.ErrExists) {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		slog.Error("dashboard CreateScheduledAudit", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.CreateScheduledAuditResponse{Audit: auditToProto(a)}), nil
}

func (s *Server) UpdateScheduledAudit(ctx context.Context, req *connect.Request[agentfleetv1.UpdateScheduledAuditRequest]) (*connect.Response[agentfleetv1.UpdateScheduledAuditResponse], error) {
	if req.Msg.GetId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}
	if req.Msg.GetIntervalSeconds() < 60 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("interval_seconds must be at least 60"))
	}

	a, err := s.audits.Update(ctx, req.Msg.GetId(), req.Msg.GetName(), req.Msg.GetPrompt(), req.Msg.GetIntervalSeconds(), req.Msg.GetEnabled())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		slog.Error("dashboard UpdateScheduledAudit", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.UpdateScheduledAuditResponse{Audit: auditToProto(a)}), nil
}

func (s *Server) RunScheduledAuditNow(ctx context.Context, req *connect.Request[agentfleetv1.RunScheduledAuditNowRequest]) (*connect.Response[agentfleetv1.RunScheduledAuditNowResponse], error) {
	if req.Msg.GetId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}
	a, err := s.audits.RunNow(ctx, req.Msg.GetId())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		slog.Error("dashboard RunScheduledAuditNow", "id", req.Msg.GetId(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.RunScheduledAuditNowResponse{Audit: auditToProto(a)}), nil
}

func (s *Server) DeleteScheduledAudit(ctx context.Context, req *connect.Request[agentfleetv1.DeleteScheduledAuditRequest]) (*connect.Response[agentfleetv1.DeleteScheduledAuditResponse], error) {
	if err := s.audits.Delete(ctx, req.Msg.GetId()); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		slog.Error("dashboard DeleteScheduledAudit", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.DeleteScheduledAuditResponse{}), nil
}

func auditToProto(a scheduledaudits.Audit) *agentfleetv1.ScheduledAudit {
	lastRun := ""
	if a.LastRunAt != nil {
		lastRun = a.LastRunAt.Format(time.RFC3339)
	}
	return &agentfleetv1.ScheduledAudit{
		Id:              a.ID,
		Name:            a.Name,
		Prompt:          a.Prompt,
		IntervalSeconds: a.IntervalSeconds,
		Enabled:         a.Enabled,
		NextRunAt:       a.NextRunAt.Format(time.RFC3339),
		LastRunAt:       lastRun,
		LastStatus:      a.LastStatus,
	}
}
