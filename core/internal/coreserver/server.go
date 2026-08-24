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
	"github.com/MohammadBnei/agent-fleet/core/internal/filediff"
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

	// notifyBlocked posts "this session needs a human" out of band, wired to
	// discord.Client.NotifyBlocked. Injected as a func rather than the client
	// so this package keeps no Discord dependency, matching warm above.
	//
	// This is the fleet's ONLY outbound notification for a blocked session.
	// Without it a session stalls on a permission decision and tells nobody —
	// the only way to find it is to already be looking at the dashboard, which
	// is exactly backwards for the one event that needs a human. The transcript
	// relay that used to carry this was deleted with the Discord-as-console
	// model; the notification is not part of that, and should not have gone
	// with it. nil disables notification and nothing else.
	notifyBlocked func(ctx context.Context, sessionID, repo, title, summary string) error

	// fileDiffs is the parking lot the dashboard drops "diff this path" into
	// and PushToolTelemetry below drains. nil disables the feature and
	// nothing else: the console falls back to the captured-tool-input diff
	// it has always had. Injected after construction like warm and
	// notifyBlocked, because dashboard.Server is built later.
	fileDiffs *filediff.Store
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

// SetNotifyBlockedFunc wires the out-of-band "needs a human" notification.
// Optional: nil leaves the dashboard as the only way to notice.
func (s *Server) SetNotifyBlockedFunc(fn func(ctx context.Context, sessionID, repo, title, summary string) error) {
	s.notifyBlocked = fn
}

// announceBlocked fires the out-of-band notification for a session that has
// just started waiting on a human.
//
// Best-effort and deliberately non-fatal: the decision is already durable in
// the transcript and visible on the dashboard, so a Discord outage must not
// fail the agent's call and leave it unable to ask at all.
func (s *Server) announceBlocked(ctx context.Context, sessionID, summary string) {
	if s.notifyBlocked == nil {
		return
	}
	repo, title := "", ""
	if sess, err := s.sessions.Get(ctx, sessionID); err == nil && sess != nil {
		repo = sess.Repo
		if sess.Title != nil {
			title = *sess.Title
		}
	}
	if err := s.notifyBlocked(ctx, sessionID, repo, title, summary); err != nil {
		slog.Warn("coreserver: blocked-session notification failed", "sessionId", sessionID, "error", err)
	}
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
	// A permission request is the moment a session stops making progress and
	// starts waiting on a person, so it is the moment worth telling them about.
	if protoTypeToString(req.GetType()) == "permission_request" {
		s.announceBlocked(ctx, req.GetSessionId(), "needs a permission decision")
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

	// On timeout this tool returns {"status":"pending"} and the agent
	// re-invokes with the *same* questions to keep waiting. A naive Append
	// per call mints a new question seq each time, so the human's answer to
	// the card they see (an earlier seq) never matches the seq this call
	// polls — the answer is silently lost and the poll times out forever
	// (plus a duplicate question card per re-invoke). Reuse the latest
	// question with identical text instead: one seq for the whole wait, so
	// the answer always correlates regardless of when it lands.
	seq, reused, err := s.reuseOrAppendQuestion(ctx, req.GetSessionId(), req.GetQuestionsJson())
	if err != nil {
		return nil, fmt.Errorf("AskUserQuestion: %w", err)
	}
	if !reused {
		// The other way a session blocks on a human. Same notification as a
		// permission request, since from a human's side they are the same
		// event: nothing moves until someone answers. A re-invoke of an
		// already-announced question must not re-notify.
		s.announceBlocked(ctx, req.GetSessionId(), "asked a question")
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

// reuseOrAppendQuestion returns the seq of the latest still-*unanswered*
// "question" entry with identical text (the same AskUserQuestion the agent is
// re-invoking to keep waiting), or appends a fresh one. reused is true when an
// existing entry was found, so the caller can skip re-announcing.
//
// Only unanswered questions are reused: an agent that legitimately re-asks an
// identical question ("Proceed?" a second time) must get a fresh card and a
// fresh answer, not the stale reply to the first. An already-answered question
// is done — its seq is not a live wait to rejoin. The gap this can't cover —
// the human answering strictly between one call's timeout return and the
// re-invoke's read — is sub-millisecond (the agent re-invokes immediately) and
// a human can't hit it; a call still blocking when the answer lands returns it
// directly.
//
// ponytail: linear scan of the transcript per call — fine at these sizes;
// index unanswered questions by text if it ever matters.
func (s *Server) reuseOrAppendQuestion(ctx context.Context, sessionID, questionsJSON string) (seq int64, reused bool, err error) {
	entries, _, err := s.transcr.ReadSince(ctx, sessionID, 0, 100000)
	if err != nil {
		return 0, false, err
	}
	answered := make(map[int64]bool, len(entries))
	for _, e := range entries {
		if e.Type == "answer" && e.ReplyTo != nil {
			answered[*e.ReplyTo] = true
		}
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Type == "question" && e.Text == questionsJSON && !answered[e.Seq] {
			return e.Seq, true, nil
		}
	}

	// Byte equality above is a best case, not a guarantee: the model
	// regenerates the questions JSON on every call, so a re-ask that differs by
	// a word, an option order or a space misses the reuse and lands here.
	//
	// Measured live 2026-08-19 on session b7753602: two calls 72s apart, three
	// questions each, both announced blocked — i.e. both appended. The reuse
	// added for exactly this never matched once.
	//
	// Accumulating is the part that kills the feature. pending_decisions counts
	// EVERY unanswered question, so a session gathering one row per retry can
	// never be unblocked by answering: the human answers the card they see, the
	// count stays above zero, and a fresh card replaces it. Reported as "the
	// question is dead — I can't even respond."
	//
	// So supersede rather than accumulate. An agent can only ever be blocked on
	// one AskUserQuestion at a time — its own tool call blocks synchronously,
	// which is docs/adr/0018's founding assumption and still true — so an
	// older unanswered question is by definition a dead retry, not a second
	// thing being asked.
	//
	// Closed BEFORE the new row is appended, because the dashboard's
	// findPendingQuestion looks for a question with no LATER answer of any
	// kind; closing afterwards would mark the new question answered too.
	//
	// Authored by "agent", which keeps it out of both delivery paths: core's
	// poll matches only From == "human", and the worker's stream handler skips
	// any entry that is not from a human or a peer session. It resolves the
	// row for counting and for the console, and reaches no agent as an answer.
	for _, e := range entries {
		if e.Type != "question" || answered[e.Seq] {
			continue
		}
		if _, serr := s.transcr.AppendReply(ctx, sessionID, "agent",
			"superseded — the agent re-asked before this was answered",
			"answer", uuid.NewString(), e.Seq); serr != nil {
			// Not fatal: a failed supersede leaves an extra pending row, which
			// is the old behaviour, not a new failure.
			slog.Warn("coreserver: could not supersede stale question", "sessionId", sessionID, "seq", e.Seq, "error", serr)
			continue
		}
		slog.Info("coreserver: superseded stale question", "sessionId", sessionID, "seq", e.Seq)
	}

	seq, err = s.transcr.Append(ctx, sessionID, "agent", questionsJSON, "question", uuid.NewString())
	if err != nil {
		return 0, false, err
	}
	return seq, false, nil
}

// Expose/Unexpose/RequestService forward to the provisioner under
// docs/adr/0020's hub-and-spoke rule: the session pod never commands the
// provisioner directly, and all three of these are pod-lifecycle operations
// needing cluster RBAC it does not hold.
//
// These are not the passthroughs docs/adr/0045 deleted. That ADR's objection
// was to core carrying traffic that was none of its business — a shell
// command's bytes. Here core is the only holder of the session row, so it is
// the only thing that can answer "which repo is this session on", and it is
// the component the lifecycle rule names.

func (s *Server) Expose(ctx context.Context, req *agentfleetv1.ExposeRequest) (*agentfleetv1.ExposeResponse, error) {
	url, err := s.provisioner.ExposeSession(ctx, req.GetSessionId(), req.GetPort())
	if err != nil {
		return nil, fmt.Errorf("Expose: %w", err)
	}
	return &agentfleetv1.ExposeResponse{Url: url}, nil
}

func (s *Server) Unexpose(ctx context.Context, req *agentfleetv1.UnexposeRequest) (*agentfleetv1.UnexposeResponse, error) {
	if err := s.provisioner.UnexposeSession(ctx, req.GetSessionId()); err != nil {
		return nil, fmt.Errorf("Unexpose: %w", err)
	}
	return &agentfleetv1.UnexposeResponse{}, nil
}

// RequestService resolves the repo from the session row rather than taking it
// on the wire. Shared instances are keyed by repo, so a pod that could name
// its own repo could reach another repo's database.
func (s *Server) RequestService(ctx context.Context, req *agentfleetv1.RequestServiceRequest) (*agentfleetv1.RequestServiceResponse, error) {
	t, err := s.sessions.Get(ctx, req.GetSessionId())
	if err != nil {
		return nil, fmt.Errorf("RequestService: get session: %w", err)
	}
	if t == nil {
		return nil, fmt.Errorf("RequestService: session %s not found", req.GetSessionId())
	}
	dsn, err := s.provisioner.ProvisionService(ctx, req.GetSessionId(), t.Repo, req.GetKind())
	if err != nil {
		return nil, fmt.Errorf("RequestService: %w", err)
	}
	return &agentfleetv1.RequestServiceResponse{Dsn: dsn}, nil
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
func (s *Server) GetSession(ctx context.Context, req *agentfleetv1.GetSessionRequest) (*agentfleetv1.GetSessionResponse, error) {
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

// UpdateSessionMeta persists a worker pod's own title/description. Like
// SetPermissionMode, no transcript append: the caller is the session whose
// labels these are, there is no other worker to notify. req.Title/Description
// are *string (proto optional) and passed through untouched — nil means "leave
// unchanged", so GetTitle() (which derefs to "") must NOT be used here.
func (s *Server) UpdateSessionMeta(ctx context.Context, req *agentfleetv1.UpdateSessionMetaRequest) (*agentfleetv1.UpdateSessionMetaResponse, error) {
	if req.GetSessionId() == "" {
		return nil, fmt.Errorf("session_id is required")
	}
	if err := s.sessions.UpdateMeta(ctx, req.GetSessionId(), req.Title, req.Description); err != nil {
		return nil, fmt.Errorf("UpdateSessionMeta: %w", err)
	}
	return &agentfleetv1.UpdateSessionMetaResponse{}, nil
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

// maxJournalLimit caps SearchJournal. With repo and query both optional, an
// unbounded read of the whole table is now expressible in one call; nothing
// capped it before because every reachable call had to name a repo.
const maxJournalLimit = 500

// parseJournalBound accepts RFC3339 or a bare YYYY-MM-DD (midnight UTC), the
// two shapes an agent actually types. "" means unbounded. The bool reports a
// date-only value: a bare date names a whole day, not the instant it starts,
// and only the caller knows whether that matters for the bound it is parsing.
func parseJournalBound(field, v string) (time.Time, bool, error) {
	if v == "" {
		return time.Time{}, false, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, false, nil
	}
	if t, err := time.Parse(time.DateOnly, v); err == nil {
		return t, true, nil
	}
	return time.Time{}, false, fmt.Errorf("%s: %q is not RFC3339 or YYYY-MM-DD", field, v)
}

func (s *Server) SearchJournal(ctx context.Context, req *agentfleetv1.SearchJournalRequest) (*agentfleetv1.SearchJournalResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 50
	}
	if limit > maxJournalLimit {
		limit = maxJournalLimit
	}
	since, _, err := parseJournalBound("since", req.GetSince())
	if err != nil {
		return nil, fmt.Errorf("SearchJournal: %w", err)
	}
	until, untilIsDateOnly, err := parseJournalBound("until", req.GetUntil())
	if err != nil {
		return nil, fmt.Errorf("SearchJournal: %w", err)
	}
	if untilIsDateOnly {
		// until is an exclusive bound, but a bare YYYY-MM-DD names a whole
		// day. "until=2026-08-18" has to mean "through the 18th", not "up to
		// the instant the 18th began" — otherwise the literal seven-day
		// window an agent writes, since=2026-08-11 until=2026-08-18, silently
		// drops everything from today, which is the day it most wanted. A
		// full RFC3339 until is left exactly as given: it named an instant.
		until = until.AddDate(0, 0, 1)
	}
	entries, err := s.journal.Search(ctx, journal.SearchOpts{
		Repo:  req.GetRepo(),
		Query: req.GetQuery(),
		Since: since,
		Until: until,
		Limit: limit,
	})
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

func (s *Server) SaveAgentSessionId(ctx context.Context, req *agentfleetv1.SaveAgentSessionIdRequest) (*agentfleetv1.SaveAgentSessionIdResponse, error) {
	// The two ids are NOT the same thing, and passing session_id for both is
	// exactly the bug the task_id -> session_id rename introduced here: the
	// fleet's own row id was written into agent_session_id, so every resume
	// asked the SDK to continue a session that never existed. It fails
	// silently — the pod starts, the SDK just begins a fresh conversation,
	// and the only symptom is an agent with no memory of what was said before
	// the Stop.
	saved, err := s.sessions.SaveAgentSessionID(ctx, req.GetSessionId(), req.GetAgentSessionId(), req.GetModel(), req.GetLeaseId())
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
	// The summary may be empty now. The sidecar's tick became unconditional so
	// that wanted_paths below still reaches it on a session where nothing has
	// changed for a while; appending an empty summary in that case would write
	// a transcript row every 5 seconds forever, and latestToolCallSummary reads
	// the NEWEST tool_call entry — so it would also blank the CHANGES panel.
	if req.GetSummaryJson() != "" {
		if _, err := s.transcr.Append(ctx, req.GetSessionId(), "sidecar", req.GetSummaryJson(), "tool_call", uuid.NewString()); err != nil {
			return nil, fmt.Errorf("PushToolTelemetry: %w", err)
		}
	}
	if s.fileDiffs == nil {
		return &agentfleetv1.PushToolTelemetryResponse{}, nil
	}
	// Diffs are NOT appended. They go to memory, deliberately — see the field
	// comment and internal/filediff's package doc.
	if raw := req.GetFileDiffsJson(); raw != "" {
		var diffs map[string]string
		if err := json.Unmarshal([]byte(raw), &diffs); err != nil {
			// The telemetry summary above already landed; a malformed diff
			// payload must not fail the call and cost the session its
			// change stats too.
			slog.Warn("PushToolTelemetry: bad file_diffs_json", "session", req.GetSessionId(), "error", err)
		} else {
			s.fileDiffs.Put(req.GetSessionId(), diffs)
		}
	}
	return &agentfleetv1.PushToolTelemetryResponse{WantedPaths: s.fileDiffs.Wanted(req.GetSessionId())}, nil
}

// SetFileDiffStore wires the shared parking lot in after construction, so
// coreserver and dashboard.Server hold the same one. Same pattern as
// SetWarmFunc above and for the same reason.
func (s *Server) SetFileDiffStore(store *filediff.Store) { s.fileDiffs = store }

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
// (created/scheduled/running/crashed/terminated).
//
// Since docs/adr/0048 this stream is not merely telemetry: pod_phase IS the
// session lifecycle, and CountLivePods reads exactly the phases this writes.
// A dropped event therefore leaks a concurrency slot — which is why
// sessions.Loop re-reconciles every phase against the provisioner's Job list
// on a 60s ticker rather than trusting the stream to be lossless.
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
		if err := s.applyPodEvent(ctx, event); err != nil {
			return err
		}
	}
}

// applyPodEvent is one event's effect, split out from the stream loop so it
// can be tested directly — the alternative is standing up a bufconn stream to
// assert a two-line state transition.
func (s *Server) applyPodEvent(ctx context.Context, event *agentfleetv1.PodEvent) error {
	// No journal write here. Every pod phase transition used to append a
	// pod.POD_PHASE_* row, ~80% of knowledge_journal, and nothing ever read
	// one: the live value is sessions.pod_phase, written four lines down, and
	// the history is in Loki. knowledge_journal is what a future session needs
	// to know about a repo, not pod telemetry — a reader asking "what did
	// sessions learn last week" had to page past four rows of noise for every
	// row of signal. Do not add it back; put pod observability in Loki or
	// Prometheus.

	// Only worker pods have a session row to attach state to; e2e pods do not.
	//
	// MarkCrashed used to run here as a fast-path accelerant on top of the
	// heartbeat-reclaim fallback: it backdated heartbeat_at so ClaimNextTask
	// would reclaim the task without waiting out the full 10-minute staleness
	// window. Both the heartbeat and the reclaim are gone (docs/adr/0048), and
	// with them the reason for a fast path. This SetPodPhase write IS the
	// crash report now — CRASHED frees the session's slot, surfaces as
	// `failing` in the topology, and is what a human acts on.
	//
	// The one thing that had to be added for this to hold is on the
	// provisioner side: gcTerminalWorkerJobs now reports TERMINATED for a
	// Succeeded Job too, not just CRASHED for a Failed one. Without that, a
	// cleanly finished session would keep its slot forever.
	if event.GetKind() == agentfleetv1.SessionKind_SESSION_KIND_WORKER {
		if err := s.sessions.SetPodPhase(ctx, event.GetSessionId(), event.GetPhase().String(), event.GetMessage()); err != nil {
			slog.Error("ReportPodEvents: set pod phase failed", "sessionId", event.GetSessionId(), "error", err)
		}
	}
	return nil
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
