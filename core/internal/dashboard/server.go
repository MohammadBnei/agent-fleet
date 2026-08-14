// Package dashboard implements the web dashboard's ConnectRPC API
// (agentfleet.v1.DashboardService, see docs/adr/0015) on top of the exact
// same sessions.Store / transcript.Store / provisionerclient.Client that core's
// Discord commands already use — no new business logic, just a second
// caller of the same store methods.
package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/e2edial"
	"github.com/MohammadBnei/agent-fleet/core/internal/filestore"
	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/lokiclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/promclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/promptsnippets"
	"github.com/MohammadBnei/agent-fleet/core/internal/proposals"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/scheduledaudits"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

type Server struct {
	sessions  *sessions.Store
	proposals *proposals.Store
	transcr   transcript.Store
	journal   *journal.Store
	repos     *repos.Store
	snippets  *promptsnippets.Store
	e2e       *provisionerclient.Client
	files     filestore.Store
	hub       *Hub
	// maxLive is the fleet's blast-radius bound: how many pods, each running
	// an agent, may exist at once. Enforced inside sessions.ReserveSlot under
	// an advisory lock rather than checked here, because a read-then-act
	// check is exactly what let CI observe 4 tasks claimed with a cap of 2.
	maxLive int
	loki    lokiclient.Querier
	prom    promclient.Querier
	audits  *scheduledaudits.Store
}

func NewServer(sessionStore *sessions.Store, proposalStore *proposals.Store, transcr transcript.Store, journalStore *journal.Store, repoStore *repos.Store, snippetStore *promptsnippets.Store, e2e *provisionerclient.Client, files filestore.Store, hub *Hub, maxLive int, loki lokiclient.Querier, prom promclient.Querier, auditStore *scheduledaudits.Store) *Server {
	return &Server{sessions: sessionStore, proposals: proposalStore, transcr: transcr, journal: journalStore, repos: repoStore, snippets: snippetStore, e2e: e2e, files: files, hub: hub, maxLive: maxLive, loki: loki, prom: prom, audits: auditStore}
}

// toolKeysFor resolves the only ingredient that survived docs/adr/0034's
// recipe system: cluster-access, which is a privilege grant rather than a
// toolchain (docs/adr/0037, docs/adr/0048). Everything else the recipe used
// to carry is now either the agent's own `Bash` or repos.image.
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

var _ agentfleetv1connect.DashboardServiceHandler = (*Server)(nil)

const defaultListLimit = 50

func (s *Server) ListSessions(ctx context.Context, req *connect.Request[agentfleetv1.ListSessionsRequest]) (*connect.Response[agentfleetv1.ListSessionsResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = defaultListLimit
	}
	list, err := s.sessions.List(ctx, limit)
	if err != nil {
		slog.Error("dashboard ListTasks", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*agentfleetv1.Session, len(list))
	for i, t := range list {
		out[i] = SessionToProto(t)
	}
	return connect.NewResponse(&agentfleetv1.ListSessionsResponse{Sessions: out}), nil
}

func (s *Server) GetSession(ctx context.Context, req *connect.Request[agentfleetv1.GetSessionRequest]) (*connect.Response[agentfleetv1.GetSessionResponse], error) {
	t, err := s.sessions.Get(ctx, req.Msg.GetId())
	if err != nil {
		slog.Error("dashboard GetTask", "sessionId", req.Msg.GetId(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if t == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("task not found"))
	}
	return connect.NewResponse(&agentfleetv1.GetSessionResponse{Session: SessionToProto(*t)}), nil
}

// CreateTask lets the dashboard create a task the same way a Discord /task
// command does, minus the Discord thread — it calls the exact same
// sessions.Store.CreateTask core/internal/discord/handlers.go's startTask
// calls, just with nil channel/thread (docs/adr/0015). PostToThread
// (core/internal/discord/session.go) already no-ops on a nil ThreadID, so
// no other code needs to special-case a dashboard-origin task.
// CreateSession makes a row and nothing else — no pod, no directory, no
// worktree. The first PostMessage is what provisions (docs/adr/0048).
//
// That split is the human gate: nothing machine-initiated produces a message,
// so nothing machine-initiated produces a pod. It also makes an empty session
// a valid resting state rather than a broken one, which matters because the
// Agent SDK's streaming-input generator is never entered until an input
// arrives — a pod booted with nothing to do would never emit a session id,
// would be unresumable, and would be swept as stalled with nothing logged.
//
// `description` is optional now, and is a label rather than an instruction:
// the session's actual instruction is its first transcript entry. Snippets
// are no longer resolved here into a hidden `guidance` column — the dashboard
// prefills them into the message composer, so their text reaches the model as
// part of a message a human sent and can edit.
func (s *Server) CreateSession(ctx context.Context, req *connect.Request[agentfleetv1.CreateSessionRequest]) (*connect.Response[agentfleetv1.CreateSessionResponse], error) {
	repo := req.Msg.GetRepo()
	repoCfg, err := s.repos.Get(ctx, repo)
	if err != nil {
		slog.Error("dashboard CreateSession: repo lookup", "repo", repo, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if repoCfg == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown repo %q", repo))
	}

	model := req.Msg.GetModel()
	if model == "" {
		model = os.Getenv("CLAUDE_MODEL")
		if model == "" {
			model = "claude-opus-4-8"
		}
	} else if !isValidModel(model) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid model %q", model))
	}

	id, err := s.sessions.Create(ctx, repo, req.Msg.GetTitle(), req.Msg.GetDescription(), model)
	if err != nil {
		slog.Error("dashboard CreateSession", "repo", repo, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		slog.Error("dashboard CreateSession", "sessionId", id, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	slog.Info("dashboard CreateSession", "sessionId", id, "repo", repo)
	return connect.NewResponse(&agentfleetv1.CreateSessionResponse{Session: SessionToProto(*sess)}), nil
}

// ArchiveSession marks a session finished by a human — the only terminal
// state in the fleet, because it is the only one a machine can compute.
//
// Tears down any live pod and dismisses the proposal that spawned it, if any:
// that is what re-arms the dedup key so a still-firing alert or the next audit
// tick can propose the same work again.
func (s *Server) ArchiveSession(ctx context.Context, req *connect.Request[agentfleetv1.ArchiveSessionRequest]) (*connect.Response[agentfleetv1.ArchiveSessionResponse], error) {
	id := req.Msg.GetSessionId()
	if err := s.sessions.Archive(ctx, id); err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Best-effort, logged not propagated: the archive itself has committed,
	// and a teardown hiccup should not fail it. The reconcile loop notices a
	// pod whose session is archived on its next pass anyway.
	if _, err := s.e2e.TearDownSession(ctx, id, agentfleetv1.SessionKind_SESSION_KIND_WORKER); err != nil {
		slog.Warn("dashboard ArchiveSession: worker teardown failed", "sessionId", id, "error", err)
	}
	if _, err := s.e2e.TearDownSession(ctx, id, agentfleetv1.SessionKind_SESSION_KIND_E2E); err != nil {
		slog.Warn("dashboard ArchiveSession: e2e teardown failed", "sessionId", id, "error", err)
	}
	if err := s.proposals.DismissForSession(ctx, id); err != nil {
		slog.Warn("dashboard ArchiveSession: proposal dismiss failed", "sessionId", id, "error", err)
	}
	return connect.NewResponse(&agentfleetv1.ArchiveSessionResponse{}), nil
}

func (s *Server) ListProposals(ctx context.Context, _ *connect.Request[agentfleetv1.ListProposalsRequest]) (*connect.Response[agentfleetv1.ListProposalsResponse], error) {
	list, err := s.proposals.ListOpen(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*agentfleetv1.Proposal, 0, len(list))
	for _, p := range list {
		out = append(out, proposalToProto(p))
	}
	return connect.NewResponse(&agentfleetv1.ListProposalsResponse{Proposals: out}), nil
}

// OpenFromProposal turns a machine-initiated suggestion into a real session.
//
// THE human gate — the one call that can hand a cluster-access agent a
// session, reachable only from the dashboard behind Traefik basic-auth. It
// replaces ApproveTask, which flipped a status value to release a task into a
// dispatch queue that no longer exists.
//
// It creates the session but does NOT boot a pod: the proposal body is posted
// as the first message, and that is what provisions. So a human can open a
// proposal, read it, and still walk away without an agent having run.
//
// proposals.Open guards inside its UPDATE, so two clicks — or two humans, or
// one stale browser tab — cannot open two sessions from one proposal.
func (s *Server) OpenFromProposal(ctx context.Context, req *connect.Request[agentfleetv1.OpenFromProposalRequest]) (*connect.Response[agentfleetv1.OpenFromProposalResponse], error) {
	p, err := s.proposals.Get(ctx, req.Msg.GetProposalId())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if p == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such proposal"))
	}

	id, err := s.sessions.Create(ctx, p.Repo, p.Title, p.Body, "")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := s.proposals.Open(ctx, p.ID, id); err != nil {
		if errors.Is(err, proposals.ErrNotOpen) {
			// Someone else won the race. Delete the session we just made
			// rather than leaving an orphan behind — it has no pod and no
			// transcript, so this is a clean rollback.
			if delErr := s.sessions.Delete(ctx, id); delErr != nil {
				slog.Warn("dashboard OpenFromProposal: rollback failed", "sessionId", id, "error", delErr)
			}
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("proposal is already opened or dismissed"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	slog.Info("dashboard OpenFromProposal", "proposalId", p.ID, "sessionId", id, "repo", p.Repo)
	return connect.NewResponse(&agentfleetv1.OpenFromProposalResponse{Session: SessionToProto(*sess)}), nil
}

func (s *Server) DismissProposal(ctx context.Context, req *connect.Request[agentfleetv1.DismissProposalRequest]) (*connect.Response[agentfleetv1.DismissProposalResponse], error) {
	if err := s.proposals.Dismiss(ctx, req.Msg.GetProposalId()); err != nil {
		if errors.Is(err, proposals.ErrNotOpen) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.DismissProposalResponse{}), nil
}

func proposalToProto(p proposals.Proposal) *agentfleetv1.Proposal {
	out := &agentfleetv1.Proposal{
		Id:        p.ID,
		Repo:      p.Repo,
		Source:    p.Source,
		Title:     p.Title,
		Body:      p.Body,
		CreatedAt: p.CreatedAt.Format(time.RFC3339),
	}
	out.SessionId = p.SessionID
	return out
}

// resolveGuidanceAndMode used to live here: it joined the operator's selected
// prompt snippets into the hidden `guidance` blob stored alongside the task's
// description, and picked up a suggested permission mode along the way.
//
// Both halves are gone in docs/adr/0048. There is no `guidance` column,
// because there is no fleet-composed prompt to hide text in — whatever the
// human types is the turn, verbatim. Snippets survive as what they always
// really were: text the dashboard prefills into the composer, visible and
// editable before it is sent. ListPromptSnippets still serves them.

// validModels is the allowlist of Claude models that can be selected per-task.
// This prevents injection of arbitrary model names and ensures only supported
// models are used.
var validModels = map[string]bool{
	"claude-opus-4-8":            true,
	"claude-sonnet-4-5-20250929": true,
	"claude-opus-4-5-20251101":   true,
	"claude-haiku-4-5-20250929":  true,
}

func isValidModel(model string) bool {
	return validModels[model]
}

func (s *Server) GetTranscript(ctx context.Context, req *connect.Request[agentfleetv1.ReadTranscriptSinceRequest]) (*connect.Response[agentfleetv1.ReadTranscriptSinceResponse], error) {
	sessionID := req.Msg.GetSessionId()
	entries, next, err := s.transcr.ReadSince(ctx, sessionID, req.Msg.GetSinceSeq(), 1000)
	if err != nil {
		slog.Error("dashboard GetTranscript", "sessionId", sessionID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*agentfleetv1.TranscriptEntry, len(entries))
	for i, e := range entries {
		out[i] = entryToProto(sessionID, e)
	}
	return connect.NewResponse(&agentfleetv1.ReadTranscriptSinceResponse{Entries: out, NextSeq: next}), nil
}

func (s *Server) StreamTranscript(ctx context.Context, req *connect.Request[agentfleetv1.StreamTranscriptRequest], stream *connect.ServerStream[agentfleetv1.TranscriptEntry]) error {
	ch, cancel := s.hub.Subscribe(req.Msg.GetSessionId(), req.Msg.GetSinceSeq())
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
	live, err := s.e2e.GetSessionStatus(ctx, req.Msg.GetSessionId())
	if err != nil {
		slog.Error("dashboard GetE2EStatus", "sessionId", req.Msg.GetSessionId(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &agentfleetv1.GetE2EStatusResponse{
		Status:        live.GetStatus(),
		PreviewUrl:    live.GetPreviewUrl(),
		CodeServerUrl: live.GetCodeServerUrl(),
		StartCmd:      live.GetStartCmd(),
		PodPhase:      live.GetPodPhase(),
		AppReady:      live.GetAppReady(),
		Restarts:      live.GetRestarts(),
		StartedAt:     live.GetStartedAt(),
	}
	// The declared recipe is core's half — the provisioner holds no DB
	// credentials (docs/adr/0020 point 1) and can't read repo_profiles.
	// The recipe fields (profile_name, tools, services, start_cmd_overridden)
	// used to be resolved here from repo_profiles. That whole system is gone
	// in docs/adr/0048 — the agent starts its own server, so there is no
	// configured start command for a running one to differ from, and no
	// profile name to report. The remaining fields are live pod truth passed
	// straight through from the provisioner, which is all this card ever
	// needed to answer "is the preview up".
	//
	// `tools` still carries something real: cluster-access, the one
	// ingredient that survived, because it is a privilege grant rather than a
	// toolchain (docs/adr/0037).
	if t, err := s.sessions.Get(ctx, req.Msg.GetSessionId()); err != nil {
		slog.Warn("dashboard GetE2EStatus: get session", "sessionId", req.Msg.GetSessionId(), "error", err)
	} else if t != nil {
		if keys, err := s.toolKeysFor(ctx, t.Repo); err != nil {
			slog.Warn("dashboard GetE2EStatus: tool keys", "repo", t.Repo, "error", err)
		} else {
			resp.Tools = keys
		}
	}
	return connect.NewResponse(resp), nil
}

// Kill and KillE2E below call the exact same store methods
// core/internal/discord/handlers.go's /kill and /e2e-kill commands already
// call — no new business logic, just a second caller (see docs/adr/0014,
// docs/adr/0015). Approve is gone as of the sessions redesign (supersedes
// docs/adr/0021/0025's phase-boundary framing) — there's no plan->default
// flip left to fix a button to; SetPermissionMode below covers mode
// switching, RespondToPermission covers per-tool-call decisions.

// Kill posts a cooperative abort message (the worker pod notices it via
// streamHumanMessages and exits cleanly if it's alive and responsive) and
// also durably marks when the stop was requested — dispatch.Loop's
// grace-period sweep force-tears-down the pod via TearDownSession if it
// hasn't gone terminal within the grace window, covering the hung/crashed/
// unreachable-pod case a bare abort message can never reach. Ends the
// whole session/pod — Interrupt below is the softer, session-preserving
// sibling.
func (s *Server) StopSession(ctx context.Context, req *connect.Request[agentfleetv1.StopSessionRequest]) (*connect.Response[agentfleetv1.StopSessionResponse], error) {
	sessionID := req.Msg.GetSessionId()
	reason := "killed by human"
	if req.Msg.Reason != nil && *req.Msg.Reason != "" {
		reason = req.Msg.GetReason()
	}
	if _, err := s.transcr.Append(ctx, sessionID, "human", reason, "abort", uuid.NewString()); err != nil {
		slog.Error("dashboard Kill", "sessionId", sessionID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := s.sessions.MarkStopRequested(ctx, sessionID); err != nil {
		slog.Error("dashboard Kill: mark stop requested", "sessionId", sessionID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.StopSessionResponse{}), nil
}

// Interrupt posts an "interrupt" transcript entry — the worker calls the
// SDK's q.interrupt() on just the current turn and keeps the session/pod
// alive for the next message, unlike Kill. Deliberately doesn't touch
// tasks.stop_requested_at/pod lifecycle: there's nothing for
// dispatch.Loop's grace-period sweep to force-teardown here.
func (s *Server) Interrupt(ctx context.Context, req *connect.Request[agentfleetv1.InterruptRequest]) (*connect.Response[agentfleetv1.InterruptResponse], error) {
	sessionID := req.Msg.GetSessionId()
	if _, err := s.transcr.Append(ctx, sessionID, "human", "interrupted by human", "interrupt", uuid.NewString()); err != nil {
		slog.Error("dashboard Interrupt", "sessionId", sessionID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.InterruptResponse{}), nil
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
	"plan":              true,
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
	sessionID := req.Msg.GetSessionId()
	mode := req.Msg.GetMode()
	if !validPermissionModes[mode] {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid permission mode %q", mode))
	}
	if _, err := s.transcr.Append(ctx, sessionID, "human", mode, "permission_mode", uuid.NewString()); err != nil {
		slog.Error("dashboard SetPermissionMode", "sessionId", sessionID, "mode", mode, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := s.sessions.SetPermissionMode(ctx, sessionID, mode); err != nil {
		slog.Error("dashboard SetPermissionMode: persist", "sessionId", sessionID, "mode", mode, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.SetPermissionModeResponse{}), nil
}

func (s *Server) KillE2E(ctx context.Context, req *connect.Request[agentfleetv1.KillE2ERequest]) (*connect.Response[agentfleetv1.KillE2EResponse], error) {
	sessionID := req.Msg.GetSessionId()
	var repo string
	if req.Msg.GetAlsoTeardownServices() {
		t, err := s.sessions.Get(ctx, sessionID)
		if err != nil {
			slog.Error("dashboard KillE2E: get task for repo", "sessionId", sessionID, "error", err)
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if t != nil {
			repo = t.Repo
		}
	}
	killed, servicesTornDown, err := s.e2e.KillSession(ctx, sessionID, uuid.NewString(), repo, req.Msg.GetAlsoTeardownServices())
	if err != nil {
		slog.Error("dashboard KillE2E", "sessionId", sessionID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.KillE2EResponse{Killed: killed, ServicesTornDown: servicesTornDown}), nil
}

// StartE2E creates the sandbox from the dashboard. Until now a human could
// Kill an e2e env but never start one — the only way to get a sandbox was to
// ask the agent to call request_e2e_env, which is a strange dependency when
// the sandbox is precisely what a human wants in order to look at a broken
// preview themselves (docs/adr/0044).
//
// Resolution goes through e2erecipe, the same path CoreService.RequestE2EEnv
// uses. Deliberately not a reimplementation: the hardcoded-"e2e" bug that
// package exists to prevent had already been copied into GetE2EStatus below.
func (s *Server) StartE2E(ctx context.Context, req *connect.Request[agentfleetv1.StartE2ERequest]) (*connect.Response[agentfleetv1.StartE2EResponse], error) {
	sessionID := req.Msg.GetSessionId()
	t, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		slog.Error("dashboard StartE2E: get task", "sessionId", sessionID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if t == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("task %s not found", sessionID))
	}
	// No start command is resolved any more: the recipe system is deleted in
	// docs/adr/0048, and the agent starts its own server. A sandbox with an
	// empty start_cmd is a working sandbox, not a failed pod — that part of
	// docs/adr/0044 survives its own supersession.
	toolKeys, err := s.toolKeysFor(ctx, t.Repo)
	if err != nil {
		slog.Error("dashboard StartE2E: tool keys", "sessionId", sessionID, "repo", t.Repo, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	created, err := s.e2e.CreateE2eSession(ctx, sessionID, t.Repo, "", toolKeys)
	if err != nil {
		slog.Error("dashboard StartE2E", "sessionId", sessionID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	slog.Info("dashboard StartE2E", "sessionId", sessionID, "repo", t.Repo)
	// The roster is deliberately not surfaced to the browser: a dashboard
	// user has no route to a ClusterIP, and the endpoints are only useful to
	// in-cluster callers (docs/adr/0045).
	return connect.NewResponse(&agentfleetv1.StartE2EResponse{
		Status:     created.GetStatus(),
		PreviewUrl: created.GetPreviewUrl(),
	}), nil
}

// RestartE2EApp re-runs the app inside the live pod via the e2e-restart-app
// helper on the image's PATH. It restarts the APP, not the pod — the warm
// dependency cache and the worktree survive, so it costs seconds where
// recreating the sandbox costs a 10+ minute cold install (docs/adr/0044).
//
// Routed through run_command rather than a new ProvisionerService RPC: the
// passthrough already exists, and the logic that actually matters (signalling
// the app's whole process group, so a dev server's children don't keep $PORT
// bound) belongs in the image next to the thing it restarts, where the agent
// can invoke it too.
func (s *Server) RestartE2EApp(ctx context.Context, req *connect.Request[agentfleetv1.RestartE2EAppRequest]) (*connect.Response[agentfleetv1.RestartE2EAppResponse], error) {
	out, exitCode, err := s.runInE2ePod(ctx, req.Msg.GetSessionId(), "e2e-restart-app")
	if err != nil {
		slog.Error("dashboard RestartE2EApp", "sessionId", req.Msg.GetSessionId(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	slog.Info("dashboard RestartE2EApp", "sessionId", req.Msg.GetSessionId(), "exitCode", exitCode)
	return connect.NewResponse(&agentfleetv1.RestartE2EAppResponse{Output: out, ExitCode: exitCode}), nil
}

// GetE2EAppLog reads the app's own stdout out of the live pod. Not Loki: this
// works regardless of retention, and it carries the explicit "app command
// exited with status N" marker the entrypoint writes — the single question
// the e2e card could never answer.
func (s *Server) GetE2EAppLog(ctx context.Context, req *connect.Request[agentfleetv1.GetE2EAppLogRequest]) (*connect.Response[agentfleetv1.GetE2EAppLogResponse], error) {
	lines := req.Msg.GetLines()
	if lines <= 0 {
		lines = 200
	}
	if lines > 2000 {
		lines = 2000
	}
	out, _, err := s.runInE2ePod(ctx, req.Msg.GetSessionId(), fmt.Sprintf("tail -n %d /tmp/e2e-app.log", lines))
	if err != nil {
		// A pod that isn't up yet is the common case, not a page-level error —
		// same reasoning useTaskDetail's own poll already applies.
		slog.Info("dashboard GetE2EAppLog: no readable pod", "sessionId", req.Msg.GetSessionId(), "error", err)
		return connect.NewResponse(&agentfleetv1.GetE2EAppLogResponse{}), nil
	}
	return connect.NewResponse(&agentfleetv1.GetE2EAppLogResponse{Log: out}), nil
}

// runViaSandbox dials the task's sandbox directly (docs/adr/0045).
//
// The roster arrives on GetE2eSessionStatus, which this already had to call —
// it needs the pod's live state anyway to know whether there is anything to
// run in. So direct dial costs core no extra round trip here, unlike the
// sidecar, which had to be handed its roster at pod creation.
func (s *Server) runViaSandbox(ctx context.Context, sessionID, command string) (string, error) {
	status, err := s.e2e.GetSessionStatus(ctx, sessionID)
	if err != nil {
		return "", err
	}
	endpoints := make([]e2edial.Endpoint, 0, len(status.GetEndpoints()))
	for _, e := range status.GetEndpoints() {
		endpoints = append(endpoints, e2edial.Endpoint{Name: e.GetName(), Address: e.GetAddress(), Path: e.GetPath()})
	}
	ep, ok := e2edial.Find(endpoints, e2edial.EndpointExec)
	if !ok {
		// No relay to fall back to any more (docs/adr/0045). The roster comes
		// from the same GetE2eSessionStatus call above, so an absent exec
		// endpoint means either there is no sandbox or the provisioner is too
		// old to describe one — both worth saying plainly rather than routing
		// around.
		return "", fmt.Errorf("no exec endpoint for task %s: the sandbox is not running, or the provisioner predates the endpoint roster", sessionID)
	}
	return e2edial.RunCommand(ctx, ep, command)
}

// runInE2ePod is the shared half of the two handlers above: run one command
// in the task's e2e pod and unwrap execmcp's {stdout,stderr,exitCode}
// envelope.
func (s *Server) runInE2ePod(ctx context.Context, sessionID, command string) (output string, exitCode int32, err error) {
	resultJSON, err := s.runViaSandbox(ctx, sessionID, command)
	if err != nil {
		return "", 0, err
	}
	// execmcp answers as an MCP CallToolResult whose single text block is the
	// JSON payload — unwrapped here so the dashboard never sees MCP framing.
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		return "", 0, fmt.Errorf("unmarshal tool result: %w", err)
	}
	if len(result.Content) == 0 {
		return "", 0, nil
	}
	var payload struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int32  `json:"exitCode"`
	}
	if err := json.Unmarshal([]byte(result.Content[0].Text), &payload); err != nil {
		// Not the envelope we expected — hand back the raw text rather than
		// an error, since it's still the most useful thing we have.
		return result.Content[0].Text, 0, nil
	}
	return payload.Stdout + payload.Stderr, payload.ExitCode, nil
}

// AnswerQuestion appends the human's answer to a pending QUESTION-type
// transcript entry (posted by the agent's AskUserQuestion MCP tool call,
// see docs/adr/0018) via AppendReply — req.Msg.Seq is the question entry's
// own seq, now actually used server-side for correlation
// (reliability-findings.md #0: "any pending question + any reply" let an
// unrelated message satisfy a blocked AskUserQuestion call).
func (s *Server) AnswerQuestion(ctx context.Context, req *connect.Request[agentfleetv1.AnswerQuestionRequest]) (*connect.Response[agentfleetv1.AppendResponse], error) {
	sessionID := req.Msg.GetSessionId()
	seq, err := s.transcr.AppendReply(ctx, sessionID, "human", req.Msg.GetAnswersJson(), "answer", uuid.NewString(), req.Msg.GetSeq())
	if err != nil {
		slog.Error("dashboard AnswerQuestion", "sessionId", sessionID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.AppendResponse{Seq: seq}), nil
}

// RespondToPermission answers a pending PERMISSION_REQUEST entry — the
// generalized canUseTool prompt-and-wait gate's counterpart to
// AnswerQuestion, same AppendReply-by-seq shape, kept as a sibling RPC
// rather than overloaded onto AnswerQuestion since the payload differs
// (allow/deny/updatedInput JSON vs. free-form answers JSON).
func (s *Server) RespondToPermission(ctx context.Context, req *connect.Request[agentfleetv1.RespondToPermissionRequest]) (*connect.Response[agentfleetv1.AppendResponse], error) {
	sessionID := req.Msg.GetSessionId()
	seq, err := s.transcr.AppendReply(ctx, sessionID, "human", req.Msg.GetDecisionJson(), "permission_response", uuid.NewString(), req.Msg.GetSeq())
	if err != nil {
		slog.Error("dashboard RespondToPermission", "sessionId", sessionID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.AppendResponse{Seq: seq}), nil
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
func (s *Server) PostMessage(ctx context.Context, req *connect.Request[agentfleetv1.PostMessageRequest]) (*connect.Response[agentfleetv1.AppendResponse], error) {
	sessionID := req.Msg.GetSessionId()
	text := req.Msg.GetText()
	if text == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("text is required"))
	}
	// Warm BEFORE appending, never after. resumeFromSeq is computed from
	// LatestSeq at provisioning time, so a message appended first would land
	// below the new pod's cursor and never be delivered — the pod would boot
	// and sit there with nothing to do. This ordering is the whole mechanism
	// by which a first message boots a session (docs/adr/0048).
	if _, err := s.WarmIfIdle(ctx, sessionID); err != nil {
		return nil, err
	}
	seq, err := s.transcr.Append(ctx, sessionID, "human", text, "discussion", uuid.NewString())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.AppendResponse{Seq: seq}), nil
}

// MarkSeen records that a human opened this session's detail view, which
// is what distinguishes `idle` from `done` (docs/adr/0040) — `done` being
// a session that finished while nobody was looking, the state worth
// surfacing in a fleet you are not watching continuously.
//
// Deliberately its own RPC rather than a side effect of GetTask: the task
// list polls GetTranscript/ListTasks every 5s, and marking seen from a
// poll would clear the flag for every session at once and make `done`
// unreachable. Best-effort — failing to record a look is not worth
// failing the caller over, and the next open will record it anyway.
func (s *Server) MarkSeen(ctx context.Context, req *connect.Request[agentfleetv1.MarkSeenRequest]) (*connect.Response[agentfleetv1.MarkSeenResponse], error) {
	if err := s.sessions.MarkSeen(ctx, req.Msg.GetSessionId()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.MarkSeenResponse{}), nil
}

// Warm boots a pod for an idle session on demand (see WarmSessionRequest's own
// proto comment) — the explicit counterpart to Discuss's auto-warm. Gives
// an explicit, specific rejection reason for each way a click can be a
// no-op — unlike Discuss, which shares warmIfIdle's silent-skip behavior
// for those same cases because it has a message to send regardless.
func (s *Server) WarmSession(ctx context.Context, req *connect.Request[agentfleetv1.WarmSessionRequest]) (*connect.Response[agentfleetv1.WarmSessionResponse], error) {
	sessionID := req.Msg.GetSessionId()
	t, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// An explicit click deserves an explicit answer, where PostMessage's
	// shared path treats the same condition as a silent no-op.
	if sessions.IsPodPhaseLive(t.PodPhase) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("session already has a live pod"))
	}
	podName, err := s.WarmIfIdle(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.WarmSessionResponse{PodName: podName}), nil
}

// ApproveTask and RetryTask used to live here. Both are deleted in
// docs/adr/0048.
//
// ApproveTask's job — the human gate on a machine-created proposal, the one
// write that can hand a cluster-access agent a pod — moves to
// OpenFromProposal below. It is the same guarantee expressed in the schema
// instead of in a status value: a proposal row has no pod path at all, so
// there is no dispatcher to be trusted not to SELECT it.
//
// RetryTask was the only way back from failed_permanently, a state the
// automatic reclaim could drive a task into with no exit. Nothing reclaims a
// session now, so there is no dead state to resurrect one from — retrying is
// just sending the session another message.

// WarmIfIdle provisions a pod for a session that has none, and is a silent
// no-op for one that already does. It is the single path to a worker pod in
// the entire fleet — PostMessage calls it unconditionally on every message,
// WarmSession calls it from an explicit click, and coreserver's PromptSession
// reuses it rather than duplicating the sequence (docs/adr/0041).
//
// There is no longer a dispatch loop competing for that job, which removes
// the double-dispatch hazard the old version guarded against by refusing to
// warm 'pending' and 'proposed' tasks. Both guards are gone with the statuses:
//
//   - 'pending' existed because ClaimNextTask owned a fresh task's first pod.
//     Nothing claims anything now; the first message provisions, full stop.
//   - 'proposed' was the human gate on machine-created work. That moved into
//     the schema: a proposal is a row in a different table with no pod path,
//     so there is nothing here to guard. See OpenFromProposal.
//
// Archived sessions are refused by ReserveSlot itself.
func (s *Server) WarmIfIdle(ctx context.Context, sessionID string) (podName string, err error) {
	t, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			return "", connect.NewError(connect.CodeNotFound, err)
		}
		return "", connect.NewError(connect.CodeInternal, err)
	}
	if sessions.IsPodPhaseLive(t.PodPhase) {
		return "", nil
	}
	// A swept session has had its working directory and SDK state reclaimed,
	// so there is nothing to resume into — booting a pod would hand the agent
	// an empty directory and a resume id pointing at a deleted transcript.
	if t.SweptAt != nil {
		return "", connect.NewError(connect.CodeFailedPrecondition,
			errors.New("session was swept by the retention GC — its working directory is gone, so it cannot be resumed"))
	}

	repoCfg, err := s.repos.Get(ctx, t.Repo)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	if repoCfg == nil {
		return "", connect.NewError(connect.CodeInternal, fmt.Errorf("unknown repo %q", t.Repo))
	}

	// The cap check and the lease mint happen together, under an advisory
	// lock, inside ReserveSlot. Checking here and acting after would be the
	// read-then-act race CI once observed as 4 tasks claimed with a cap of 2.
	leaseID, err := s.sessions.ReserveSlot(ctx, sessionID, s.maxLive)
	if err != nil {
		switch {
		case errors.Is(err, sessions.ErrAtCapacity):
			return "", connect.NewError(connect.CodeResourceExhausted,
				fmt.Errorf("fleet at capacity (%d live sessions) — stop one first", s.maxLive))
		case errors.Is(err, sessions.ErrNotFound):
			return "", connect.NewError(connect.CodeFailedPrecondition, errors.New("session is archived"))
		}
		return "", connect.NewError(connect.CodeInternal, err)
	}

	resumeAgentSessionID := ""
	if t.AgentSessionID != nil {
		resumeAgentSessionID = *t.AgentSessionID
	}
	// Read AFTER reserving and BEFORE the caller appends: this cursor is what
	// the new pod starts streaming from, so an entry written before the pod
	// exists would land below it and never be delivered.
	resumeFromSeq, err := s.transcr.LatestSeq(ctx, sessionID)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	toolKeys, err := s.toolKeysFor(ctx, t.Repo)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	podName, err = s.e2e.CreateWorkerPod(ctx, sessionID, t.Repo, repoCfg.URL, repoCfg.BaseBranch, t.Description, leaseID, resumeAgentSessionID, resumeFromSeq, toolKeys)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	return podName, nil
}

// DeleteTask force-tears-down any live session for sessionID (both kinds —
// mirrors the same two calls coreserver/server.go's SetTaskStatus makes on
// a terminal status, just invoked directly instead of waiting for the
// worker pod to reach that code path, so a wedged/crashed pod doesn't
// block removal like Stop's cooperative abort-signal does) and then
// soft-deletes the task row. Doesn't touch status — see
// sessions.Store.SoftDelete's own comment.
func (s *Server) DeleteSession(ctx context.Context, req *connect.Request[agentfleetv1.DeleteSessionRequest]) (*connect.Response[agentfleetv1.DeleteSessionResponse], error) {
	sessionID := req.Msg.GetSessionId()
	if _, err := s.e2e.TearDownSession(ctx, sessionID, agentfleetv1.SessionKind_SESSION_KIND_WORKER); err != nil {
		slog.Error("dashboard DeleteTask: worker teardown", "sessionId", sessionID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if _, err := s.e2e.TearDownSession(ctx, sessionID, agentfleetv1.SessionKind_SESSION_KIND_E2E); err != nil {
		slog.Error("dashboard DeleteTask: e2e teardown", "sessionId", sessionID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := s.sessions.Delete(ctx, sessionID); err != nil {
		slog.Error("dashboard DeleteTask: soft delete", "sessionId", sessionID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	slog.Info("dashboard DeleteTask", "sessionId", sessionID)
	return connect.NewResponse(&agentfleetv1.DeleteSessionResponse{}), nil
}

// ListWorktrees left-joins the provisioner's raw worktree list against
// `tasks` (reliability-findings.md #2) — core has no PVC access itself,
// so the worktree data comes from a passthrough call to the provisioner;
// task_status/task_error/pr_url are left unset (not an error) for a
// worktree whose task row no longer exists, exactly the orphaned case
// this view exists to surface. An inner join would hide it.
func (s *Server) ListWorktrees(ctx context.Context, _ *connect.Request[agentfleetv1.ListWorktreesRequest]) (*connect.Response[agentfleetv1.ListWorktreesViewResponse], error) {
	resp, err := s.e2e.ListWorktrees(ctx)
	if err != nil {
		slog.Error("dashboard ListWorktrees", "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	worktrees := resp.GetWorktrees()
	out := make([]*agentfleetv1.WorktreeView, len(worktrees))
	for i, w := range worktrees {
		view := &agentfleetv1.WorktreeView{
			SessionId:     w.GetSessionId(),
			Repo:          w.GetRepo(),
			Branch:        w.GetBranch(),
			UpstreamTrack: w.GetUpstreamTrack(),
			MtimeUnix:     w.GetMtimeUnix(),
			Path:          w.GetPath(),
			DirtyFiles:    w.GetDirtyFiles(),
			SizeBytes:     w.GetSizeBytes(),
		}
		// Left join, deliberately: a directory whose session row is gone is
		// exactly the orphan case this view exists to surface, so a missing
		// session leaves live_state empty rather than failing the whole list.
		if sess, err := s.sessions.Get(ctx, w.GetSessionId()); err == nil {
			view.LiveState = string(sessions.DeriveLiveState(sess, time.Now(), DefaultTurnStall))
			view.SessionError = sess.LastError
		} else if !errors.Is(err, sessions.ErrNotFound) {
			slog.Error("dashboard ListWorktrees: get session", "sessionId", w.GetSessionId(), "error", err)
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		out[i] = view
	}
	return connect.NewResponse(&agentfleetv1.ListWorktreesViewResponse{
		Worktrees:     out,
		PvcTotalBytes: resp.GetPvcTotalBytes(),
		PvcFreeBytes:  resp.GetPvcFreeBytes(),
	}), nil
}

func (s *Server) DeleteWorktree(ctx context.Context, req *connect.Request[agentfleetv1.DeleteWorktreeRequest]) (*connect.Response[agentfleetv1.DeleteWorktreeResponse], error) {
	deleted, err := s.e2e.DeleteWorktree(ctx, req.Msg.GetSessionId(), req.Msg.GetRepo(), req.Msg.GetAlsoDeleteBranch())
	if err != nil {
		slog.Error("dashboard DeleteWorktree", "sessionId", req.Msg.GetSessionId(), "repo", req.Msg.GetRepo(), "error", err)
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
	r := repos.Repo{Name: name, URL: url, BaseBranch: req.Msg.GetBaseBranch(), Image: req.Msg.GetImage(), ClusterAccess: req.Msg.GetClusterAccess()}
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
	r := repos.Repo{Name: name, URL: url, BaseBranch: req.Msg.GetBaseBranch(), Image: req.Msg.GetImage(), ClusterAccess: req.Msg.GetClusterAccess()}
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
	return connect.NewResponse(&agentfleetv1.DeleteRepoResponse{}), nil
}

func repoToProto(r repos.Repo) *agentfleetv1.Repo {
	return &agentfleetv1.Repo{
		Name:          r.Name,
		Url:           r.URL,
		BaseBranch:    r.BaseBranch,
		Image:         r.Image,
		ClusterAccess: r.ClusterAccess,
	}
}

// The four RepoProfile CRUD handlers and their five proto-conversion helpers
// used to live here — the environment-recipe editor from docs/adr/0034.
//
// Deleted in docs/adr/0048. The recipe stored, in Postgres, what the agent
// can read off the working tree it is already sitting in. Its three parts
// each went somewhere different: start_cmd is the agent's own `Bash` plus an
// expose(port) call, the toolchain keys became repos.image, and the service
// ingredients became request_service(kind) — which stays fleet-side only
// because it needs cluster RBAC the agent does not have.
//
// cluster-access is the one key that did not collapse into an image, because
// it is a privilege grant rather than a toolchain. It is repos.cluster_access
// now, still data rather than code for docs/adr/0037's own reason: which
// sessions may reach the cluster is a human's decision, editable without a
// redeploy.

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
	return connect.NewResponse(&agentfleetv1.DeletePromptSnippetResponse{}), nil
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
	return connect.NewResponse(&agentfleetv1.DeleteFileResponse{}), nil
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

// DefaultTurnStall is what SessionToProto derives live_state with. The
// sweeps' own copy comes from config (TURN_STALL_MS); this mirrors the
// default so the two agree without threading config through every conversion
// call site, including coreserver's. Diverging only changes how quickly a
// session reads as `stalled` in the UI — nothing is torn down on this clock
// (docs/adr/0040).
const DefaultTurnStall = 90 * time.Second

func rfc3339(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}

func SessionToProto(t sessions.Session) *agentfleetv1.Session {
	return &agentfleetv1.Session{
		Id:             t.ID,
		Repo:           t.Repo,
		Title:          t.Title,
		Description:    t.Description,
		PodPhase:       t.PodPhase,
		PodMessage:     t.PodMessage,
		LastError:      t.LastError,
		AgentSessionId: t.AgentSessionID,
		Model:          t.Model,
		PermissionMode: t.PermissionMode,
		LastActiveAt:   rfc3339(t.LastActiveAt),
		SweptAt:        rfc3339(t.SweptAt),
		ArchivedAt:     rfc3339(t.ArchivedAt),
		// A count, not a boolean. Parallel tool calls each get their own seq,
		// so answering one decision must not report the session unblocked
		// while others are still waiting (docs/adr/0048).
		PendingDecisions: int32(t.PendingDecisions),
		// Derived per read rather than stored, so it can never disagree with
		// the row it was computed from (docs/adr/0040). With `status` gone
		// this is the only status there is.
		LiveState: string(sessions.DeriveLiveState(&t, time.Now(), DefaultTurnStall)),
	}
}

func entryToProto(sessionID string, e transcript.Entry) *agentfleetv1.TranscriptEntry {
	return &agentfleetv1.TranscriptEntry{
		SessionId: sessionID,
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
	case "interrupt":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_INTERRUPT
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
		TaskID:    req.Msg.GetSessionId(),
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
