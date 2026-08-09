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
	"log/slog"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/filestore"
	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/lokiclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/promptsnippets"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

type Server struct {
	tasks       *tasks.Store
	transcr     transcript.Store
	journal     *journal.Store
	repos       *repos.Store
	snippets    *promptsnippets.Store
	e2e         *provisionerclient.Client
	files       filestore.Store
	hub         *Hub
	maxInFlight int
	loki        lokiclient.Querier
}

func NewServer(taskStore *tasks.Store, transcr transcript.Store, journalStore *journal.Store, repoStore *repos.Store, snippetStore *promptsnippets.Store, e2e *provisionerclient.Client, files filestore.Store, hub *Hub, maxInFlight int, loki lokiclient.Querier) *Server {
	return &Server{tasks: taskStore, transcr: transcr, journal: journalStore, repos: repoStore, snippets: snippetStore, e2e: e2e, files: files, hub: hub, maxInFlight: maxInFlight, loki: loki}
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
		slog.Error("dashboard ListTasks", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*agentfleetv1.Task, len(list))
	for i, t := range list {
		out[i] = TaskToProto(t)
	}
	return connect.NewResponse(&agentfleetv1.ListTasksResponse{Tasks: out}), nil
}

func (s *Server) GetTask(ctx context.Context, req *connect.Request[agentfleetv1.GetTaskRequest]) (*connect.Response[agentfleetv1.GetTaskResponse], error) {
	t, err := s.tasks.GetTask(ctx, req.Msg.GetId())
	if err != nil {
		slog.Error("dashboard GetTask", "taskId", req.Msg.GetId(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if t == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("task not found"))
	}
	return connect.NewResponse(&agentfleetv1.GetTaskResponse{Task: TaskToProto(*t)}), nil
}

// CreateTask lets the dashboard create a task the same way a Discord /task
// command does, minus the Discord thread — it calls the exact same
// tasks.Store.CreateTask core/internal/discord/handlers.go's startTask
// calls, just with nil channel/thread (docs/adr/0015). PostToThread
// (core/internal/discord/session.go) already no-ops on a nil ThreadID, so
// no other code needs to special-case a dashboard-origin task.
func (s *Server) CreateTask(ctx context.Context, req *connect.Request[agentfleetv1.CreateTaskRequest]) (*connect.Response[agentfleetv1.CreateTaskResponse], error) {
	repo := req.Msg.GetRepo()
	repoCfg, err := s.repos.Get(ctx, repo)
	if err != nil {
		slog.Error("dashboard CreateTask: repo lookup", "repo", repo, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if repoCfg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown repo %q", repo))
	}
	description := req.Msg.GetDescription()
	if description == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("description is required"))
	}

	guidance, suggestedMode, err := s.resolveGuidanceAndMode(ctx, req.Msg.GetSnippetIds())
	if err != nil {
		slog.Error("dashboard CreateTask: snippet lookup", "repo", repo, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Get model from request, validate it, and default to env if empty
	model := req.Msg.GetModel()
	if model == "" {
		model = os.Getenv("CLAUDE_MODEL")
		if model == "" {
			model = "claude-opus-4-8"
		}
	} else if !isValidModel(model) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid model %q", model))
	}

	id, err := s.tasks.CreateTask(ctx, repo, description, guidance, model, nil, nil)
	if err != nil {
		slog.Error("dashboard CreateTask", "repo", repo, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// If any snippet suggests a permission mode, auto-set it immediately
	if suggestedMode != "" {
		if err := s.tasks.SetPermissionMode(ctx, id, suggestedMode); err != nil {
			slog.Warn("dashboard CreateTask: failed to set suggested permission mode", "taskId", id, "mode", suggestedMode, "error", err)
		}
	}
	t, err := s.tasks.GetTask(ctx, id)
	if err != nil {
		slog.Error("dashboard CreateTask", "taskId", id, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	slog.Info("dashboard CreateTask", "taskId", id, "repo", repo)
	return connect.NewResponse(&agentfleetv1.CreateTaskResponse{Task: TaskToProto(*t)}), nil
}

// resolveGuidanceAndMode joins the text of the operator's selected prompt
// snippets, in the store's own name order, into the single guidance blob
// stored on the task. Also returns the first non-empty suggested_permission_mode
// from any of the snippets. Empty input returns "" for both — a task with no
// snippets attached gets no extra guidance at all, just its own description.
func (s *Server) resolveGuidanceAndMode(ctx context.Context, snippetIDs []string) (string, string, error) {
	if len(snippetIDs) == 0 {
		return "", "", nil
	}
	snippets, err := s.snippets.GetByIDs(ctx, snippetIDs)
	if err != nil {
		return "", "", err
	}
	texts := make([]string, len(snippets))
	suggestedMode := ""
	for i, sn := range snippets {
		texts[i] = sn.Text
		// Take the first non-empty suggested mode
		if suggestedMode == "" && sn.SuggestedPermissionMode != nil && *sn.SuggestedPermissionMode != "" {
			suggestedMode = *sn.SuggestedPermissionMode
		}
	}
	return strings.Join(texts, "\n\n"), suggestedMode, nil
}

// validModels is the allowlist of Claude models that can be selected per-task.
// This prevents injection of arbitrary model names and ensures only supported
// models are used.
var validModels = map[string]bool{
	"claude-opus-4-8":              true,
	"claude-sonnet-4-5-20250929":   true,
	"claude-opus-4-5-20251101":     true,
	"claude-haiku-4-5-20250929":    true,
}

func isValidModel(model string) bool {
	return validModels[model]
}

func (s *Server) GetTranscript(ctx context.Context, req *connect.Request[agentfleetv1.ReadTranscriptSinceRequest]) (*connect.Response[agentfleetv1.ReadTranscriptSinceResponse], error) {
	taskID := req.Msg.GetTaskId()
	entries, next, err := s.transcr.ReadSince(ctx, taskID, req.Msg.GetSinceSeq(), 1000)
	if err != nil {
		slog.Error("dashboard GetTranscript", "taskId", taskID, "error", err)
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
		slog.Error("dashboard GetE2EStatus", "taskId", req.Msg.GetTaskId(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.GetE2EStatusResponse{Status: status, PreviewUrl: previewURL}), nil
}

// Stop and KillE2E below call the exact same store methods
// core/internal/discord/handlers.go's /stop and /e2e-kill commands already
// call — no new business logic, just a second caller (see docs/adr/0014,
// docs/adr/0015). Approve is gone as of the sessions redesign (supersedes
// docs/adr/0021/0025's phase-boundary framing) — there's no plan->default
// flip left to fix a button to; SetPermissionMode below covers mode
// switching, RespondToPermission covers per-tool-call decisions.

// Stop posts a cooperative abort message (the worker pod notices it via
// streamHumanMessages and exits cleanly if it's alive and responsive) and
// also durably marks when the stop was requested — dispatch.Loop's
// grace-period sweep force-tears-down the pod via TearDownSession if it
// hasn't gone terminal within the grace window, covering the hung/crashed/
// unreachable-pod case a bare abort message can never reach.
func (s *Server) Stop(ctx context.Context, req *connect.Request[agentfleetv1.StopRequest]) (*connect.Response[agentfleetv1.StopResponse], error) {
	taskID := req.Msg.GetTaskId()
	reason := "stopped by human"
	if req.Msg.Reason != nil && *req.Msg.Reason != "" {
		reason = req.Msg.GetReason()
	}
	if _, err := s.transcr.Append(ctx, taskID, "human", reason, "abort", uuid.NewString()); err != nil {
		slog.Error("dashboard Stop", "taskId", taskID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := s.tasks.MarkStopRequested(ctx, taskID); err != nil {
		slog.Error("dashboard Stop: mark stop requested", "taskId", taskID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.StopResponse{Status: "stopping"}), nil
}

// validPermissionModes is an allowlist, not a passthrough — the value ends
// up in a real SDK call (worker/src/session.ts's streamHumanMessages calls
// q.setPermissionMode(mode) verbatim), so an unvalidated string here would
// let a malformed dashboard request reach the SDK with an arbitrary value.
// "default"/"plan" are now included — Approve (the old fixed plan->default
// flip) is gone as of the sessions redesign, so these two are only
// reachable through this one allowlisted lever.
var validPermissionModes = map[string]bool{
	"default":           true,
	"plan":               true,
	"acceptEdits":       true,
	"dontAsk":           true,
	"bypassPermissions": true,
}

// SetPermissionMode sets the running session's SDK permission mode
// (docs/adr/0027, extended by the sessions redesign supersession of
// docs/adr/0021/0025 — this is now the only mode lever, Approve is gone).
// "bypassPermissions" deliberately disables the canUseTool prompt-and-wait
// gate for the task's remaining session (see ADR-0027's SDK-source trace);
// the dashboard is responsible for getting explicit, typed confirmation
// from the human before ever sending that value here. Persists to
// tasks.permission_mode (durable "what mode is this session in right now"
// for the dashboard's mode picker) in addition to the transcript append
// that actually reaches the running worker.
func (s *Server) SetPermissionMode(ctx context.Context, req *connect.Request[agentfleetv1.SetPermissionModeRequest]) (*connect.Response[agentfleetv1.SetPermissionModeResponse], error) {
	taskID := req.Msg.GetTaskId()
	mode := req.Msg.GetMode()
	if !validPermissionModes[mode] {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid permission mode %q", mode))
	}
	if _, err := s.transcr.Append(ctx, taskID, "human", mode, "permission_mode", uuid.NewString()); err != nil {
		slog.Error("dashboard SetPermissionMode", "taskId", taskID, "mode", mode, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := s.tasks.SetPermissionMode(ctx, taskID, mode); err != nil {
		slog.Error("dashboard SetPermissionMode: persist", "taskId", taskID, "mode", mode, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.SetPermissionModeResponse{Status: "set"}), nil
}

func (s *Server) KillE2E(ctx context.Context, req *connect.Request[agentfleetv1.KillE2ERequest]) (*connect.Response[agentfleetv1.KillE2EResponse], error) {
	killed, err := s.e2e.KillSession(ctx, req.Msg.GetTaskId(), uuid.NewString())
	if err != nil {
		slog.Error("dashboard KillE2E", "taskId", req.Msg.GetTaskId(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.KillE2EResponse{Killed: killed}), nil
}

// AnswerQuestion appends the human's answer to a pending QUESTION-type
// transcript entry (posted by the agent's AskUserQuestion MCP tool call,
// see docs/adr/0018) via AppendReply — req.Msg.Seq is the question entry's
// own seq, now actually used server-side for correlation
// (reliability-findings.md #0: "any pending question + any reply" let an
// unrelated message satisfy a blocked AskUserQuestion call).
func (s *Server) AnswerQuestion(ctx context.Context, req *connect.Request[agentfleetv1.AnswerQuestionRequest]) (*connect.Response[agentfleetv1.AnswerQuestionResponse], error) {
	taskID := req.Msg.GetTaskId()
	if _, err := s.transcr.AppendReply(ctx, taskID, "human", req.Msg.GetAnswersJson(), "answer", uuid.NewString(), req.Msg.GetSeq()); err != nil {
		slog.Error("dashboard AnswerQuestion", "taskId", taskID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.AnswerQuestionResponse{Status: "answered"}), nil
}

// RespondToPermission answers a pending PERMISSION_REQUEST entry — the
// generalized canUseTool prompt-and-wait gate's counterpart to
// AnswerQuestion, same AppendReply-by-seq shape, kept as a sibling RPC
// rather than overloaded onto AnswerQuestion since the payload differs
// (allow/deny/updatedInput JSON vs. free-form answers JSON).
func (s *Server) RespondToPermission(ctx context.Context, req *connect.Request[agentfleetv1.RespondToPermissionRequest]) (*connect.Response[agentfleetv1.RespondToPermissionResponse], error) {
	taskID := req.Msg.GetTaskId()
	if _, err := s.transcr.AppendReply(ctx, taskID, "human", req.Msg.GetDecisionJson(), "permission_response", uuid.NewString(), req.Msg.GetSeq()); err != nil {
		slog.Error("dashboard RespondToPermission", "taskId", taskID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.RespondToPermissionResponse{Status: "answered"}), nil
}

// Discuss appends a human-authored free-text message from the dashboard —
// full parity with a Discord thread reply. The worker's existing
// streamHumanMessages SSE picks it up the same way it already picks up a
// Discord reply, since it's just a cursor-based transcript stream with no
// origin-specific handling (reliability-findings.md's "seamless
// interaction" gap: the dashboard previously had no way to send arbitrary
// text, only structured Stop/AnswerQuestion/RespondToPermission).
//
// Also the sessions redesign's "boots on first interaction" path
// (supersedes docs/adr/0021/0025's phase-boundary framing): a message to
// an idle session (no live pod) warms one first, same as clicking Warm
// explicitly, before the message is appended — so the pod that reads it
// back off streamHumanMessages already exists. Silently does nothing
// extra when a pod is already live (the common case).
func (s *Server) Discuss(ctx context.Context, req *connect.Request[agentfleetv1.DiscussRequest]) (*connect.Response[agentfleetv1.DiscussResponse], error) {
	taskID := req.Msg.GetTaskId()
	text := req.Msg.GetText()
	if text == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("text is required"))
	}
	if _, err := s.warmIfIdle(ctx, taskID); err != nil {
		return nil, err
	}
	if _, err := s.transcr.Append(ctx, taskID, "human", text, "discussion", uuid.NewString()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.DiscussResponse{Status: "sent"}), nil
}

// Warm boots a pod for an idle session on demand (see WarmRequest's own
// proto comment) — the explicit counterpart to Discuss's auto-warm. Gives
// an explicit, specific rejection reason for each way a click can be a
// no-op — unlike Discuss, which shares warmIfIdle's silent-skip behavior
// for those same cases because it has a message to send regardless.
func (s *Server) Warm(ctx context.Context, req *connect.Request[agentfleetv1.WarmRequest]) (*connect.Response[agentfleetv1.WarmResponse], error) {
	taskID := req.Msg.GetTaskId()
	t, err := s.tasks.GetTask(ctx, taskID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if t == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("task not found"))
	}
	if tasks.IsPodPhaseLive(t.PodPhase) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("session already has a live pod"))
	}
	if t.Status == "pending" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("task hasn't been claimed yet — it will dispatch automatically"))
	}
	podName, err := s.warmIfIdle(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.WarmResponse{Status: "warming", PodName: podName}), nil
}

// warmIfIdle is Warm/Discuss's shared implementation: returns ("", nil)
// if the task already has a live pod (a no-op, not an error — Discuss
// calls this unconditionally on every message), the new pod's name if it
// warmed one, or a typed connect error for anything else (unknown task,
// fleet at MAX_IN_FLIGHT_TASKS). Deliberately never touches tasks.status —
// status is a loose UI-freshness signal now, not control flow (sessions
// redesign, supersedes docs/adr/0021/0025's phase-boundary framing); pod
// lifecycle is pod_phase alone.
func (s *Server) warmIfIdle(ctx context.Context, taskID string) (podName string, err error) {
	t, err := s.tasks.GetTask(ctx, taskID)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	if t == nil {
		return "", connect.NewError(connect.CodeNotFound, errors.New("task not found"))
	}
	if tasks.IsPodPhaseLive(t.PodPhase) {
		return "", nil
	}
	// A still-'pending' task hasn't been claimed yet — dispatch.Loop's own
	// ClaimNextTask owns that first pod for every fresh task (it'll pick
	// this one up within one poll tick regardless). Warming it here too
	// would double-dispatch: both this call and the next dispatch tick
	// would call CreateWorkerPod for the same task independently, since
	// neither knows about the other's in-flight attempt. A silent no-op,
	// not an error, from this shared helper — Discuss (which calls this
	// unconditionally on every message) must still append the message
	// either way; Warm's own handler below gives an explicit rejection
	// instead, since a human clicking it deserves to know why nothing
	// happened. Once claimed (any other status), the task is exclusively
	// this function's territory.
	if t.Status == "pending" {
		return "", nil
	}
	// Accepted TOCTOU window (see tasks.Store.CountLivePods' own comment):
	// this whole function only ever runs from a low-frequency human action
	// (a click, or a typed message), never the hot dispatch loop.
	live, err := s.tasks.CountLivePods(ctx)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	if live >= s.maxInFlight {
		return "", connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("fleet at capacity (%d/%d warm pods)", live, s.maxInFlight))
	}
	repoCfg, err := s.repos.Get(ctx, t.Repo)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	if repoCfg == nil {
		return "", connect.NewError(connect.CodeInternal, fmt.Errorf("unknown repo %q", t.Repo))
	}
	resumeSessionID := ""
	if t.SessionID != nil {
		resumeSessionID = *t.SessionID
	}
	leaseID, err := s.tasks.RefreshLease(ctx, taskID)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	resumeFromSeq, err := s.transcr.LatestSeq(ctx, taskID)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	podName, err = s.e2e.CreateWorkerPod(ctx, taskID, t.Repo, repoCfg.URL, repoCfg.BaseBranch, t.Description, t.Guidance, leaseID, resumeSessionID, resumeFromSeq)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	return podName, nil
}

// DeleteTask force-tears-down any live session for taskID (both kinds —
// mirrors the same two calls coreserver/server.go's SetTaskStatus makes on
// a terminal status, just invoked directly instead of waiting for the
// worker pod to reach that code path, so a wedged/crashed pod doesn't
// block removal like Stop's cooperative abort-signal does) and then
// soft-deletes the task row. Doesn't touch status — see
// tasks.Store.SoftDelete's own comment.
func (s *Server) DeleteTask(ctx context.Context, req *connect.Request[agentfleetv1.DeleteTaskRequest]) (*connect.Response[agentfleetv1.DeleteTaskResponse], error) {
	taskID := req.Msg.GetTaskId()
	if _, err := s.e2e.TearDownSession(ctx, taskID, agentfleetv1.SessionKind_SESSION_KIND_WORKER); err != nil {
		slog.Error("dashboard DeleteTask: worker teardown", "taskId", taskID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if _, err := s.e2e.TearDownSession(ctx, taskID, agentfleetv1.SessionKind_SESSION_KIND_E2E); err != nil {
		slog.Error("dashboard DeleteTask: e2e teardown", "taskId", taskID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := s.tasks.SoftDelete(ctx, taskID); err != nil {
		slog.Error("dashboard DeleteTask: soft delete", "taskId", taskID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	slog.Info("dashboard DeleteTask", "taskId", taskID)
	return connect.NewResponse(&agentfleetv1.DeleteTaskResponse{Status: "deleted"}), nil
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
		slog.Error("dashboard ListWorktrees", "error", err)
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
			slog.Error("dashboard ListWorktrees: GetTaskStatusInfo", "taskId", w.GetTaskId(), "error", err)
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
		slog.Error("dashboard DeleteWorktree", "taskId", req.Msg.GetTaskId(), "repo", req.Msg.GetRepo(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.DeleteWorktreeResponse{Deleted: deleted}), nil
}

// GetJournal is the read path reliability-findings.md #1/#7 both call out
// as missing — knowledge_journal previously had no Get/List RPC anywhere.
func (s *Server) GetJournal(ctx context.Context, req *connect.Request[agentfleetv1.GetJournalRequest]) (*connect.Response[agentfleetv1.GetJournalResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = defaultListLimit
	}
	entries, err := s.journal.List(ctx, req.Msg.GetRepo(), req.Msg.GetSinceId(), limit)
	if err != nil {
		slog.Error("dashboard GetJournal", "repo", req.Msg.GetRepo(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*agentfleetv1.JournalEntry, len(entries))
	nextID := req.Msg.GetSinceId()
	for i, e := range entries {
		out[i] = journalEntryToProto(e)
		nextID = e.ID
	}
	return connect.NewResponse(&agentfleetv1.GetJournalResponse{Entries: out, NextId: nextID}), nil
}

// ListRepos/CreateRepo/UpdateRepo/DeleteRepo back the dashboard's "manage
// repos" UI (docs/adr/0028) — the DB-backed replacement for the hardcoded
// tasks.KnownRepos Go map. repos.Store.SetOnChange (wired in
// core/cmd/core/run.go) refreshes Discord's /task repo choices after every
// mutation here, so no redeploy or bot restart is needed either.

func (s *Server) ListRepos(ctx context.Context, _ *connect.Request[agentfleetv1.ListReposRequest]) (*connect.Response[agentfleetv1.ListReposResponse], error) {
	list, err := s.repos.List(ctx)
	if err != nil {
		slog.Error("dashboard ListRepos", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*agentfleetv1.Repo, len(list))
	for i, r := range list {
		out[i] = repoToProto(r)
	}
	return connect.NewResponse(&agentfleetv1.ListReposResponse{Repos: out}), nil
}

func (s *Server) CreateRepo(ctx context.Context, req *connect.Request[agentfleetv1.CreateRepoRequest]) (*connect.Response[agentfleetv1.CreateRepoResponse], error) {
	name := req.Msg.GetName()
	url := req.Msg.GetUrl()
	if name == "" || url == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name and url are required"))
	}
	r := repos.Repo{Name: name, URL: url, BaseBranch: req.Msg.GetBaseBranch()}
	if err := s.repos.Create(ctx, r); err != nil {
		if errors.Is(err, repos.ErrExists) {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		slog.Error("dashboard CreateRepo", "name", name, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.CreateRepoResponse{Repo: repoToProto(r)}), nil
}

func (s *Server) UpdateRepo(ctx context.Context, req *connect.Request[agentfleetv1.UpdateRepoRequest]) (*connect.Response[agentfleetv1.UpdateRepoResponse], error) {
	name := req.Msg.GetName()
	url := req.Msg.GetUrl()
	if name == "" || url == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name and url are required"))
	}
	r := repos.Repo{Name: name, URL: url, BaseBranch: req.Msg.GetBaseBranch()}
	if err := s.repos.Update(ctx, r); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown repo %q", name))
		}
		slog.Error("dashboard UpdateRepo", "name", name, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.UpdateRepoResponse{Repo: repoToProto(r)}), nil
}

func (s *Server) DeleteRepo(ctx context.Context, req *connect.Request[agentfleetv1.DeleteRepoRequest]) (*connect.Response[agentfleetv1.DeleteRepoResponse], error) {
	name := req.Msg.GetName()
	if err := s.repos.Delete(ctx, name); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown repo %q", name))
		}
		slog.Error("dashboard DeleteRepo", "name", name, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.DeleteRepoResponse{Status: "deleted"}), nil
}

func repoToProto(r repos.Repo) *agentfleetv1.Repo {
	return &agentfleetv1.Repo{Name: r.Name, Url: r.URL, BaseBranch: r.BaseBranch}
}

// ListPromptSnippets/CreatePromptSnippet/UpdatePromptSnippet/
// DeletePromptSnippet back the dashboard's "manage guidance" UI — the
// dashboard-editable replacement for worker/src/session.ts's old
// hardcoded taskPrompt() workflow text. Same shape as the repos CRUD
// above, no onChange wiring needed (unlike repos, nothing outside the
// dashboard itself reads this list live).

func (s *Server) ListPromptSnippets(ctx context.Context, _ *connect.Request[agentfleetv1.ListPromptSnippetsRequest]) (*connect.Response[agentfleetv1.ListPromptSnippetsResponse], error) {
	list, err := s.snippets.List(ctx)
	if err != nil {
		slog.Error("dashboard ListPromptSnippets", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*agentfleetv1.PromptSnippet, len(list))
	for i, sn := range list {
		out[i] = snippetToProto(sn)
	}
	return connect.NewResponse(&agentfleetv1.ListPromptSnippetsResponse{Snippets: out}), nil
}

func (s *Server) CreatePromptSnippet(ctx context.Context, req *connect.Request[agentfleetv1.CreatePromptSnippetRequest]) (*connect.Response[agentfleetv1.CreatePromptSnippetResponse], error) {
	name := req.Msg.GetName()
	text := req.Msg.GetText()
	if name == "" || text == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name and text are required"))
	}
	sn, err := s.snippets.Create(ctx, promptsnippets.Snippet{Name: name, Text: text})
	if err != nil {
		if errors.Is(err, promptsnippets.ErrExists) {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		slog.Error("dashboard CreatePromptSnippet", "name", name, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.CreatePromptSnippetResponse{Snippet: snippetToProto(sn)}), nil
}

func (s *Server) UpdatePromptSnippet(ctx context.Context, req *connect.Request[agentfleetv1.UpdatePromptSnippetRequest]) (*connect.Response[agentfleetv1.UpdatePromptSnippetResponse], error) {
	id := req.Msg.GetId()
	name := req.Msg.GetName()
	text := req.Msg.GetText()
	if id == "" || name == "" || text == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id, name, and text are required"))
	}
	sn := promptsnippets.Snippet{ID: id, Name: name, Text: text}
	if err := s.snippets.Update(ctx, sn); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown prompt snippet %q", id))
		}
		slog.Error("dashboard UpdatePromptSnippet", "id", id, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.UpdatePromptSnippetResponse{Snippet: snippetToProto(sn)}), nil
}

func (s *Server) DeletePromptSnippet(ctx context.Context, req *connect.Request[agentfleetv1.DeletePromptSnippetRequest]) (*connect.Response[agentfleetv1.DeletePromptSnippetResponse], error) {
	id := req.Msg.GetId()
	if err := s.snippets.Delete(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown prompt snippet %q", id))
		}
		slog.Error("dashboard DeletePromptSnippet", "id", id, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.DeletePromptSnippetResponse{Status: "deleted"}), nil
}

func snippetToProto(sn promptsnippets.Snippet) *agentfleetv1.PromptSnippet {
	return &agentfleetv1.PromptSnippet{
		Id:                      sn.ID,
		Name:                    sn.Name,
		Text:                    sn.Text,
		SuggestedPermissionMode: sn.SuggestedPermissionMode,
	}
}

// --- shared file space (docs/adr/0030) — core mints presigned URLs, the
// dashboard's browser moves the actual bytes directly against Garage ---

func (s *Server) ListFiles(ctx context.Context, _ *connect.Request[agentfleetv1.ListFilesRequest]) (*connect.Response[agentfleetv1.ListFilesResponse], error) {
	files, err := s.files.List(ctx)
	if err != nil {
		slog.Error("dashboard ListFiles", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*agentfleetv1.FileMetadata, len(files))
	for i, f := range files {
		out[i] = &agentfleetv1.FileMetadata{
			Key:          f.Key,
			SizeBytes:    f.SizeBytes,
			LastModified: f.LastModified.Format(time.RFC3339),
			ContentType:  f.ContentType,
		}
	}
	return connect.NewResponse(&agentfleetv1.ListFilesResponse{Files: out}), nil
}

func (s *Server) GetFileUploadUrl(ctx context.Context, req *connect.Request[agentfleetv1.GetFileUploadUrlRequest]) (*connect.Response[agentfleetv1.GetFileUploadUrlResponse], error) {
	url, key, expiresAt, err := s.files.PresignUpload(ctx, req.Msg.GetFilename(), req.Msg.GetContentType())
	if err != nil {
		slog.Error("dashboard GetFileUploadUrl", "filename", req.Msg.GetFilename(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.GetFileUploadUrlResponse{UploadUrl: url, Key: key, ExpiresAt: expiresAt.Format(time.RFC3339)}), nil
}

func (s *Server) GetFileDownloadUrl(ctx context.Context, req *connect.Request[agentfleetv1.GetFileDownloadUrlRequest]) (*connect.Response[agentfleetv1.GetFileDownloadUrlResponse], error) {
	url, expiresAt, err := s.files.PresignDownload(ctx, req.Msg.GetKey())
	if err != nil {
		slog.Error("dashboard GetFileDownloadUrl", "key", req.Msg.GetKey(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.GetFileDownloadUrlResponse{DownloadUrl: url, ExpiresAt: expiresAt.Format(time.RFC3339)}), nil
}

func (s *Server) DeleteFile(ctx context.Context, req *connect.Request[agentfleetv1.DeleteFileRequest]) (*connect.Response[agentfleetv1.DeleteFileResponse], error) {
	if err := s.files.Delete(ctx, req.Msg.GetKey()); err != nil {
		slog.Error("dashboard DeleteFile", "key", req.Msg.GetKey(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.DeleteFileResponse{Status: "deleted"}), nil
}

func journalEntryToProto(e journal.Entry) *agentfleetv1.JournalEntry {
	return &agentfleetv1.JournalEntry{
		Id:          e.ID,
		Repo:        e.Repo,
		Actor:       e.Actor,
		EventType:   e.EventType,
		PayloadJson: e.PayloadJSON,
		CreatedAt:   e.CreatedAt.Format(time.RFC3339),
	}
}

func TaskToProto(t tasks.Task) *agentfleetv1.Task {
	var heartbeatAt *string
	if t.HeartbeatAt != nil {
		s := t.HeartbeatAt.Format(time.RFC3339)
		heartbeatAt = &s
	}
	return &agentfleetv1.Task{
		Id:                t.ID,
		Repo:              t.Repo,
		Description:       t.Description,
		Status:            t.Status,
		ThreadId:          t.ThreadID,
		PrUrl:             t.PrURL,
		PodPhase:          t.PodPhase,
		PodMessage:        t.PodMessage,
		HeartbeatAt:       heartbeatAt,
		RetryCount:        int32(t.RetryCount),
		LastError:         t.LastError,
		SessionId:         t.SessionID,
		PermissionMode:    t.PermissionMode,
	}
}

func entryToProto(taskID string, e transcript.Entry) *agentfleetv1.TranscriptEntry {
	return &agentfleetv1.TranscriptEntry{
		TaskId:  taskID,
		Seq:     e.Seq,
		From:    e.From,
		Text:    e.Text,
		Type:    stringToProtoType(e.Type),
		ReplyTo: e.ReplyTo,
	}
}

// stringToProtoType maps the MCP wire's plain-string transcript type
// ("" | "discussion" | "approve" | "abort" | "question" | "answer" |
// "tool_call") to the enum this proto message uses — one direction only,
// nothing in this service ever needs enum->string.
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
	case "tool_call":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_TOOL_CALL
	case "system":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_SYSTEM
	case "assistant":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ASSISTANT
	case "user":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_USER
	case "result":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_RESULT
	case "permission_mode":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_MODE
	case "permission_request":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_REQUEST
	case "permission_response":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_RESPONSE
	default:
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED
	}
}

// QueryLogs queries Loki for log entries from fleet components or deployed
// apps. Used by the dashboard's Logs tab to display logs for a specific task,
// component, or deployed application. Returns structured log entries with
// timestamp, level, message, and other fields.
func (s *Server) QueryLogs(ctx context.Context, req *connect.Request[agentfleetv1.QueryLogsRequest]) (*connect.Response[agentfleetv1.QueryLogsResponse], error) {
	// Parse time strings (RFC3339)
	start, err := time.Parse(time.RFC3339, req.Msg.GetStartTime())
	if err != nil {
		slog.Error("dashboard QueryLogs: invalid start_time", "start_time", req.Msg.GetStartTime(), "error", err)
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid start_time: %w", err))
	}
	end, err := time.Parse(time.RFC3339, req.Msg.GetEndTime())
	if err != nil {
		slog.Error("dashboard QueryLogs: invalid end_time", "end_time", req.Msg.GetEndTime(), "error", err)
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid end_time: %w", err))
	}

	// Default namespace to agent-fleet if not specified
	namespace := req.Msg.GetNamespace()
	if namespace == "" {
		namespace = "agent-fleet"
	}

	// Default limit to 100 if not specified, cap at 1000
	limit := int(req.Msg.GetLimit())
	if limit == 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	// Query Loki via lokiclient
	entries, err := s.loki.Query(ctx, lokiclient.QueryRequest{
		TaskID:    req.Msg.GetTaskId(),
		Namespace: namespace,
		Component: req.Msg.GetComponent(),
		AppName:   req.Msg.GetAppName(),
		Level:     req.Msg.GetLevel(),
		Start:     start,
		End:       end,
		Limit:     limit,
	})
	if err != nil {
		slog.Error("dashboard QueryLogs: Loki query failed", "error", err)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("query Loki: %w", err))
	}

	// Convert to proto
	protoEntries := make([]*agentfleetv1.LogEntry, len(entries))
	for i, e := range entries {
		protoEntries[i] = &agentfleetv1.LogEntry{
			Timestamp:  e.Timestamp.Format(time.RFC3339),
			Level:      e.Level,
			Msg:        e.Msg,
			Component:  e.Component,
			PodName:    e.PodName,
			Namespace:  e.Namespace,
			FieldsJson: e.FieldsJSON,
		}
	}

	slog.Info("dashboard QueryLogs", "component", req.Msg.GetComponent(), "count", len(entries))
	return connect.NewResponse(&agentfleetv1.QueryLogsResponse{
		Entries:    protoEntries,
		TotalCount: int32(len(entries)),
	}), nil
}
