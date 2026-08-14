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
	"strings"
	"time"

	"github.com/google/uuid"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/filestore"
	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/lokiclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// Same poll interval today's internal/mcpserver used for wait_for_messages/
// AskUserQuestion's long-poll — preserved so both handlers behave exactly
// as before, just reached over gRPC instead of MCP-over-HTTP.
const pollInterval = 500 * time.Millisecond

type Server struct {
	agentfleetv1.UnimplementedCoreServiceServer
	transcr  transcript.Store
	sessions *sessions.Store
	journal  *journal.Store

	// repos supplies the per-repo cluster_access grant (docs/adr/0037) —
	// the one ingredient that outlived the recipe system.
	repos       *repos.Store
	provisioner *provisionerclient.Client
	files       filestore.Store
	loki        lokiclient.Querier
	// warm boots a pod for an idle session. Injected rather than
	// reimplemented: dashboard.Server.warmIfIdle is the only path to a
	// worker pod outside the dispatch loop, and it carries real rules
	// (capacity cap, the 'proposed'/'pending' gates that stop an
	// unapproved task ever getting a pod). A second copy here would be a
	// second dispatch implementation to keep in sync. nil disables
	// warm-on-prompt, which only makes PromptSession fail for idle
	// targets.
	warm func(ctx context.Context, taskID string) (string, error)
}

func New(transcr transcript.Store, sessionStore *sessions.Store, journalStore *journal.Store, repoStore *repos.Store, provisioner *provisionerclient.Client, files filestore.Store, loki lokiclient.Querier) *Server {
	return &Server{transcr: transcr, sessions: sessionStore, journal: journalStore, repos: repoStore, provisioner: provisioner, files: files, loki: loki}
}

// SetWarmFunc wires the shared warm-an-idle-session path in after
// construction — dashboard.Server is built later than this one in
// cmd/core/run.go, and inverting that order would be a bigger change than
// a setter.
func (s *Server) SetWarmFunc(warm func(ctx context.Context, taskID string) (string, error)) {
	s.warm = warm
}

// --- agent-facing (proxied MCP-shaped calls) ---

// SendMessage ports internal/mcpserver/server.go's sendMessageHandler
// verbatim, just as a gRPC method instead of an MCP tool handler.
func (s *Server) SendMessage(ctx context.Context, req *agentfleetv1.SendMessageRequest) (*agentfleetv1.AppendResponse, error) {
	if req.GetSessionId() == "" || req.GetFrom() == "" || req.GetText() == "" {
		return nil, fmt.Errorf("task_id, from, and text are required")
	}
	// AppendReply only when the caller actually asked for correlation —
	// same reason transcript keeps these as two methods rather than one
	// that takes a meaningless zero.
	var seq int64
	var err error
	if req.ReplyToSeq != nil {
		seq, err = s.transcr.AppendReply(ctx, req.GetSessionId(), req.GetFrom(), req.GetText(), protoTypeToString(req.GetType()), req.GetIdempotencyKey(), req.GetReplyToSeq())
	} else {
		seq, err = s.transcr.Append(ctx, req.GetSessionId(), req.GetFrom(), req.GetText(), protoTypeToString(req.GetType()), req.GetIdempotencyKey())
	}
	if err != nil {
		return nil, fmt.Errorf("SendMessage: %w", err)
	}
	return &agentfleetv1.AppendResponse{Seq: seq}, nil
}

// WaitForMessages ports waitForMessagesHandler verbatim.
func (s *Server) WaitForMessages(ctx context.Context, req *agentfleetv1.ReadTranscriptSinceRequest) (*agentfleetv1.ReadTranscriptSinceResponse, error) {
	taskID := req.GetSessionId()
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
	if req.GetSessionId() == "" || req.GetQuestionsJson() == "" {
		return nil, fmt.Errorf("task_id and questions_json are required")
	}

	seq, err := s.transcr.Append(ctx, req.GetSessionId(), "agent", req.GetQuestionsJson(), "question", uuid.NewString())
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
		entries, nextSeq, err := s.transcr.ReadSince(ctx, req.GetSessionId(), cursor, 1000)
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
				return &agentfleetv1.AskUserQuestionResponse{Answered: true, AnswersJson: e.Text, QuestionSeq: seq}, nil
			}
		}
		if time.Now().After(deadline) {
			return &agentfleetv1.AskUserQuestionResponse{Answered: false, QuestionSeq: seq}, nil
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
	t, err := s.sessions.Get(ctx, req.GetSessionId())
	if err != nil {
		return nil, fmt.Errorf("RequestE2EEnv: get task: %w", err)
	}
	if t == nil {
		return nil, fmt.Errorf("RequestE2EEnv: task %s not found", req.GetSessionId())
	}
	// There is no recipe to resolve any more (docs/adr/0048). The agent's
	// own start command is whatever it decides to run in its own shell, so
	// the caller's override is now the ONLY source — an empty one means a
	// sandbox with no app, which docs/adr/0044 already established is a
	// working sandbox rather than a failed pod.
	toolKeys, err := s.toolKeysFor(ctx, t.Repo)
	if err != nil {
		return nil, fmt.Errorf("RequestE2EEnv: %w", err)
	}
	created, err := s.provisioner.CreateE2eSession(ctx, req.GetSessionId(), t.Repo, req.GetStartCmd(), toolKeys)
	if err != nil {
		return nil, fmt.Errorf("RequestE2EEnv: %w", err)
	}
	resp := &agentfleetv1.RequestE2EEnvResponse{
		Status:           created.GetStatus(),
		PreviewUrl:       created.GetPreviewUrl(),
		ResolvedStartCmd: req.GetStartCmd(),
		Tools:            toolKeys,
		// Forwarded byte-for-byte from the provisioner (docs/adr/0045). core
		// does not build, cache, validate or interpret these — it does not
		// know what MCP is. That is what makes this a field passthrough
		// rather than the call passthrough it replaces.
		Endpoints: created.GetEndpoints(),
	}
	return resp, nil
}

// toolKeysFor mirrors the dashboard's helper of the same name: the only
// ingredient that survived docs/adr/0034's recipe system is cluster-access,
// because it is a privilege grant rather than a toolchain (docs/adr/0037).
func (s *Server) toolKeysFor(ctx context.Context, repo string) ([]string, error) {
	r, err := s.repos.Get(ctx, repo)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, nil
	}
	return provisionerclient.ToolKeysFor(r.ClusterAccess), nil
}

func (s *Server) KillE2EEnv(ctx context.Context, req *agentfleetv1.KillE2EEnvRequest) (*agentfleetv1.KillE2EEnvResponse, error) {
	killed, _, err := s.provisioner.KillSession(ctx, req.GetSessionId(), uuid.NewString(), "", false)
	if err != nil {
		return nil, fmt.Errorf("KillE2EEnv: %w", err)
	}
	return &agentfleetv1.KillE2EEnvResponse{Killed: killed}, nil
}

// --- shared file space (docs/adr/0030) — core mints presigned URLs, the
// agent's own Bash `curl` moves the actual bytes ---

func (s *Server) ListFiles(ctx context.Context, _ *agentfleetv1.ListFilesRequest) (*agentfleetv1.ListFilesResponse, error) {
	files, err := s.files.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListFiles: %w", err)
	}
	return &agentfleetv1.ListFilesResponse{Files: filesToProto(files)}, nil
}

func (s *Server) GetFileUploadUrl(ctx context.Context, req *agentfleetv1.GetFileUploadUrlRequest) (*agentfleetv1.GetFileUploadUrlResponse, error) {
	url, key, expiresAt, err := s.files.PresignUpload(ctx, req.GetFilename(), req.GetContentType())
	if err != nil {
		return nil, fmt.Errorf("GetFileUploadUrl: %w", err)
	}
	return &agentfleetv1.GetFileUploadUrlResponse{UploadUrl: url, Key: key, ExpiresAt: expiresAt.Format(time.RFC3339)}, nil
}

func (s *Server) GetFileDownloadUrl(ctx context.Context, req *agentfleetv1.GetFileDownloadUrlRequest) (*agentfleetv1.GetFileDownloadUrlResponse, error) {
	url, expiresAt, err := s.files.PresignDownload(ctx, req.GetKey())
	if err != nil {
		return nil, fmt.Errorf("GetFileDownloadUrl: %w", err)
	}
	return &agentfleetv1.GetFileDownloadUrlResponse{DownloadUrl: url, ExpiresAt: expiresAt.Format(time.RFC3339)}, nil
}

func (s *Server) DeleteFile(ctx context.Context, req *agentfleetv1.DeleteFileRequest) (*agentfleetv1.DeleteFileResponse, error) {
	if err := s.files.Delete(ctx, req.GetKey()); err != nil {
		return nil, fmt.Errorf("DeleteFile: %w", err)
	}
	return &agentfleetv1.DeleteFileResponse{}, nil
}

// --- wrapper-facing (never agent-initiated, docs/adr/0020 point 5's third
// sidecar responsibility) — replaces worker/src/db.ts's direct SQL ---

// GetTask lets a worker pod fetch its own fresh task row on startup instead
// of relying on stale environment variables. Same message shapes as
// DashboardService.GetTask (core.proto's comment on Task/GetSessionRequest),
// reusing its taskToProto mapper rather than duplicating the field list.
func (s *Server) GetTask(ctx context.Context, req *agentfleetv1.GetSessionRequest) (*agentfleetv1.GetSessionResponse, error) {
	t, err := s.sessions.Get(ctx, req.GetId())
	if err != nil {
		return nil, fmt.Errorf("GetTask: %w", err)
	}
	if t == nil {
		return nil, fmt.Errorf("GetTask: task %s not found", req.GetId())
	}
	return &agentfleetv1.GetSessionResponse{Session: dashboard.SessionToProto(*t)}, nil
}

// SetPermissionMode persists a worker pod's own permission mode (the
// initial "default" on startup, or a change it made itself) — a plain
// column write. Unlike DashboardService.SetPermissionMode, it does not
// append a transcript entry: there's no other running worker to notify,
// the caller *is* the session whose mode this is.
func (s *Server) SetPermissionMode(ctx context.Context, req *agentfleetv1.SetPermissionModeRequest) (*agentfleetv1.SetPermissionModeResponse, error) {
	if err := s.sessions.SetPermissionMode(ctx, req.GetSessionId(), req.GetMode()); err != nil {
		return nil, fmt.Errorf("SetPermissionMode: %w", err)
	}
	return &agentfleetv1.SetPermissionModeResponse{}, nil
}

// Heartbeat and SetTaskStatus used to live here, and their absence is the
// single biggest behavioural change in docs/adr/0048.
//
// Heartbeat refreshed a timestamp every 30s from inside the worker. It only
// ever proved a timer was still firing — it could not tell a healthy session
// from a wedged one, and it was the pod being asked to report on itself.
// Liveness now reconciles pod_phase against Kubernetes, which actually knows
// whether a pod exists.
//
// SetTaskStatus went with the `status` column. It was also the fleet's only
// TearDownSession trigger, which is why deleting it required teaching the
// provisioner's reconcile pass to report POD_PHASE_TERMINATED for a Succeeded
// Job — without that, a finished session would hold a slot forever and the
// fleet would wedge after five. Teardown now happens in three places that all
// observe reality rather than trusting a self-report: the reconcile loop, the
// idle/stall sweeps, and ArchiveSession.

func (s *Server) AppendJournal(ctx context.Context, req *agentfleetv1.AppendJournalRequest) (*agentfleetv1.AppendJournalResponse, error) {
	if err := s.journal.Append(ctx, req.GetRepo(), req.GetActor(), req.GetEventType(), req.GetPayloadJson()); err != nil {
		return nil, fmt.Errorf("AppendJournal: %w", err)
	}
	return &agentfleetv1.AppendJournalResponse{}, nil
}

func (s *Server) SearchJournal(ctx context.Context, req *agentfleetv1.SearchJournalRequest) (*agentfleetv1.SearchJournalResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 50
	}
	entries, err := s.journal.Search(ctx, req.GetRepo(), req.GetQuery(), limit)
	if err != nil {
		return nil, fmt.Errorf("SearchJournal: %w", err)
	}
	out := make([]*agentfleetv1.JournalEntry, len(entries))
	for i, e := range entries {
		out[i] = &agentfleetv1.JournalEntry{
			Id:          e.ID,
			Repo:        e.Repo,
			Actor:       e.Actor,
			EventType:   e.EventType,
			PayloadJson: e.PayloadJSON,
			CreatedAt:   e.CreatedAt.Format(time.RFC3339),
		}
	}
	return &agentfleetv1.SearchJournalResponse{Entries: out}, nil
}

func (s *Server) SaveSessionId(ctx context.Context, req *agentfleetv1.SaveAgentSessionIdRequest) (*agentfleetv1.SaveAgentSessionIdResponse, error) {
	saved, err := s.sessions.SaveAgentSessionID(ctx, req.GetSessionId(), req.GetSessionId(), req.GetModel(), req.GetLeaseId())
	if err == nil && !saved {
		// The pod that sent this no longer holds the lease — it was torn
		// down and another pod owns the session now. Dropping the write is
		// the correct outcome (docs/adr/0041): tasks.session_id is what the
		// next resume reads, and letting a dead pod set it would resume the
		// wrong conversation.
		slog.Warn("coreserver: ignored SaveSessionId from a pod that no longer holds the lease",
			"sessionId", req.GetSessionId(), "sessionId", req.GetSessionId())
	}
	if err != nil {
		return nil, fmt.Errorf("SaveSessionId: %w", err)
	}
	return &agentfleetv1.SaveAgentSessionIdResponse{}, nil
}

// StillHoldsLease used to live here — a pre-push check against a stale pod
// finishing work after a reclaim respawned a fresh one. Deleted in
// docs/adr/0048 along with the reclaim that created that race. It had no
// production caller either way: only worker test fakes referenced it.

// PushToolTelemetry persists the sidecar's independently-scheduled git
// diff/branch/tool-call-summary telemetry as a TOOL_CALL transcript entry
// (docs/adr/0020 point 5's second bullet) — never relayed to Discord
// (internal/transcript/relay.go's relayPending skips this type).
func (s *Server) PushToolTelemetry(ctx context.Context, req *agentfleetv1.PushToolTelemetryRequest) (*agentfleetv1.PushToolTelemetryResponse, error) {
	if _, err := s.transcr.Append(ctx, req.GetSessionId(), "sidecar", req.GetSummaryJson(), "tool_call", uuid.NewString()); err != nil {
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
	taskID := req.GetSessionId()
	cursor := req.GetSinceSeq()

	for {
		entries, nextSeq, err := s.transcr.ReadSince(ctx, taskID, cursor, 100)
		if err != nil {
			return fmt.Errorf("StreamHumanMessages: %w", err)
		}
		cursor = nextSeq
		for _, e := range entries {
			// "human" is the operator; "session" is another session's
			// prompt (docs/adr/0041). Everything else — crucially the
			// target's own from="agent" output — must not be streamed back,
			// or a session feeds itself forever.
			if e.From != "human" && e.From != "session" {
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
			"sessionId": event.GetSessionId(),
			"kind":      event.GetKind().String(),
			"phase":     event.GetPhase().String(),
			"podName":   event.GetPodName(),
			"message":   event.GetMessage(),
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
			if err := s.sessions.SetPodPhase(ctx, event.GetSessionId(), event.GetPhase().String(), event.GetMessage()); err != nil {
				slog.Error("ReportPodEvents: set pod phase failed", "sessionId", event.GetSessionId(), "error", err)
			}
		}

		// MarkCrashed used to run here as a fast-path accelerant on top of the
		// heartbeat-reclaim fallback: it backdated heartbeat_at so
		// ClaimNextTask would reclaim the task without waiting out the full
		// 10-minute staleness window.
		//
		// Both the heartbeat and the reclaim are gone (docs/adr/0048), and
		// with them the reason for a fast path. The SetPodPhase write above
		// IS the crash report now — pod_phase CRASHED frees the session's
		// slot, surfaces as `failing` in the topology, and is what a human
		// acts on. Nothing needs to be accelerated toward.
		//
		// The one thing that had to be added for this to hold is on the
		// provisioner side: gcTerminalWorkerJobs now reports TERMINATED for a
		// Succeeded Job too, not just CRASHED for a Failed one. Without that,
		// a cleanly finished session would keep its slot forever.
	}
}

// --- helpers ---

func filesToProto(files []filestore.FileMetadata) []*agentfleetv1.FileMetadata {
	out := make([]*agentfleetv1.FileMetadata, len(files))
	for i, f := range files {
		out[i] = &agentfleetv1.FileMetadata{
			Key:          f.Key,
			SizeBytes:    f.SizeBytes,
			LastModified: f.LastModified.Format(time.RFC3339),
			ContentType:  f.ContentType,
		}
	}
	return out
}

func entriesToProto(taskID string, entries []transcript.Entry) []*agentfleetv1.TranscriptEntry {
	out := make([]*agentfleetv1.TranscriptEntry, len(entries))
	for i, e := range entries {
		out[i] = entryToProto(taskID, e)
	}
	return out
}

func entryToProto(taskID string, e transcript.Entry) *agentfleetv1.TranscriptEntry {
	return &agentfleetv1.TranscriptEntry{
		SessionId: taskID,
		Seq:       e.Seq,
		From:      e.From,
		Text:      e.Text,
		Type:      stringToProtoType(e.Type),
		ReplyTo:   e.ReplyTo,
		// Zero time would serialize as year 1 — send "" so a client can
		// tell "no timestamp" from "the epoch".
		CreatedAt: transcript.RFC3339OrEmpty(e.CreatedAt),
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
	case "interrupt":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_INTERRUPT
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
	case agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_INTERRUPT:
		return "interrupt"
	default:
		return ""
	}
}

// --- Log querying (Loki queries, docs/adr/0013) ---

// ViewLogs queries Loki for logs and returns formatted text for agent consumption.
// Supports both duration-based queries (e.g., "last 1h") and explicit timestamp ranges.
func (s *Server) ViewLogs(ctx context.Context, req *agentfleetv1.ViewLogsRequest) (*agentfleetv1.ViewLogsResponse, error) {
	if req.GetComponent() == "" {
		return nil, fmt.Errorf("component is required")
	}

	// Extract taskId from context metadata (sidecar includes it)
	taskID := extractTaskIDFromMetadata(ctx)

	// Determine time range: explicit timestamps override duration
	var start, end time.Time
	if req.GetStartTime() != "" {
		// Parse explicit start timestamp (RFC3339)
		var err error
		start, err = time.Parse(time.RFC3339, req.GetStartTime())
		if err != nil {
			return nil, fmt.Errorf("invalid start_time (must be RFC3339): %w", err)
		}
	} else {
		// Use duration to calculate start time
		duration := parseDuration(req.GetDuration(), time.Hour) // default 1h
		start = time.Now().Add(-duration)
	}

	if req.GetEndTime() != "" {
		// Parse explicit end timestamp (RFC3339)
		var err error
		end, err = time.Parse(time.RFC3339, req.GetEndTime())
		if err != nil {
			return nil, fmt.Errorf("invalid end_time (must be RFC3339): %w", err)
		}
	} else {
		// Default to now
		end = time.Now()
	}

	// Apply defaults
	namespace := req.GetNamespace()
	if namespace == "" {
		namespace = "agent-fleet"
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}

	// Query Loki
	entries, err := s.loki.Query(ctx, lokiclient.QueryRequest{
		TaskID:    taskID,
		Namespace: namespace,
		Component: req.GetComponent(),
		AppName:   req.GetAppName(),
		Level:     req.GetLevel(),
		Start:     start,
		End:       end,
		Limit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("query Loki: %w", err)
	}

	// Format for agent (table with timestamp, level, message)
	text := formatLogsForAgent(entries)
	return &agentfleetv1.ViewLogsResponse{LogsText: text}, nil
}

// extractTaskIDFromMetadata extracts task_id from gRPC metadata.
// The sidecar includes this in every call.
func extractTaskIDFromMetadata(ctx context.Context) string {
	// TODO: Extract from gRPC metadata when sidecar implementation is done
	// For now, return empty - this will cause Loki to query all pods
	// of the specified component
	return ""
}

// parseDuration parses a duration string like "1h", "30m", "24h".
func parseDuration(s string, defaultDur time.Duration) time.Duration {
	if s == "" {
		return defaultDur
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		slog.Warn("invalid duration, using default", "duration", s, "default", defaultDur)
		return defaultDur
	}
	return d
}

// formatLogsForAgent formats log entries compactly for agent consumption.
// Anything constant across the whole result — the pod name, the date — is
// hoisted into one header line instead of being restated on every row: the old
// table spent ~45 characters per line repeating them, which for a typical e2e
// pod dump was more tokens than the log content itself. Blank-message rows are
// dropped for the same reason.
func formatLogsForAgent(entries []lokiclient.LogEntry) string {
	rows := make([]lokiclient.LogEntry, 0, len(entries))
	pods := map[string]struct{}{}
	dates := map[string]struct{}{}
	for _, e := range entries {
		if strings.TrimSpace(e.Msg) == "" {
			continue
		}
		rows = append(rows, e)
		pods[e.PodName] = struct{}{}
		dates[e.Timestamp.Format("2006-01-02")] = struct{}{}
	}
	if len(rows) == 0 {
		return "No logs found."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d entries", len(rows))
	if len(pods) == 1 {
		fmt.Fprintf(&b, " · %s", rows[0].PodName)
	}
	if len(dates) == 1 {
		fmt.Fprintf(&b, " · %s", rows[0].Timestamp.Format("2006-01-02"))
	}
	b.WriteString("\n")

	// Only carry the date per-row when the result actually spans days.
	layout := "15:04:05"
	if len(dates) > 1 {
		layout = "01-02 15:04:05"
	}
	for _, e := range rows {
		level := strings.ToLower(e.Level)
		if level == "" {
			level = "info"
		}
		msg := strings.TrimSpace(e.Msg)
		if len(msg) > 80 {
			msg = msg[:77] + "..."
		}
		fmt.Fprintf(&b, "%s %-5s %s", e.Timestamp.Format(layout), level, msg)
		if len(pods) > 1 {
			podName := e.PodName
			if len(podName) > 19 {
				podName = podName[:16] + "..."
			}
			fmt.Fprintf(&b, " [%s]", podName)
		}
		b.WriteString("\n")
	}
	return b.String()
}
