// Package coreserver implements agentfleet.v1.CoreService — core's first
// gRPC server (docs/adr/0020's Context: "nothing under fleet-core/ hosts
// one today"). Two kinds of callers: the provisioner (ReportPodEvents
// only, streaming pod-lifecycle events in) and every worker pod's sidecar
// (everything else — its local MCP server and wrapper-facing API both
// funnel through this one connection, docs/adr/0020 point 5). This is
// where every MCP tool handler that used to live in the now-deleted
// internal/mcpserver/ moved to, plus the direct-SQL calls that used to
// live in worker/src/db.ts — core is the fleet's sole Postgres-credential
// holder (docs/adr/0020 point 1).
package coreserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// Same poll interval today's internal/mcpserver used for wait_for_messages/
// AskUserQuestion's long-poll — preserved so both handlers behave exactly
// as before, just reached over gRPC instead of MCP-over-HTTP.
const pollInterval = 500 * time.Millisecond

type Server struct {
	agentfleetv1.UnimplementedCoreServiceServer
	transcr     transcript.Store
	tasks       *tasks.Store
	journal     *journal.Store
	provisioner *provisionerclient.Client
}

func New(transcr transcript.Store, taskStore *tasks.Store, journalStore *journal.Store, provisioner *provisionerclient.Client) *Server {
	return &Server{transcr: transcr, tasks: taskStore, journal: journalStore, provisioner: provisioner}
}

// --- agent-facing (proxied MCP-shaped calls) ---

// SendMessage ports internal/mcpserver/server.go's sendMessageHandler
// verbatim, just as a gRPC method instead of an MCP tool handler.
func (s *Server) SendMessage(ctx context.Context, req *agentfleetv1.SendMessageRequest) (*agentfleetv1.SendMessageResponse, error) {
	if req.GetTaskId() == "" || req.GetFrom() == "" || req.GetText() == "" {
		return nil, fmt.Errorf("task_id, from, and text are required")
	}
	seq, err := s.transcr.Append(ctx, req.GetTaskId(), req.GetFrom(), req.GetText(), protoTypeToString(req.GetType()), req.GetIdempotencyKey())
	if err != nil {
		return nil, fmt.Errorf("SendMessage: %w", err)
	}
	return &agentfleetv1.SendMessageResponse{Seq: seq}, nil
}

// WaitForMessages ports waitForMessagesHandler verbatim.
func (s *Server) WaitForMessages(ctx context.Context, req *agentfleetv1.ReadTranscriptSinceRequest) (*agentfleetv1.ReadTranscriptSinceResponse, error) {
	taskID := req.GetTaskId()
	if taskID == "" {
		return nil, fmt.Errorf("task_id is required")
	}
	timeoutMs := req.GetTimeoutMs()
	if timeoutMs <= 0 {
		timeoutMs = 30000
	}
	sinceSeq := req.GetSinceSeq()
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for {
		entries, nextSeq, err := s.transcr.ReadSince(ctx, taskID, sinceSeq, 1000)
		if err != nil {
			return nil, fmt.Errorf("WaitForMessages: %w", err)
		}
		if len(entries) > 0 || time.Now().After(deadline) {
			if len(entries) == 0 {
				nextSeq = sinceSeq
			}
			return &agentfleetv1.ReadTranscriptSinceResponse{Entries: entriesToProto(taskID, entries), NextSeq: nextSeq}, nil
		}
		select {
		case <-ctx.Done():
			return &agentfleetv1.ReadTranscriptSinceResponse{Entries: nil, NextSeq: sinceSeq}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// AskUserQuestion ports internal/mcpserver/ask_user_question.go's
// askUserQuestionHandler verbatim — same long-poll-for-a-matching-answer
// contract, same {"status":"pending"} re-invoke semantics (docs/adr/0018).
func (s *Server) AskUserQuestion(ctx context.Context, req *agentfleetv1.AskUserQuestionRequest) (*agentfleetv1.AskUserQuestionResponse, error) {
	if req.GetTaskId() == "" || req.GetQuestionsJson() == "" {
		return nil, fmt.Errorf("task_id and questions_json are required")
	}

	seq, err := s.transcr.Append(ctx, req.GetTaskId(), "agent", req.GetQuestionsJson(), "question", uuid.NewString())
	if err != nil {
		return nil, fmt.Errorf("AskUserQuestion: %w", err)
	}

	timeoutMs := req.GetTimeoutMs()
	if timeoutMs <= 0 {
		timeoutMs = 60000
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	cursor := seq

	for {
		entries, nextSeq, err := s.transcr.ReadSince(ctx, req.GetTaskId(), cursor, 1000)
		if err != nil {
			return nil, fmt.Errorf("AskUserQuestion: %w", err)
		}
		cursor = nextSeq
		for _, e := range entries {
			// ReplyTo must match this call's own question seq
			// (reliability-findings.md #0) — "any pending question + any
			// reply" (the old check) would let an unrelated answer or a
			// second concurrent question's answer satisfy this one.
			if e.From == "human" && e.Type == "answer" && e.ReplyTo != nil && *e.ReplyTo == seq {
				return &agentfleetv1.AskUserQuestionResponse{Status: "answered", AnswersJson: e.Text, QuestionSeq: seq}, nil
			}
		}
		if time.Now().After(deadline) {
			return &agentfleetv1.AskUserQuestionResponse{Status: "pending", QuestionSeq: seq}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// RequestE2EEnv/KillE2EEnv proxy to the provisioner (docs/adr/0020's
// hub-and-spoke rule — the sidecar never talks to the provisioner
// directly, even for e2e requests that used to be a direct MCP call).

func (s *Server) RequestE2EEnv(ctx context.Context, req *agentfleetv1.RequestE2EEnvRequest) (*agentfleetv1.RequestE2EEnvResponse, error) {
	t, err := s.tasks.GetTask(ctx, req.GetTaskId())
	if err != nil {
		return nil, fmt.Errorf("RequestE2EEnv: get task: %w", err)
	}
	if t == nil {
		return nil, fmt.Errorf("RequestE2EEnv: task %s not found", req.GetTaskId())
	}
	status, previewURL, err := s.provisioner.CreateE2eSession(ctx, req.GetTaskId(), t.Repo, req.GetStartCmd())
	if err != nil {
		return nil, fmt.Errorf("RequestE2EEnv: %w", err)
	}
	return &agentfleetv1.RequestE2EEnvResponse{Status: status, PreviewUrl: previewURL}, nil
}

func (s *Server) KillE2EEnv(ctx context.Context, req *agentfleetv1.KillE2EEnvRequest) (*agentfleetv1.KillE2EEnvResponse, error) {
	killed, err := s.provisioner.KillSession(ctx, req.GetTaskId(), uuid.NewString())
	if err != nil {
		return nil, fmt.Errorf("KillE2EEnv: %w", err)
	}
	return &agentfleetv1.KillE2EEnvResponse{Killed: killed}, nil
}

func (s *Server) ListE2ETools(ctx context.Context, req *agentfleetv1.ListE2EToolsRequest) (*agentfleetv1.ListE2EToolsResponse, error) {
	tools, err := s.provisioner.ListE2eTools(ctx, req.GetTaskId())
	if err != nil {
		return nil, fmt.Errorf("ListE2ETools: %w", err)
	}
	return &agentfleetv1.ListE2EToolsResponse{Tools: tools}, nil
}

func (s *Server) CallE2ETool(ctx context.Context, req *agentfleetv1.CallE2EToolRequest) (*agentfleetv1.CallE2EToolResponse, error) {
	resultJSON, isError, err := s.provisioner.CallE2eTool(ctx, req.GetTaskId(), req.GetToolName(), req.GetArgumentsJson())
	if err != nil {
		return nil, fmt.Errorf("CallE2ETool: %w", err)
	}
	return &agentfleetv1.CallE2EToolResponse{ResultJson: resultJSON, IsError: isError}, nil
}

// --- wrapper-facing (never agent-initiated, docs/adr/0020 point 5's third
// sidecar responsibility) — replaces worker/src/db.ts's direct SQL ---

func (s *Server) Heartbeat(ctx context.Context, req *agentfleetv1.HeartbeatRequest) (*agentfleetv1.HeartbeatResponse, error) {
	if err := s.tasks.UpdateHeartbeat(ctx, req.GetTaskId(), req.GetLeaseId()); err != nil {
		return nil, fmt.Errorf("Heartbeat: %w", err)
	}
	return &agentfleetv1.HeartbeatResponse{}, nil
}

// terminalTaskStatuses mirrors db/schema.sql's tasks_status_check values
// that end a task's lifecycle — the only statuses that should ever trigger
// teardown.
var terminalTaskStatuses = map[string]bool{
	"done": true, "failed": true, "cancelled": true, "failed_permanently": true,
}

func (s *Server) SetTaskStatus(ctx context.Context, req *agentfleetv1.SetTaskStatusRequest) (*agentfleetv1.SetTaskStatusResponse, error) {
	if err := s.tasks.SetStatus(ctx, req.GetTaskId(), req.GetStatus(), req.PrUrl, req.Notes, req.LastError); err != nil {
		return nil, fmt.Errorf("SetTaskStatus: %w", err)
	}

	// Opportunistic teardown trigger: core owns `tasks`, so it's the only
	// thing that can notice a task reaching a terminal state — the
	// provisioner no longer polls/joins against task status itself
	// (docs/adr/0020 point 1). Best-effort and unconditional for both kinds:
	// TearDownSession's own contract ("false = nothing to tear down") means
	// calling it for a task with no active worker pod, or no active e2e
	// session, is a correct no-op, not an error — cheaper than tracking
	// which kinds are actually active just to avoid a no-op call. Logged,
	// not propagated: a teardown hiccup shouldn't fail the status write
	// that's usually the last thing a finishing worker pod does.
	if terminalTaskStatuses[req.GetStatus()] {
		if _, err := s.provisioner.TearDownSession(ctx, req.GetTaskId(), agentfleetv1.SessionKind_SESSION_KIND_WORKER); err != nil {
			slog.Warn("SetTaskStatus: worker teardown failed", "taskId", req.GetTaskId(), "error", err)
		}
		if _, err := s.provisioner.TearDownSession(ctx, req.GetTaskId(), agentfleetv1.SessionKind_SESSION_KIND_E2E); err != nil {
			slog.Warn("SetTaskStatus: e2e teardown failed", "taskId", req.GetTaskId(), "error", err)
		}
	}

	return &agentfleetv1.SetTaskStatusResponse{}, nil
}

func (s *Server) AppendJournal(ctx context.Context, req *agentfleetv1.AppendJournalRequest) (*agentfleetv1.AppendJournalResponse, error) {
	if err := s.journal.Append(ctx, req.GetRepo(), req.GetActor(), req.GetEventType(), req.GetPayloadJson()); err != nil {
		return nil, fmt.Errorf("AppendJournal: %w", err)
	}
	return &agentfleetv1.AppendJournalResponse{}, nil
}

func (s *Server) SaveSessionId(ctx context.Context, req *agentfleetv1.SaveSessionIdRequest) (*agentfleetv1.SaveSessionIdResponse, error) {
	if err := s.tasks.SaveSessionID(ctx, req.GetTaskId(), req.GetSessionId(), req.GetModel()); err != nil {
		return nil, fmt.Errorf("SaveSessionId: %w", err)
	}
	return &agentfleetv1.SaveSessionIdResponse{}, nil
}

func (s *Server) StillHoldsLease(ctx context.Context, req *agentfleetv1.StillHoldsLeaseRequest) (*agentfleetv1.StillHoldsLeaseResponse, error) {
	holds, err := s.tasks.StillHoldsLease(ctx, req.GetTaskId(), req.GetLeaseId())
	if err != nil {
		return nil, fmt.Errorf("StillHoldsLease: %w", err)
	}
	return &agentfleetv1.StillHoldsLeaseResponse{Holds: holds}, nil
}

// PushToolTelemetry persists the sidecar's independently-scheduled git
// diff/branch/tool-call-summary telemetry as a TOOL_CALL transcript entry
// (docs/adr/0020 point 5's second bullet) — never relayed to Discord
// (internal/transcript/relay.go's relayPending skips this type).
func (s *Server) PushToolTelemetry(ctx context.Context, req *agentfleetv1.PushToolTelemetryRequest) (*agentfleetv1.PushToolTelemetryResponse, error) {
	if _, err := s.transcr.Append(ctx, req.GetTaskId(), "sidecar", req.GetSummaryJson(), "tool_call", uuid.NewString()); err != nil {
		return nil, fmt.Errorf("PushToolTelemetry: %w", err)
	}
	return &agentfleetv1.PushToolTelemetryResponse{}, nil
}

// StreamHumanMessages is the mechanism that lets the sidecar deliver new
// human input to the wrapper live, for streamInput() (docs/adr/0021 point
// 2) — a genuine live feed, not a poll the wrapper initiates on its own
// schedule. Filters to from=="human": echoing the agent's own
// send_message posts back as "new input" would double-feed the agent's own
// words back to itself as a user turn.
func (s *Server) StreamHumanMessages(req *agentfleetv1.StreamHumanMessagesRequest, stream agentfleetv1.CoreService_StreamHumanMessagesServer) error {
	ctx := stream.Context()
	taskID := req.GetTaskId()
	cursor := req.GetSinceSeq()

	for {
		entries, nextSeq, err := s.transcr.ReadSince(ctx, taskID, cursor, 100)
		if err != nil {
			return fmt.Errorf("StreamHumanMessages: %w", err)
		}
		cursor = nextSeq
		for _, e := range entries {
			if e.From != "human" {
				continue
			}
			if err := stream.Send(entryToProto(taskID, e)); err != nil {
				return err // client (sidecar) disconnected
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pollInterval):
		}
	}
}

// --- provisioner-facing (pushed, not polled, docs/adr/0020 point 3) ---

// ReportPodEvents ingests the provisioner's pushed pod-lifecycle events
// (created/scheduled/running/crashed/terminated) into knowledge_journal
// for logging/dashboard/future use. It is deliberately NOT the source of
// truth for dispatch concurrency headroom — see internal/dispatch, which
// derives that atomically inside tasks.ClaimNextTask's own claim query
// (Postgres, survives a core restart; this stream's in-memory state would
// not).
func (s *Server) ReportPodEvents(stream agentfleetv1.CoreService_ReportPodEventsServer) error {
	ctx := stream.Context()
	for {
		event, err := stream.Recv()
		if err != nil {
			// io.EOF (client closed the stream cleanly) or a real transport
			// error both end this call the same way — either return isn't a
			// backend error worth propagating specially, matching how a
			// pushed telemetry stream shouldn't crash the provisioner's own
			// caller loop.
			return stream.SendAndClose(&agentfleetv1.ReportPodEventsResponse{})
		}
		payload, marshalErr := json.Marshal(map[string]any{
			"taskId":  event.GetTaskId(),
			"kind":    event.GetKind().String(),
			"phase":   event.GetPhase().String(),
			"podName": event.GetPodName(),
			"message": event.GetMessage(),
		})
		if marshalErr != nil {
			return fmt.Errorf("ReportPodEvents: marshal event: %w", marshalErr)
		}
		if err := s.journal.Append(ctx, "", "provisioner", "pod."+event.GetPhase().String(), string(payload)); err != nil {
			return fmt.Errorf("ReportPodEvents: %w", err)
		}

		// Live worker-pod state for the dashboard (separate from the
		// crash-reclaim fast-path below) — only worker pods have a task to
		// attach state to; e2e pods have no matching tasks row.
		if event.GetKind() == agentfleetv1.SessionKind_SESSION_KIND_WORKER {
			if err := s.tasks.SetPodPhase(ctx, event.GetTaskId(), event.GetPhase().String(), event.GetMessage()); err != nil {
				slog.Error("ReportPodEvents: set pod phase failed", "taskId", event.GetTaskId(), "error", err)
			}
		}

		// Fast-path accelerant on top of the heartbeat-reclaim fallback
		// (reliability-findings.md #1) — without this, a mid-task crash is
		// invisible to core for up to the full 10-minute staleness window.
		// MarkCrashed only touches a non-terminal task itself, so this is a
		// safe no-op if the task already reached done/failed/cancelled
		// through its own SetTaskStatus call before the provisioner's
		// reconcile loop noticed the crash.
		if event.GetKind() == agentfleetv1.SessionKind_SESSION_KIND_WORKER && event.GetPhase() == agentfleetv1.PodPhase_POD_PHASE_CRASHED {
			if err := s.tasks.MarkCrashed(ctx, event.GetTaskId()); err != nil {
				slog.Error("ReportPodEvents: mark crashed failed", "taskId", event.GetTaskId(), "error", err)
			}
		}
	}
}

// --- helpers ---

func entriesToProto(taskID string, entries []transcript.Entry) []*agentfleetv1.TranscriptEntry {
	out := make([]*agentfleetv1.TranscriptEntry, len(entries))
	for i, e := range entries {
		out[i] = entryToProto(taskID, e)
	}
	return out
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

func protoTypeToString(t agentfleetv1.TranscriptEntryType) string {
	switch t {
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_DISCUSSION:
		return "discussion"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_APPROVE:
		return "approve"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ABORT:
		return "abort"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_QUESTION:
		return "question"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ANSWER:
		return "answer"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_TOOL_CALL:
		return "tool_call"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_SYSTEM:
		return "system"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_ASSISTANT:
		return "assistant"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_USER:
		return "user"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_RESULT:
		return "result"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_MODE:
		return "permission_mode"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_REQUEST:
		return "permission_request"
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_PERMISSION_RESPONSE:
		return "permission_response"
	default:
		return ""
	}
}
