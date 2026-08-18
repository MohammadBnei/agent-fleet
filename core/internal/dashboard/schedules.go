package dashboard

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/schedules"
)

// CRUD over the schedules table — a direct copy of the
// ListRepos/CreateRepo/UpdateRepo/DeleteRepo handler shape, including its
// error mapping, since this is the same "dashboard-editable entity, live
// refresh, no redeploy" pattern docs/adr/0028 established.

func (s *Server) ListSchedules(ctx context.Context, _ *connect.Request[agentfleetv1.ListSchedulesRequest]) (*connect.Response[agentfleetv1.ListSchedulesResponse], error) {
	list, err := s.schedules.List(ctx)
	if err != nil {
		slog.Error("dashboard ListSchedules", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*agentfleetv1.Schedule, 0, len(list))
	for _, sc := range list {
		out = append(out, scheduleToProto(sc))
	}
	return connect.NewResponse(&agentfleetv1.ListSchedulesResponse{Schedules: out}), nil
}

func (s *Server) CreateSchedule(ctx context.Context, req *connect.Request[agentfleetv1.CreateScheduleRequest]) (*connect.Response[agentfleetv1.CreateScheduleResponse], error) {
	in, runAt, err := scheduleFromProto("", req.Msg.GetName(), req.Msg.GetRepo(), req.Msg.GetPrompt(),
		req.Msg.GetCron(), req.Msg.GetIntervalSeconds(), true, req.Msg.GetRunAt())
	if err != nil {
		return nil, err
	}

	sc, err := s.schedules.Create(ctx, in, runAt)
	if err != nil {
		return nil, scheduleErr("CreateSchedule", err)
	}
	return connect.NewResponse(&agentfleetv1.CreateScheduleResponse{Schedule: scheduleToProto(sc)}), nil
}

func (s *Server) UpdateSchedule(ctx context.Context, req *connect.Request[agentfleetv1.UpdateScheduleRequest]) (*connect.Response[agentfleetv1.UpdateScheduleResponse], error) {
	if req.Msg.GetId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}
	in, runAt, err := scheduleFromProto(req.Msg.GetId(), req.Msg.GetName(), req.Msg.GetRepo(), req.Msg.GetPrompt(),
		req.Msg.GetCron(), req.Msg.GetIntervalSeconds(), req.Msg.GetEnabled(), req.Msg.GetRunAt())
	if err != nil {
		return nil, err
	}

	sc, err := s.schedules.Update(ctx, in, runAt)
	if err != nil {
		return nil, scheduleErr("UpdateSchedule", err)
	}
	return connect.NewResponse(&agentfleetv1.UpdateScheduleResponse{Schedule: scheduleToProto(sc)}), nil
}

func (s *Server) RunScheduleNow(ctx context.Context, req *connect.Request[agentfleetv1.RunScheduleNowRequest]) (*connect.Response[agentfleetv1.RunScheduleNowResponse], error) {
	if req.Msg.GetId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}
	sc, err := s.schedules.RunNow(ctx, req.Msg.GetId())
	if err != nil {
		return nil, scheduleErr("RunScheduleNow", err)
	}
	return connect.NewResponse(&agentfleetv1.RunScheduleNowResponse{Schedule: scheduleToProto(sc)}), nil
}

func (s *Server) DeleteSchedule(ctx context.Context, req *connect.Request[agentfleetv1.DeleteScheduleRequest]) (*connect.Response[agentfleetv1.DeleteScheduleResponse], error) {
	if req.Msg.GetId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}
	if err := s.schedules.Delete(ctx, req.Msg.GetId()); err != nil {
		return nil, scheduleErr("DeleteSchedule", err)
	}
	return connect.NewResponse(&agentfleetv1.DeleteScheduleResponse{}), nil
}

// scheduleFromProto validates the wire shape and maps it onto the store's.
// Validation lives here — at the trust boundary — rather than at fire time,
// where a bad cadence's only symptom is a last_status nobody reads.
func scheduleFromProto(id, name, repo, prompt, cron string, interval int32, enabled bool, runAt string) (schedules.Schedule, time.Time, error) {
	if name == "" || prompt == "" || repo == "" {
		return schedules.Schedule{}, time.Time{}, connect.NewError(connect.CodeInvalidArgument,
			errors.New("name, repo and prompt are required"))
	}
	if cron != "" && interval != 0 {
		return schedules.Schedule{}, time.Time{}, connect.NewError(connect.CodeInvalidArgument,
			errors.New("set a cron expression or an interval, not both"))
	}
	// Mirrors the column CHECK rather than trusting the DB error, so the user
	// gets a real message instead of a raw constraint violation.
	if interval != 0 && interval < 60 {
		return schedules.Schedule{}, time.Time{}, connect.NewError(connect.CodeInvalidArgument,
			errors.New("interval_seconds must be at least 60"))
	}
	if err := schedules.ValidateCron(cron, time.Now()); err != nil {
		return schedules.Schedule{}, time.Time{}, connect.NewError(connect.CodeInvalidArgument, err)
	}

	sc := schedules.Schedule{ID: id, Name: name, Repo: repo, Prompt: prompt, Cron: cron, Enabled: enabled}
	if interval != 0 {
		sc.IntervalSeconds = &interval
	}

	// One-shot: run_at is the whole schedule, so an unparseable or missing one
	// would silently become "fire on the next tick".
	var when time.Time
	if sc.OneShot() {
		var err error
		if when, err = time.Parse(time.RFC3339, runAt); err != nil {
			return schedules.Schedule{}, time.Time{}, connect.NewError(connect.CodeInvalidArgument,
				errors.New("a schedule with no cron and no interval needs run_at as an RFC3339 time"))
		}
	}
	return sc, when, nil
}

func scheduleErr(op string, err error) error {
	switch {
	case errors.Is(err, schedules.ErrExists):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, pgx.ErrNoRows):
		return connect.NewError(connect.CodeNotFound, err)
	}
	slog.Error("dashboard "+op, "error", err)
	return connect.NewError(connect.CodeInternal, err)
}

func scheduleToProto(sc schedules.Schedule) *agentfleetv1.Schedule {
	lastRun := ""
	if sc.LastRunAt != nil {
		lastRun = sc.LastRunAt.Format(time.RFC3339)
	}
	var interval int32
	if sc.IntervalSeconds != nil {
		interval = *sc.IntervalSeconds
	}
	return &agentfleetv1.Schedule{
		Id:              sc.ID,
		Name:            sc.Name,
		Repo:            sc.Repo,
		Prompt:          sc.Prompt,
		Cron:            sc.Cron,
		IntervalSeconds: interval,
		Enabled:         sc.Enabled,
		NextRunAt:       sc.NextRunAt.Format(time.RFC3339),
		LastRunAt:       lastRun,
		LastStatus:      sc.LastStatus,
		RunNow:          sc.RunNow,
	}
}
