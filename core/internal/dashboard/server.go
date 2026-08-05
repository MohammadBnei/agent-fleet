// Package dashboard implements the web dashboard's ConnectRPC API
// (agentfleet.v1.DashboardService, see docs/adr/0015) on top of the exact
// same tasks.Store / transcript.Store / provisionerclient.Client that core's
// Discord commands already use — no new business logic, just a second
// caller of the same store methods.
package dashboard

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

type Server struct {
	tasks   *tasks.Store
	transcr transcript.Store
	e2e     *provisionerclient.Client
	hub     *Hub
}

func NewServer(taskStore *tasks.Store, transcr transcript.Store, e2e *provisionerclient.Client, hub *Hub) *Server {
	return &Server{tasks: taskStore, transcr: transcr, e2e: e2e, hub: hub}
}

var _ agentfleetv1connect.DashboardServiceHandler = (*Server)(nil)

const defaultListLimit = 50

func (s *Server) ListTasks(ctx context.Context, req *connect.Request[agentfleetv1.ListTasksRequest]) (*connect.Response[agentfleetv1.ListTasksResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = defaultListLimit
	}
	list, err := s.tasks.ListRecentTasks(ctx, limit)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*agentfleetv1.Task, len(list))
	for i, t := range list {
		out[i] = taskToProto(t)
	}
	return connect.NewResponse(&agentfleetv1.ListTasksResponse{Tasks: out}), nil
}

func (s *Server) GetTask(ctx context.Context, req *connect.Request[agentfleetv1.GetTaskRequest]) (*connect.Response[agentfleetv1.GetTaskResponse], error) {
	t, err := s.tasks.GetTask(ctx, req.Msg.GetId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if t == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("task not found"))
	}
	return connect.NewResponse(&agentfleetv1.GetTaskResponse{Task: taskToProto(*t)}), nil
}

// CreateTask lets the dashboard create a task the same way a Discord /task
// command does, minus the Discord thread — it calls the exact same
// tasks.Store.CreateTask core/internal/discord/handlers.go's startTask
// calls, just with nil channel/thread (docs/adr/0015). PostToThread
// (core/internal/discord/session.go) already no-ops on a nil ThreadID, so
// no other code needs to special-case a dashboard-origin task.
func (s *Server) CreateTask(ctx context.Context, req *connect.Request[agentfleetv1.CreateTaskRequest]) (*connect.Response[agentfleetv1.CreateTaskResponse], error) {
	repo := req.Msg.GetRepo()
	if _, ok := tasks.KnownRepos[repo]; !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown repo %q", repo))
	}
	description := req.Msg.GetDescription()
	if description == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("description is required"))
	}

	id, err := s.tasks.CreateTask(ctx, repo, description, nil, nil)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	t, err := s.tasks.GetTask(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.CreateTaskResponse{Task: taskToProto(*t)}), nil
}

func (s *Server) GetTranscript(ctx context.Context, req *connect.Request[agentfleetv1.ReadTranscriptSinceRequest]) (*connect.Response[agentfleetv1.ReadTranscriptSinceResponse], error) {
	taskID := req.Msg.GetTaskId()
	entries, next, err := s.transcr.ReadSince(ctx, taskID, req.Msg.GetSinceSeq(), 1000)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*agentfleetv1.TranscriptEntry, len(entries))
	for i, e := range entries {
		out[i] = entryToProto(taskID, e)
	}
	return connect.NewResponse(&agentfleetv1.ReadTranscriptSinceResponse{Entries: out, NextSeq: next}), nil
}

func (s *Server) StreamTranscript(ctx context.Context, req *connect.Request[agentfleetv1.StreamTranscriptRequest], stream *connect.ServerStream[agentfleetv1.TranscriptEntry]) error {
	ch, cancel := s.hub.Subscribe(req.Msg.GetTaskId(), req.Msg.GetSinceSeq())
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(e); err != nil {
				return err // client disconnected mid-write
			}
		}
	}
}

func (s *Server) GetE2EStatus(ctx context.Context, req *connect.Request[agentfleetv1.GetE2EStatusRequest]) (*connect.Response[agentfleetv1.GetE2EStatusResponse], error) {
	status, previewURL, err := s.e2e.GetSessionStatus(ctx, req.Msg.GetTaskId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.GetE2EStatusResponse{Status: status, PreviewUrl: previewURL}), nil
}

// Approve, Stop, and KillE2E below call the exact same store methods
// core/internal/discord/handlers.go's /approve, /stop, and
// /e2e-kill commands already call — no new business logic, just a second
// caller (see docs/adr/0014, docs/adr/0015).

func (s *Server) Approve(ctx context.Context, req *connect.Request[agentfleetv1.ApproveRequest]) (*connect.Response[agentfleetv1.ApproveResponse], error) {
	taskID := req.Msg.GetTaskId()
	if _, err := s.transcr.Append(ctx, taskID, "human", "approved", "approve", uuid.NewString()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.ApproveResponse{Status: "approved"}), nil
}

func (s *Server) Stop(ctx context.Context, req *connect.Request[agentfleetv1.StopRequest]) (*connect.Response[agentfleetv1.StopResponse], error) {
	taskID := req.Msg.GetTaskId()
	reason := "stopped by human"
	if req.Msg.Reason != nil && *req.Msg.Reason != "" {
		reason = req.Msg.GetReason()
	}
	if _, err := s.transcr.Append(ctx, taskID, "human", reason, "abort", uuid.NewString()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.StopResponse{Status: "stopping"}), nil
}

func (s *Server) KillE2E(ctx context.Context, req *connect.Request[agentfleetv1.KillE2ERequest]) (*connect.Response[agentfleetv1.KillE2EResponse], error) {
	killed, err := s.e2e.KillSession(ctx, req.Msg.GetTaskId(), uuid.NewString())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.KillE2EResponse{Killed: killed}), nil
}

// AnswerQuestion appends the human's answer to a pending QUESTION-type
// transcript entry (posted by the planner's AskUserQuestion MCP tool call,
// see docs/adr/0018) — the same Append() call Approve/Stop above make, just
// with an opaque JSON payload instead of a fixed string. `req.Msg.Seq` isn't
// used server-side for correlation (only one question is ever pending per
// task at a time); it's carried through for the dashboard's own bookkeeping.
func (s *Server) AnswerQuestion(ctx context.Context, req *connect.Request[agentfleetv1.AnswerQuestionRequest]) (*connect.Response[agentfleetv1.AnswerQuestionResponse], error) {
	taskID := req.Msg.GetTaskId()
	if _, err := s.transcr.Append(ctx, taskID, "human", req.Msg.GetAnswersJson(), "answer", uuid.NewString()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.AnswerQuestionResponse{Status: "answered"}), nil
}

// ListWorktrees left-joins the provisioner's raw worktree list against
// `tasks` (reliability-findings.md #2) — core has no PVC access itself,
// so the worktree data comes from a passthrough call to the provisioner;
// task_status/task_error/pr_url are left unset (not an error) for a
// worktree whose task row no longer exists, exactly the orphaned case
// this view exists to surface. An inner join would hide it.
func (s *Server) ListWorktrees(ctx context.Context, _ *connect.Request[agentfleetv1.ListWorktreesRequest]) (*connect.Response[agentfleetv1.ListWorktreesViewResponse], error) {
	worktrees, err := s.e2e.ListWorktrees(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*agentfleetv1.WorktreeView, len(worktrees))
	for i, w := range worktrees {
		view := &agentfleetv1.WorktreeView{
			TaskId:        w.GetTaskId(),
			Repo:          w.GetRepo(),
			Branch:        w.GetBranch(),
			UpstreamTrack: w.GetUpstreamTrack(),
			MtimeUnix:     w.GetMtimeUnix(),
		}
		if info, err := s.tasks.GetTaskStatusInfo(ctx, w.GetTaskId()); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		} else if info != nil {
			view.TaskStatus = &info.Status
			view.TaskError = info.LastError
			view.PrUrl = info.PrURL
		}
		out[i] = view
	}
	return connect.NewResponse(&agentfleetv1.ListWorktreesViewResponse{Worktrees: out}), nil
}

func (s *Server) DeleteWorktree(ctx context.Context, req *connect.Request[agentfleetv1.DeleteWorktreeRequest]) (*connect.Response[agentfleetv1.DeleteWorktreeResponse], error) {
	deleted, err := s.e2e.DeleteWorktree(ctx, req.Msg.GetTaskId(), req.Msg.GetRepo(), req.Msg.GetAlsoDeleteBranch())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.DeleteWorktreeResponse{Deleted: deleted}), nil
}

func taskToProto(t tasks.Task) *agentfleetv1.Task {
	return &agentfleetv1.Task{
		Id:          t.ID,
		Repo:        t.Repo,
		Description: t.Description,
		Status:      t.Status,
		ThreadId:    t.ThreadID,
		PrUrl:       t.PrURL,
	}
}

func entryToProto(taskID string, e transcript.Entry) *agentfleetv1.TranscriptEntry {
	return &agentfleetv1.TranscriptEntry{
		TaskId: taskID,
		Seq:    e.Seq,
		From:   e.From,
		Text:   e.Text,
		Type:   stringToProtoType(e.Type),
	}
}

// stringToProtoType maps the MCP wire's plain-string transcript type
// ("" | "discussion" | "approve" | "abort" | "question" | "answer") to the
// enum this proto message uses — one direction only, nothing in this
// service ever needs enum->string.
func stringToProtoType(s string) agentfleetv1.TranscriptEntryType {
	switch s {
	case "discussion":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_DISCUSSION
	case "approve":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_APPROVE
	case "abort":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ABORT
	case "question":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_QUESTION
	case "answer":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ANSWER
	default:
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED
	}
}
