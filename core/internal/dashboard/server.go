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
	"regexp"
	"slices"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/filediff"
	"github.com/MohammadBnei/agent-fleet/core/internal/filestore"
	"github.com/MohammadBnei/agent-fleet/core/internal/grants"
	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/lokiclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/packs"
	"github.com/MohammadBnei/agent-fleet/core/internal/promclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/proposals"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/schedules"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sttclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

type Server struct {
	sessions  *sessions.Store
	proposals *proposals.Store
	transcr   transcript.Store
	journal   *journal.Store
	repos     *repos.Store
	e2e       *provisionerclient.Client
	files     filestore.Store
	hub       *Hub
	// caps is the fleet's blast-radius bound: how many pods, each running an
	// agent, may exist at once, and the definition pool inside it
	// (docs/adr/0062). Enforced inside sessions.ReserveSlot under an advisory
	// lock rather than checked here, because a read-then-act check is exactly
	// what let CI observe 4 tasks claimed with a cap of 2.
	caps      sessions.Caps
	loki      lokiclient.Querier
	prom      promclient.Querier
	schedules *schedules.Store
	// version is core's own build, stamped in via -ldflags (cmd/core/main.go).
	// It covers the dashboard SPA too — that is compiled into this binary.
	version string
	// model is the operator's override for a repo session whose creator named none (config.ClaudeModel,
	// CLAUDE_MODEL): "" stores NULL, so Claude Code picks (docs/adr/0077).
	model string

	// fileDiffs is where GetFileDiff parks a request for the sidecar's next
	// telemetry tick to pick up. nil disables the RPC (it answers NO_POD),
	// which degrades the console to the captured-tool-input diff it has
	// always had rather than breaking it.
	fileDiffs *filediff.Store

	// stt proxies dictation to ukubi-stt. nil disables Transcribe (it answers
	// Unavailable), which loses voice input and nothing else — same shape as
	// fileDiffs above. The GPU node reboots for gaming by design
	// (infra-bootstrap ADR-0044), so "STT is not there" is a normal state and
	// must not be able to take the console with it.
	stt *sttclient.Client

	// definitions is the defined-agents registry (docs/adr/0062). nil makes
	// definition creates and warms answer Unavailable and ListDefinitions an
	// empty list carrying that as a sync error; repo sessions are unaffected.
	definitions *definitions.Registry
	// packs renders each warm's context packs (docs/adr/0063). nil sends none.
	packs *packs.Store
	// grants stores remembered approvals (docs/adr/0076 §3). nil refuses any
	// scope but "once".
	grants *grants.Store
	// decisions is what AnswerQuestion and RespondToPermission answer
	// through (SetDecisions).
	decisions *decisions.Store
	// openEvent opens an event proposal as the clicking admin (docs/adr/0078
	// §5, dispatch.OpenProposal), injected since AgentService sits above
	// this package. nil refuses event proposals.
	openEvent func(ctx context.Context, p *proposals.Proposal, adminSub string) (threadID string, err error)
}

// SetEventOpener installs how an event proposal opens its definition's thread.
func (s *Server) SetEventOpener(f func(ctx context.Context, p *proposals.Proposal, adminSub string) (string, error)) {
	s.openEvent = f
}

func NewServer(sessionStore *sessions.Store, proposalStore *proposals.Store, transcr transcript.Store, journalStore *journal.Store, repoStore *repos.Store, e2e *provisionerclient.Client, files filestore.Store, hub *Hub, caps sessions.Caps, loki lokiclient.Querier, prom promclient.Querier, scheduleStore *schedules.Store, version, model string) *Server {
	return &Server{sessions: sessionStore, proposals: proposalStore, transcr: transcr, journal: journalStore, repos: repoStore, e2e: e2e, files: files, hub: hub, caps: caps, loki: loki, prom: prom, schedules: scheduleStore, version: version, model: model}
}

var _ agentfleetv1connect.DashboardServiceHandler = (*Server)(nil)

const defaultListLimit = 50

func (s *Server) ListSessions(ctx context.Context, req *connect.Request[agentfleetv1.ListSessionsRequest]) (*connect.Response[agentfleetv1.ListSessionsResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = defaultListLimit
	}
	list, err := s.sessions.List(ctx, limit, req.Msg.GetQuery())
	if err != nil {
		slog.ErrorContext(ctx, "dashboard ListTasks", "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	out := make([]*agentfleetv1.Session, len(list))
	for i, t := range list {
		out[i] = SessionToProto(t)
	}
	return connect.NewResponse(&agentfleetv1.ListSessionsResponse{Sessions: out}), nil
}

func (s *Server) GetSession(ctx context.Context, req *connect.Request[agentfleetv1.GetSessionRequest]) (*connect.Response[agentfleetv1.GetSessionResponse], error) {
	t, err := s.sessions.Get(ctx, req.Msg.GetId())
	// ErrNotFound first: Get signals a missing row with that error, not with a
	// nil session, so checking nil alone would map every unknown id to a 500.
	// The nil check stays as a belt-and-braces guard against a nil-with-no-error
	// return, which would otherwise panic one line down.
	if errors.Is(err, sessions.ErrNotFound) || (err == nil && t == nil) {
		return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, errors.New("session not found"))
	}
	if err != nil {
		slog.ErrorContext(ctx, "dashboard GetSession", "sessionId", req.Msg.GetId(), "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.GetSessionResponse{Session: SessionToProto(*t)}), nil
}

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
// the session's actual instruction is its first transcript entry.
func (s *Server) CreateSession(ctx context.Context, req *connect.Request[agentfleetv1.CreateSessionRequest]) (*connect.Response[agentfleetv1.CreateSessionResponse], error) {
	sess, err := s.createSession(ctx, req.Msg, nil, sessions.CreateParams{})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.CreateSessionResponse{Session: SessionToProto(*sess)}), nil
}

// CreateThread is CreateSession's definition path for AgentService
// (docs/adr/0066), pinned to the head the caller already resolved and
// judged: the same repo checks, plus the thread's owner, visibility,
// interaction and lifecycle.
func (s *Server) CreateThread(ctx context.Context, head definitions.Head, title string, thread sessions.CreateParams) (*sessions.Session, error) {
	return s.createSession(ctx, &agentfleetv1.CreateSessionRequest{Title: &title}, &head, thread)
}

func (s *Server) createSession(ctx context.Context, msg *agentfleetv1.CreateSessionRequest, head *definitions.Head, thread sessions.CreateParams) (*sessions.Session, error) {
	repo, model, mode, effort := msg.GetRepo(), msg.GetModel(), msg.GetPermissionMode(), msg.GetEffort()
	// A definition session (docs/adr/0062) is pinned to the definition's
	// current valid version, and repo, model, mode and effort (its
	// thinking.default) all come from it. The
	// mode is written EXPLICITLY from mode.default, never the column default:
	// workspace-base's is auto (docs/adr/0076), but a definition may narrow.
	if head != nil {
		repo, model, mode = head.Effective.Workspace.Repo, defaultModelOf(head.Effective), head.Effective.Mode.Default
	} else if name := msg.GetDefinition(); name != "" {
		if repo != "" || model != "" || mode != "" || effort != "" {
			return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, errors.New("definition sessions take repo, model, mode and effort from the definition"))
		}
		if s.definitions == nil {
			return nil, errkeys.Connect(connect.CodeUnavailable, errkeys.KeyInternal, errors.New("defined agents are not configured"))
		}
		h, err := s.definitions.Resolve(name)
		switch {
		case errors.Is(err, definitions.ErrUnknown):
			return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, err)
		case errors.Is(err, definitions.ErrInvalid):
			return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, err)
		case err != nil:
			return nil, errkeys.Connect(connect.CodeUnavailable, errkeys.KeyInternal, err)
		}
		head = &h
		repo, model, mode = h.Effective.Workspace.Repo, defaultModelOf(h.Effective), h.Effective.Mode.Default
	}

	repoCfg, err := s.startableRepo(ctx, repo)
	switch {
	case connect.CodeOf(err) == connect.CodeNotFound && head != nil:
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal,
			fmt.Errorf("definition %q targets repo %q, which is not onboarded (add it under manage repos)", head.Name, repo))

	case err != nil:
		return nil, err
	case head != nil && repoCfg.ClusterAccess:
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, errors.New("definition sessions never run on a cluster-access repo"))
	}

	// A definition with no model.default stays NULL (docs/adr/0077): its
	// model comes from the definition, and that one leaves it to Claude Code.
	switch {
	case model == "" && head == nil:
		model = s.fallbackModel()
	case model != "" && !isValidModel(model):
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("invalid model %q", model))
	}
	// Reaches the SDK verbatim as `effort`, like mode via launchMode.
	if effort != "" && !slices.Contains(definitions.EffortLevels, effort) {
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("invalid effort %q", effort))
	}

	// Same allowlist SetPermissionMode uses, for the same reason: the value
	// ends up in a real SDK call via worker/src/session.ts's launchMode, so an
	// unvalidated string here would reach the SDK verbatim. Empty is allowed
	// and means the column default — "auto" since docs/adr/0061.
	if mode != "" && !validPermissionModes[mode] {
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("invalid permission mode %q", mode))
	}

	params := sessions.CreateParams{
		Repo:           repo,
		Title:          msg.GetTitle(),
		Description:    msg.GetDescription(),
		Model:          model,
		PermissionMode: mode,
		Effort:         effort,
		Owner:          thread.Owner,
		Visibility:     thread.Visibility,
		Interaction:    thread.Interaction,
		Lifecycle:      thread.Lifecycle,
		Invoker:        thread.Invoker,
		ThreadKey:      thread.ThreadKey,
	}
	// No capacity check for either kind: a create provisions nothing
	// (docs/adr/0048 §1), so ReserveSlot at warm is the one capacity gate
	// (docs/adr/0062 §5).
	if head != nil {
		params.DefinitionName, params.DefinitionSHA = head.Name, head.SHA
	}
	id, err := s.sessions.Create(ctx, params)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard CreateSession", "repo", repo, "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard CreateSession", "sessionId", id, "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	slog.InfoContext(ctx, "dashboard CreateSession", "sessionId", id, "repo", repo, "definition", params.DefinitionName, "sha", params.DefinitionSHA)
	return sess, nil
}

// ListDefinitions lists each present definition's current version
// (docs/adr/0062). Admin-only through the same interceptor as every method.
func (s *Server) ListDefinitions(_ context.Context, _ *connect.Request[agentfleetv1.ListDefinitionsRequest]) (*connect.Response[agentfleetv1.ListDefinitionsResponse], error) {
	if s.definitions == nil {
		return connect.NewResponse(&agentfleetv1.ListDefinitionsResponse{SyncErrors: []string{"defined agents are not configured"}}), nil
	}
	heads, syncErrs := s.definitions.List()
	out := &agentfleetv1.ListDefinitionsResponse{SyncErrors: syncErrs}
	for _, h := range heads {
		d := &agentfleetv1.AgentDefinition{Name: h.Name, Sha: h.SHA, Valid: h.SHA != "", Error: h.Err}
		if h.Effective != nil {
			d.Repo, d.Model, d.PermissionMode = h.Effective.Workspace.Repo, defaultModelOf(h.Effective), h.Effective.Mode.Default
			d.AudienceGroup = h.Effective.Audience.Group
			d.SyncedAt = h.SyncedAt.UTC().Format(time.RFC3339)
		}
		out.Definitions = append(out.Definitions, d)
	}
	return connect.NewResponse(out), nil
}

// ArchiveSession marks a session finished by a human — the only terminal
// state in the fleet, because it is the only one a machine can compute.
//
// Tears down any live pod and dismisses the proposal that spawned it, if any:
// that is what re-arms the dedup key so a still-firing alert or the next schedule
// tick can propose the same work again.
func (s *Server) ArchiveSession(ctx context.Context, req *connect.Request[agentfleetv1.ArchiveSessionRequest]) (*connect.Response[agentfleetv1.ArchiveSessionResponse], error) {
	id := req.Msg.GetSessionId()
	if err := s.sessions.Archive(ctx, id); err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, err)
		}
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	// Best-effort, logged not propagated: the archive itself has committed,
	// and a teardown hiccup should not fail it. The reconcile loop notices a
	// pod whose session is archived on its next pass anyway.
	if _, err := s.e2e.TearDownSession(ctx, id, agentfleetv1.SessionKind_SESSION_KIND_WORKER); err != nil {
		slog.WarnContext(ctx, "dashboard ArchiveSession: worker teardown failed", "sessionId", id, "error", err)
	}
	// Same revocation as sessions.Loop.tearDown: an archived session is never
	// warmed again, so its lease would otherwise stay valid forever. This is
	// the second of exactly two teardown paths — if a third appears, it needs
	// this too.
	if err := s.sessions.ClearLease(ctx, id); err != nil {
		slog.WarnContext(ctx, "dashboard ArchiveSession: clearing lease failed", "sessionId", id, "error", err)
	}
	if err := s.proposals.DismissForSession(ctx, id); err != nil {
		slog.WarnContext(ctx, "dashboard ArchiveSession: proposal dismiss failed", "sessionId", id, "error", err)
	}
	return connect.NewResponse(&agentfleetv1.ArchiveSessionResponse{}), nil
}

func (s *Server) ListProposals(ctx context.Context, _ *connect.Request[agentfleetv1.ListProposalsRequest]) (*connect.Response[agentfleetv1.ListProposalsResponse], error) {
	list, err := s.proposals.ListOpen(ctx)
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
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
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	if p == nil {
		return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, errors.New("no such proposal"))
	}
	if p.Event != nil {
		return s.openEventProposal(ctx, p)
	}
	// Checked before anything is consumed: a schedule or alert must not open
	// a session on a repo the admin switched off, and the proposal stays open.
	if _, err := s.startableRepo(ctx, p.Repo); err != nil {
		return nil, err
	}

	// Title for both the label fields: the body is not a label, it is the
	// instruction, and it goes out as the first message below. Putting it in
	// `description` made the list render an entire alert payload as a row.
	// No permission mode: a proposal-opened session takes the column default,
	// "auto" (docs/adr/0061) — so this body runs unattended except `rm`/`sudo`.
	// That was a deliberate call, ADR records the prompt-injection trade-off.
	id, err := s.sessions.Create(ctx, sessions.CreateParams{Repo: p.Repo, Title: p.Title, Description: p.Title, Model: s.fallbackModel()})
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	if err := s.proposals.Open(ctx, p.ID, id); err != nil {
		if errors.Is(err, proposals.ErrNotOpen) {
			// Someone else won the race. Delete the session we just made
			// rather than leaving an orphan behind — it has no pod and no
			// transcript, so this is a clean rollback.
			if delErr := s.sessions.Delete(ctx, id); delErr != nil {
				slog.WarnContext(ctx, "dashboard OpenFromProposal: rollback failed", "sessionId", id, "error", delErr)
			}
			return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, errors.New("proposal is already opened or dismissed"))
		}
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}

	// The proposal's body IS the instruction, so it has to be sent as a real
	// first message — not left in `description`, which is a human-facing label
	// the agent never sees (a description it cannot read is one that silently
	// drifts from what it was actually asked to do). Without this the approved
	// session opened with no pod and nothing to do, and the finding that
	// prompted it never reached the agent at all.
	//
	// Warm BEFORE appending, for the same reason PostMessage does: resumeFromSeq
	// is computed from LatestSeq at provisioning time, so a message appended
	// first lands below the new pod's cursor and is never delivered.
	if _, err := s.WarmIfIdle(ctx, id); err != nil {
		// Roll the whole thing back. The comment here used to say the session
		// and the proposal link both stand, so a human could send the message
		// by hand — that was wrong twice over: the proposal is already
		// consumed at this point (gone from ListOpen, and a second click gets
		// FailedPrecondition), and the instruction it carried lives only in
		// its body, so the session left behind has no pod, no transcript and
		// no record of what it was for. The likely trigger is the ordinary
		// one, ErrAtCapacity.
		slog.ErrorContext(ctx, "dashboard OpenFromProposal: warm failed", "sessionId", id, "proposalId", p.ID, "error", err)
		s.rollbackOpen(ctx, p.ID, id)
		return nil, err
	}
	if _, err := s.transcr.Append(ctx, id, "human", p.Body, "discussion", uuid.NewString()); err != nil {
		slog.ErrorContext(ctx, "dashboard OpenFromProposal: append failed", "sessionId", id, "proposalId", p.ID, "error", err)
		s.rollbackOpen(ctx, p.ID, id)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}

	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	slog.InfoContext(ctx, "dashboard OpenFromProposal", "proposalId", p.ID, "sessionId", id, "repo", p.Repo)
	return connect.NewResponse(&agentfleetv1.OpenFromProposalResponse{Session: SessionToProto(*sess)}), nil
}

// openEventProposal opens the definition's thread an event proposal was filed
// for, as the clicking admin (docs/adr/0078 §5). The thread comes first and
// the proposal is marked after: Open's idempotency key makes a second click
// land on the same thread, and a failed Open leaves the proposal standing to
// click again. It is marked only while its body is the one delivered: an
// event folded in meanwhile would otherwise retire with it, so the current
// body is delivered again (a new key, the same thread). The repo check is
// Open's own, against the definition's repo.
func (s *Server) openEventProposal(ctx context.Context, p *proposals.Proposal) (*connect.Response[agentfleetv1.OpenFromProposalResponse], error) {
	if p.SessionID != nil || p.DismissedAt != nil {
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, errors.New("proposal is already opened or dismissed"))
	}
	if s.openEvent == nil {
		return nil, errkeys.Connect(connect.CodeUnavailable, errkeys.KeyInternal, errors.New("event proposals need the defined-agents registry"))
	}
	id, _ := auth.FromContext(ctx)
	sub := id.Sub
	if sub == "" {
		sub = id.Email // a cookie minted before Sub existed
	}
	if sub == "" {
		return nil, errkeys.Connect(connect.CodeUnauthenticated, errkeys.KeyInternal, errors.New("no identity on the request: an event proposal is opened as its admin"))
	}
	var threadID string
	var err error
	for attempt := 0; ; attempt++ {
		threadID, err = s.openEvent(ctx, p, sub)
		if err != nil {
			slog.ErrorContext(ctx, "dashboard OpenFromProposal: open event thread", "proposalId", p.ID, "error", err)
			return nil, err
		}
		err = s.proposals.OpenIfBody(ctx, p.ID, threadID, p.Body)
		if !errors.Is(err, proposals.ErrBodyChanged) || attempt == 2 {
			break
		}
		if p, err = s.proposals.Get(ctx, p.ID); err != nil || p == nil {
			return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, fmt.Errorf("reread proposal: %v", err))
		}
		if p.SessionID != nil || p.DismissedAt != nil {
			err = proposals.ErrNotOpen
			break
		}
	}
	switch {
	case errors.Is(err, proposals.ErrNotOpen):
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, errors.New("proposal is already opened or dismissed"))
	case errors.Is(err, proposals.ErrBodyChanged):
		return nil, errkeys.Connect(connect.CodeAborted, errkeys.KeyInternal, errors.New("events keep arriving on this thread; open it again"))
	case err != nil:
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	sess, err := s.sessions.Get(ctx, threadID)
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	slog.InfoContext(ctx, "dashboard OpenFromProposal: event thread", "proposalId", p.ID, "sessionId", threadID, "definition", p.Event.Definition)
	return connect.NewResponse(&agentfleetv1.OpenFromProposalResponse{Session: SessionToProto(*sess)}), nil
}

// rollbackOpen undoes an OpenFromProposal that could not be completed: the
// session goes away and the proposal returns to the open list, so the human
// who clicked can click again once whatever failed is fixed. Best effort by
// design — it runs on a path that is already failing, and a warning here is
// more useful than masking the original error with the rollback's.
func (s *Server) rollbackOpen(ctx context.Context, proposalID, sessionID string) {
	// Tear the pod down BEFORE deleting the row, the same order DeleteSession
	// uses. The append-failure path gets here with WarmIfIdle already
	// succeeded, so a pod exists — and sessions.Delete is a bare SQL DELETE.
	// Dropping the row first orphans that pod AND frees a concurrency slot it
	// still occupies (ReserveSlot counts rows), which is how the fleet ends up
	// over MAX_LIVE_SESSIONS with no way to see why.
	if _, err := s.e2e.TearDownSession(ctx, sessionID, agentfleetv1.SessionKind_SESSION_KIND_WORKER); err != nil {
		slog.WarnContext(ctx, "dashboard OpenFromProposal: rollback teardown failed", "sessionId", sessionID, "error", err)
	}
	if err := s.sessions.Delete(ctx, sessionID); err != nil {
		slog.WarnContext(ctx, "dashboard OpenFromProposal: rollback delete failed", "sessionId", sessionID, "error", err)
	}
	if err := s.proposals.Unclaim(ctx, proposalID, sessionID); err != nil {
		slog.WarnContext(ctx, "dashboard OpenFromProposal: rollback unclaim failed", "proposalId", proposalID, "error", err)
	}
}

func (s *Server) DismissProposal(ctx context.Context, req *connect.Request[agentfleetv1.DismissProposalRequest]) (*connect.Response[agentfleetv1.DismissProposalResponse], error) {
	if err := s.proposals.Dismiss(ctx, req.Msg.GetProposalId()); err != nil {
		if errors.Is(err, proposals.ErrNotOpen) {
			return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, err)
		}
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
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
		Payload:   p.Payload,
		CreatedAt: p.CreatedAt.Format(time.RFC3339),
	}
	out.SessionId = p.SessionID
	return out
}

// fallbackModel is the model for a repo session whose creator named none:
// CLAUDE_MODEL, the operator's override, or "" — stored as NULL, so the worker
// passes no model and Claude Code picks (docs/adr/0077). The fleet used to pin
// claude-opus-5-5 here. Shared by CreateSession and OpenFromProposal.
func (s *Server) fallbackModel() string {
	return s.model
}

// defaultModelOf is a definition's model.default, "" when it sets no model
// (optional since docs/adr/0077).
func defaultModelOf(s *definitions.Spec) string {
	if s.Model == nil {
		return ""
	}
	return s.Model.Default
}

// pickableModels is validModels minus the dated snapshots kept only so old
// rows stay valid: what the New Session dialog offers (ListRepos.models).
func pickableModels() []string {
	var out []string
	for m := range validModels {
		if !datedModel.MatchString(m) {
			out = append(out, m)
		}
	}
	slices.Sort(out)
	return out
}

var datedModel = regexp.MustCompile(`-\d{8}$`)

// validModels is the allowlist of Claude models that can be selected per-task.
// This prevents injection of arbitrary model names and ensures only supported
// models are used.
//
// Checked on CreateSession only, and only when a model is explicitly supplied —
// nothing re-validates a stored value on read. So an entry here is a claim that
// the API will accept the string, and a wrong one fails at dispatch rather than
// at selection.
//
// `claude-haiku-4-5-20250929` used to sit in this map and was never a real
// model: 20250929 is Sonnet 4.5's date, pasted onto Haiku's name. Haiku is
// 20251001. Picking Haiku in the dashboard passed this check and then failed
// against the API, so the option had never worked. Aliases now, not dated
// snapshots — an alias tracks its model and cannot rot this way.
var validModels = map[string]bool{
	"claude-opus-5-5":  true,
	"claude-opus-5":    true,
	"claude-opus-4-8":  true,
	"claude-sonnet-5":  true,
	"claude-haiku-4-5": true,
	// Kept so sessions created on them stay re-selectable.
	"claude-sonnet-4-5-20250929": true,
	"claude-opus-4-5-20251101":   true,
}

func isValidModel(model string) bool {
	return validModels[model]
}

// maxTranscriptPage bounds every GetTranscript response. It was the only
// page size before the window selector existed, and a client asking for more
// is clamped rather than refused.
const maxTranscriptPage = 1000

// transcriptWindow turns the request's (since_seq, limit, before_seq) into the
// (sinceSeq, limit) pair ReadSince actually takes, given the session's current
// latest seq (one past the highest, per Store.LatestSeq).
//
// It relies on seq being dense — Append assigns MAX(seq)+1 per session — so
// "the newest N" is "everything from latest-N". Retention could in principle
// punch holes, which is why the before_seq caller trims the result rather than
// trusting the arithmetic.
//
// ponytail: dense-seq arithmetic over a real ORDER BY seq DESC LIMIT n query,
// so this needs no new Store method and no fake to grow one. If retention ever
// deletes enough mid-session rows for a short page to be noticeable, add
// ReadBefore to the interface and delete this.
func transcriptWindow(sinceSeq, latestSeq int64, limit int32, beforeSeq *int64) (int64, int) {
	page := int(limit)
	if page <= 0 || page > maxTranscriptPage {
		page = maxTranscriptPage
	}
	switch {
	case beforeSeq != nil:
		return max(*beforeSeq-int64(page), 0), page
	case limit > 0:
		// Newest page. latestSeq is one past the highest, so this is exactly
		// the last `page` entries of a dense transcript.
		return max(latestSeq-int64(page), sinceSeq), page
	default:
		return sinceSeq, page
	}
}

func (s *Server) GetTranscript(ctx context.Context, req *connect.Request[agentfleetv1.ReadTranscriptSinceRequest]) (*connect.Response[agentfleetv1.ReadTranscriptSinceResponse], error) {
	sessionID := req.Msg.GetSessionId()
	var latest int64
	if req.Msg.Limit != nil && req.Msg.BeforeSeq == nil {
		var err error
		if latest, err = s.transcr.LatestSeq(ctx, sessionID); err != nil {
			slog.ErrorContext(ctx, "dashboard GetTranscript latestSeq", "sessionId", sessionID, "error", err)
			return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
		}
	}
	since, page := transcriptWindow(req.Msg.GetSinceSeq(), latest, req.Msg.GetLimit(), req.Msg.BeforeSeq)
	entries, next, err := s.transcr.ReadSince(ctx, sessionID, since, page)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard GetTranscript", "sessionId", sessionID, "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	if b := req.Msg.BeforeSeq; b != nil {
		// A gap in seq would let the page run past the window's own edge and
		// hand the client entries it already has. Cheaper to trim than to
		// assume density.
		for len(entries) > 0 && entries[len(entries)-1].Seq >= *b {
			entries = entries[:len(entries)-1]
		}
		// This is a backwards read: the live stream cursor must not follow it.
		next = *b
	}
	out := make([]*agentfleetv1.TranscriptEntry, len(entries))
	for i, e := range entries {
		out[i] = entryToProto(sessionID, e)
	}
	return connect.NewResponse(&agentfleetv1.ReadTranscriptSinceResponse{Entries: out, NextSeq: next}), nil
}

// streamMaxAge bounds how long StreamTranscript will hold a connection open
// without sending anything. A var, not a const, so tests can shrink it.
//
// An idle session sends zero bytes for as long as nobody types — hours, if the
// agent is thinking or blocked on a permission decision. Cloudflare's free plan
// answers 100 seconds of origin silence with a 524, which is the sole reason
// fleet.bnei.dev is DNS-only while every other *.bnei.dev host is proxied
// (k8s/core.yaml, infra-bootstrap ADR-0038) — and being grey costs it the WAF,
// the geo rules, the origin lock and a private origin IP.
//
// Ending the response is deliberately the whole mechanism. subscribeTranscript
// resubscribes from its own cursor on a CLEAN end, not only on an error, so no
// entry is skipped and nothing changes on the client. The alternative — a
// keepalive frame on the stream — would need a sentinel value that every
// present and future consumer of `stream TranscriptEntry` has to know to
// ignore, and any tab still running older JS across the deploy would take the
// sentinel as real, advance its cursor and replay the transcript.
var streamMaxAge = 90 * time.Second

func (s *Server) StreamTranscript(ctx context.Context, req *connect.Request[agentfleetv1.StreamTranscriptRequest], stream *connect.ServerStream[agentfleetv1.TranscriptEntry]) error {
	ch, cancel := s.hub.Subscribe(req.Msg.GetSessionId(), req.Msg.GetSinceSeq())
	defer cancel()

	deadline := time.NewTimer(streamMaxAge)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			// Not an error: the client reconnects from its cursor.
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

// SetFileDiffStore wires the shared parking lot in after construction, so
// this server and coreserver hold the same one.
func (s *Server) SetFileDiffStore(store *filediff.Store) { s.fileDiffs = store }

func (s *Server) SetSTTClient(c *sttclient.Client) { s.stt = c }

func (s *Server) SetDefinitions(r *definitions.Registry) { s.definitions = r }

func (s *Server) SetPacks(p *packs.Store) { s.packs = p }

// GetFileDiff answers "what actually changed in this file" for a file the
// CHANGES panel listed but the console cannot reconstruct — anything a Bash
// command wrote: sed, a formatter, a codegen script. The panel itself has
// always been honest about those files, because it comes from the sidecar's
// `git diff --numstat` rather than from captured tool inputs; opening one was
// the half that was reconstructed, and it showed nothing.
//
// A poll, not a fetch. Core cannot dial the sidecar (internal/filediff's
// package doc has the topology), so the first call arms the request and returns
// PENDING; the sidecar carries it out on its next 5s tick and the answer is
// READY on a later call. The console retries.
func (s *Server) GetFileDiff(ctx context.Context, req *connect.Request[agentfleetv1.GetFileDiffRequest]) (*connect.Response[agentfleetv1.GetFileDiffResponse], error) {
	sessionID, path := req.Msg.GetSessionId(), req.Msg.GetPath()
	if sessionID == "" || path == "" {
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, errors.New("session_id and path are required"))
	}
	reply := func(st agentfleetv1.GetFileDiffResponse_Status, diff string) *connect.Response[agentfleetv1.GetFileDiffResponse] {
		return connect.NewResponse(&agentfleetv1.GetFileDiffResponse{Status: st, Diff: diff})
	}
	if s.fileDiffs == nil {
		return reply(agentfleetv1.GetFileDiffResponse_STATUS_NO_POD, ""), nil
	}

	// Collect before checking the pod. An answer that arrived just before the
	// pod went away is still a true diff of the tree that produced it, and
	// losing it to a phase flip mid-poll is the one way this reads as broken
	// rather than as "the session ended".
	if diff, ok := s.fileDiffs.Take(sessionID, path); ok {
		if diff == "" {
			// git reported nothing: committed, reverted, or never tracked.
			// Terminal, and distinct from PENDING so the console stops.
			return reply(agentfleetv1.GetFileDiffResponse_STATUS_EMPTY, ""), nil
		}
		return reply(agentfleetv1.GetFileDiffResponse_STATUS_READY, diff), nil
	}

	t, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, err)
		}
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	// No pod, no working tree, and no amount of polling will produce one.
	// Saying so is the whole point: the console's old copy claimed the change
	// "did not go through an Edit or Write tool call", which is a guess, and
	// on a torn-down session it was the wrong one.
	if !sessions.IsPodPhaseLive(t.PodPhase) {
		return reply(agentfleetv1.GetFileDiffResponse_STATUS_NO_POD, ""), nil
	}
	if !s.fileDiffs.Want(sessionID, path) {
		return nil, errkeys.Connect(connect.CodeResourceExhausted, errkeys.KeyInternal, errors.New("too many diffs pending for this session"))
	}
	return reply(agentfleetv1.GetFileDiffResponse_STATUS_PENDING, ""), nil
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
		slog.ErrorContext(ctx, "dashboard Kill", "sessionId", sessionID, "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	if err := s.sessions.MarkStopRequested(ctx, sessionID); err != nil {
		slog.ErrorContext(ctx, "dashboard Kill: mark stop requested", "sessionId", sessionID, "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
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
		slog.ErrorContext(ctx, "dashboard Interrupt", "sessionId", sessionID, "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
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
// "auto" is the SDK 0.3.233 mode where a model classifier answers the
// ordinary prompts instead of a human (docs/adr/0052) — the dashboard's
// plan-approval path sets it, so it has to be reachable through here.
// "bypassPermissions" is deliberately absent as of docs/adr/0053: "auto"
// does the same job without a launch profile, and the worker answers
// everything but `rm`/`sudo` in it. A stored row still carrying the old
// value is migrated to "auto" by db/migrations/000004.
var validPermissionModes = map[string]bool{
	"default":     true,
	"plan":        true,
	"acceptEdits": true,
	"auto":        true,
	"dontAsk":     true,
}

// SetPermissionMode sets the running session's SDK permission mode
// (docs/adr/0027, extended by the sessions redesign supersession of
// docs/adr/0021/0025 — this is now the only mode lever, Approve is gone).
// "auto" is the one that grants real authority — the worker allows
// everything but `rm`/`sudo` in it without asking (docs/adr/0053) — so the
// dashboard puts it behind a confirm. Persists to
// tasks.permission_mode (durable "what mode is this session in right now"
// for the dashboard's mode picker) in addition to the transcript append
// that actually reaches the running worker.
func (s *Server) SetPermissionMode(ctx context.Context, req *connect.Request[agentfleetv1.SetPermissionModeRequest]) (*connect.Response[agentfleetv1.SetPermissionModeResponse], error) {
	sessionID := req.Msg.GetSessionId()
	mode := req.Msg.GetMode()
	if !validPermissionModes[mode] {
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("invalid permission mode %q", mode))
	}
	// Persist first: the store's write carries the definition guard (a
	// definition only narrows, for admins too: docs/adr/0062), and the append
	// is what reaches the running worker, so it must never precede a refusal.
	if err := s.sessions.SetPermissionMode(ctx, sessionID, mode); err != nil {
		switch {
		case errors.Is(err, sessions.ErrModeNotAllowed):
			// InvalidArgument, where the pod's CoreService call gets
			// PermissionDenied: see coreserver's SetPermissionMode.
			return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, err)
		case errors.Is(err, sessions.ErrDefinitionInvalid):
			return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, err)
		case errors.Is(err, sessions.ErrNotFound):
			return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, err)
		}
		slog.ErrorContext(ctx, "dashboard SetPermissionMode: persist", "sessionId", sessionID, "mode", mode, "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	if _, err := s.transcr.Append(ctx, sessionID, "human", mode, "permission_mode", uuid.NewString()); err != nil {
		slog.ErrorContext(ctx, "dashboard SetPermissionMode", "sessionId", sessionID, "mode", mode, "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.SetPermissionModeResponse{}), nil
}

// AnswerQuestion answers a pending QUESTION entry — and warms the session
// first, for the same reason PostMessage does. See WarmIfIdle.
//
// This is what makes docs/adr/0050 true. That ADR decided a question outlives
// its pod: the answer is delivered to the next pod on warm, replayed above
// RESUME_FROM_SEQ like any other human entry. It never worked, because this
// function appended without warming — and resumeFromSeq is LatestSeq
// (MAX(seq)+1) read at provisioning time, so an answer written first sits
// BELOW the cursor of the pod that comes later and is never read. The worker
// has had the receiving branch the whole time (worker/src/session.ts, the
// entry.type === "answer" case); nothing ever reached it.
//
// Two consequences worth knowing, both accepted:
//   - Warming runs ReserveSlot, which stale-closes any pending older
//     worker's permission_request on this session. Answering a question
//     therefore auto-denies such a permission left over from the dead pod. That permission
//     was already unanswerable, but it is an auto-decision triggered by
//     answering something else.
//   - This RPC can now fail: ResourceExhausted at the live-pod cap,
//     FailedPrecondition on a swept session. That is honest — the answer had
//     nowhere to go in those cases either, it just failed silently before.
func (s *Server) AnswerQuestion(ctx context.Context, req *connect.Request[agentfleetv1.AnswerQuestionRequest]) (*connect.Response[agentfleetv1.AppendResponse], error) {
	seq, err := s.answer(ctx, req.Msg.GetSessionId(), req.Msg.GetSeq(), "question", decisions.Reply{Answers: req.Msg.GetAnswersJson()})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.AppendResponse{Seq: seq}), nil
}

// RespondToPermission answers a pending PERMISSION_REQUEST entry — the
// generalized canUseTool prompt-and-wait gate's counterpart to
// AnswerQuestion, same answer-by-seq shape, kept as a sibling RPC
// rather than overloaded onto AnswerQuestion since the payload differs
// ({behavior, message} JSON vs. free-form answers JSON).
//
// An older worker's request does NOT warm, where AnswerQuestion above does.
// ReserveSlot stale-closes its unanswered permission_request rows on every
// warm, so warming here would auto-deny the very request being answered, a
// millisecond before recording the human's actual decision. That asymmetry is
// docs/adr/0050: such a permission is bound to one live pod's canUseTool
// promise, which is why the idle sweep refuses to reap a pod holding one (see
// ListIdleSessionIDs).
//
// A non-blocking worker's request (stamped "nb": 1, docs/adr/0082 §5) warms,
// then appends, as a question does: ReserveSlot leaves it open, and its answer
// reaches the next pod as a message (decisions.Answer).
//
// decision_json is read for its behavior and message only: decisions.Answer
// writes the rest, so only core writes "grant" (docs/adr/0076 §3), and a plan
// answered here is the console's approvePlan, which switched the mode first.
func (s *Server) RespondToPermission(ctx context.Context, req *connect.Request[agentfleetv1.RespondToPermissionRequest]) (*connect.Response[agentfleetv1.AppendResponse], error) {
	var d struct {
		Behavior string `json:"behavior"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal([]byte(req.Msg.GetDecisionJson()), &d); err != nil {
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, errors.New("decision_json must be a JSON object"))
	}
	seq, err := s.answer(ctx, req.Msg.GetSessionId(), req.Msg.GetSeq(), "permission_request", decisions.Reply{
		Behavior: d.Behavior, Message: d.Message, Scope: req.Msg.GetScope(), RuleForm: req.Msg.GetRuleForm(),
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.AppendResponse{Seq: seq}), nil
}

// SetDecisions wires the store the answer RPCs answer through
// (docs/adr/0082 §5). Unset, they answer Unavailable.
func (s *Server) SetDecisions(d *decisions.Store) { s.decisions = d }

// SetMode is SetPermissionMode for decisions.Answer's plan approval.
func (s *Server) SetMode(ctx context.Context, sessionID, mode string) error {
	_, err := s.SetPermissionMode(ctx, connect.NewRequest(&agentfleetv1.SetPermissionModeRequest{SessionId: sessionID, Mode: mode}))
	return err
}

// answer is decisions.Answer for the request at seq, which must be a
// question (wantType "question") or a permission or plan request.
func (s *Server) answer(ctx context.Context, sessionID string, seq int64, wantType string, a decisions.Reply) (int64, error) {
	if s.decisions == nil {
		return 0, errkeys.Connect(connect.CodeUnavailable, errkeys.KeyInternal, errors.New("decisions are not configured"))
	}
	r, err := s.decisions.Request(ctx, sessionID, seq)
	// A card is answered by id only (AnswerDecision): it is no question,
	// and read as a permission here it would take a behavior.
	if errors.Is(err, decisions.ErrNotFound) || (err == nil && (r.Kind == decisions.Card || (r.Kind == decisions.Question) != (wantType == "question"))) {
		return 0, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("seq %d is not a %s", seq, wantType))
	}
	if err != nil {
		return 0, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	n, err := s.decisions.Answer(ctx, s, s.transcr, r, a)
	if err != nil {
		return 0, AnswerErr(ctx, "answer", err)
	}
	return n, nil
}

// AnswerErr is decisions.Answer's error for the wire. A connect error (a
// warm's, a mode switch's) passes through.
func AnswerErr(ctx context.Context, op string, err error) error {
	switch {
	case errors.Is(err, transcript.ErrAlreadyAnswered):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, decisions.ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return grantErr(ctx, op, err)
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
// Transcribe proxies one chunk of a dictation to ukubi-stt.
//
// The dashboard cannot call that service directly: core allows no CORS and its
// session cookie is SameSite=Lax, so a cross-origin call carries no identity —
// and handing the browser an STT bearer token to work around that would give
// every dashboard user a credential for the GPU. Proxying keeps the token here.
func (s *Server) Transcribe(ctx context.Context, req *connect.Request[agentfleetv1.TranscribeRequest]) (*connect.Response[agentfleetv1.TranscribeResponse], error) {
	if s.stt == nil {
		return nil, errkeys.Connect(connect.CodeUnavailable, errkeys.KeyInternal, errors.New("speech-to-text is not configured"))
	}

	streamID := req.Msg.GetStreamId()
	if streamID == "" {
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, errors.New("stream_id is required"))
	}
	audio := req.Msg.GetAudio()
	// Empty audio is only meaningful as a close: it tells the service to flush
	// the encoder's buffered tail and release the recognizer.
	if len(audio) == 0 && !req.Msg.GetLast() {
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, errors.New("audio is required"))
	}

	// The identity half of the session id. An empty one would collapse every
	// caller into one recognizer — and FLEET_AUTH_DISABLED makes that reachable
	// rather than theoretical, so it is refused here instead of silently
	// producing a shared stream.
	identity, ok := auth.FromContext(ctx)
	if !ok || identity.Email == "" {
		return nil, errkeys.Connect(connect.CodeUnauthenticated, errkeys.KeyInternal, errors.New("no identity on the request"))
	}

	text, err := s.stt.Transcribe(ctx, s.stt.SessionID(identity.Email, streamID), audio, req.Msg.GetLast(), req.Msg.GetLanguage())
	if err != nil {
		if sttclient.Unavailable(err) {
			// The GPU is busy, or its node is down. Both are states ADR-0044
			// accepts; neither is a bug in this console.
			return nil, errkeys.Connect(connect.CodeUnavailable, errkeys.KeyInternal, fmt.Errorf("speech-to-text unavailable: %w", err))
		}
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.TranscribeResponse{Text: text}), nil
}

func (s *Server) PostMessage(ctx context.Context, req *connect.Request[agentfleetv1.PostMessageRequest]) (*connect.Response[agentfleetv1.AppendResponse], error) {
	sessionID := req.Msg.GetSessionId()
	text := req.Msg.GetText()
	if text == "" {
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, errors.New("text is required"))
	}
	// Warm BEFORE appending, never after — see WarmIfIdle. This ordering is
	// also the whole mechanism by which a first message boots a session
	// (docs/adr/0048).
	if _, err := s.WarmIfIdle(ctx, sessionID); err != nil {
		return nil, err
	}
	seq, err := s.transcr.Append(ctx, sessionID, "human", text, "discussion", uuid.NewString())
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
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
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
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
			return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, err)
		}
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	// An explicit click deserves an explicit answer, where PostMessage's
	// shared path treats the same condition as a silent no-op.
	if sessions.IsPodPhaseLive(t.PodPhase) {
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, errors.New("session already has a live pod"))
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
//
// # Warm THEN append, never the reverse
//
// This is the fleet's most load-bearing ordering, and it lives here because it
// is a property of the resumeFromSeq read below, not of any one caller.
//
// resumeFromSeq is LatestSeq — MAX(seq)+1 — read at provisioning time, and the
// pod streams only entries at or above it. So an entry appended BEFORE this
// function runs lands one below the new pod's cursor and is never delivered.
// Not "delivered late": an entry appended to a cold session is undeliverable
// forever, because the next warm's cursor will be above it too.
//
// Every caller that wants its append to reach the agent must call this first:
// PostMessage, OpenFromProposal, PromptSession, AnswerQuestion. Each carries a
// one-line reminder pointing here rather than repeating this.
//
// RespondToPermission is the deliberate exception and must NOT warm — see its
// own comment.
//
// ponytail: the truncation above is the root of a bug class, not just a rule to
// follow. A pod cannot ask "what was appended while I was gone", because nothing
// records what the previous pod consumed — and resuming from the oldest
// unanswered entry instead would re-deliver stale abort/interrupt rows. The
// upgrade path is a cold_from_seq column set on append-while-cold and cleared on
// warm; worth building the moment a third caller needs it.
func (s *Server) WarmIfIdle(ctx context.Context, sessionID string) (podName string, err error) {
	t, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sessions.ErrNotFound) {
			return "", errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, err)
		}
		return "", errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	if sessions.IsPodPhaseLive(t.PodPhase) {
		return "", nil
	}
	// A swept session has had its working directory and SDK state reclaimed,
	// so there is nothing to resume into — booting a pod would hand the agent
	// an empty directory and a resume id pointing at a deleted transcript.
	if t.SweptAt != nil {
		return "", errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, decisions.CannotResume(
			errors.New("session was swept by the retention GC — its working directory is gone, so it cannot be resumed")))

	}

	repoCfg, err := s.repos.Get(ctx, t.Repo)
	if err != nil {
		return "", errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	if repoCfg == nil {
		return "", errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, fmt.Errorf("unknown repo %q", t.Repo))
	}
	// docs/adr/0064: a definition session gets its profile, the provisioner's
	// default image and no tool keys. repos.image is ignored: the profile's
	// worker-side hardening (FLEET_DEFINITION) needs this release's worker, and
	// an override is not guaranteed to be one.
	var profile *agentfleetv1.DefinitionPodProfile
	image := repoCfg.Image
	if t.DefinitionName != nil {
		if profile, err = s.definitionWarmable(ctx, t, repoCfg); err != nil {
			return "", err
		}
		image = ""
	}
	// Rendered before the lease, so a failed read leaves nothing to undo.
	genome, rendered, err := s.renderGenome(ctx, t)
	if err != nil {
		return "", errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}

	// The cap check and the lease mint happen together, under an advisory
	// lock, inside ReserveSlot. Checking here and acting after would be the
	// read-then-act race CI once observed as 4 tasks claimed with a cap of 2.
	leaseID, err := s.sessions.ReserveSlot(ctx, sessionID, s.caps)
	if err != nil {
		// Every ErrAtCapacity is wrapped with which cap was hit, so the
		// message is passed through as-is.
		switch {
		case errors.Is(err, sessions.ErrAtCapacity):
			return "", errkeys.Connect(connect.CodeResourceExhausted, errkeys.KeyInternal, err)
		case errors.Is(err, sessions.ErrDefinitionInvalid):
			return "", errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, err)
		case errors.Is(err, sessions.ErrNotFound):
			return "", errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, decisions.CannotResume(errors.New("session is archived")))
		}
		return "", errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
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
		return "", errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	// Both from repoCfg, which is the row already read above. This used to
	// re-fetch the same repo through a toolKeysFor helper — two reads of one
	// row, which a concurrent "manage repos" edit could have straddled, so a
	// pod could get one repo's privilege grant and another's image.
	toolKeys := provisionerclient.ToolKeysFor(repoCfg.ClusterAccess)
	pod, err := s.e2e.CreateWorkerPod(ctx, sessionID, t.Repo, repoCfg.URL, repoCfg.BaseBranch, leaseID, resumeAgentSessionID, resumeFromSeq, toolKeys, image, profile, genome)
	switch {
	case err != nil && profile != nil:
		// docs/adr/0064: an error can come after the Job exists (the call's
		// timeout, or an older replica dying mid-call), and an older replica's
		// Job is the ORDINARY pod. Unconfirmed is refused. The provisioner's
		// code passes through, so a vault refusal (docs/adr/0065: a missing
		// secret, by key name) reads as FailedPrecondition, not Internal.
		code := connect.CodeInternal
		if c := status.Code(err); c != codes.Unknown && c != codes.OK {
			code = connect.Code(c)
		}
		return "", s.refuseDefinitionPod(ctx, sessionID, code, fmt.Sprintf("definition pod create failed (%v)", err))
	case err != nil:
		return "", errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	case profile != nil && !pod.DefinitionProfileApplied:
		// The create reached a provisioner that dropped the profile (an older
		// one, or an older replica in a rollout overlap): the ORDINARY pod.
		return "", s.refuseDefinitionPod(ctx, sessionID, connect.CodeFailedPrecondition, "provisioner did not apply the definition pod profile")
	case profile != nil && !pod.VaultTokenApplied:
		// docs/adr/0065: a P1 provisioner built a pod holding real credentials.
		return "", s.refuseDefinitionPod(ctx, sessionID, connect.CodeFailedPrecondition, "provisioner did not mint a vault token")
	case profile != nil && s.packs != nil && !pod.GenomeApplied:
		// docs/adr/0063: a definition's packs are part of what it is.
		return "", s.refuseDefinitionPod(ctx, sessionID, connect.CodeFailedPrecondition, "provisioner did not apply the context packs")
	case s.packs != nil && !pod.GenomeApplied:
		// A repo session still works without them; the next warm on a
		// current provisioner gets them.
		slog.WarnContext(ctx, "dashboard warm: provisioner did not apply the context packs", "sessionId", sessionID)
	}
	s.recordRendered(ctx, sessionID, rendered, pod.GenomeApplied)
	// Record the build this pod is on. Not fatal: the pod exists and the
	// session is live either way, and failing the warm over a metadata write
	// would trade a working session for a cosmetic one.
	if err := s.sessions.SetPodImages(ctx, sessionID, pod.WorkerImage, pod.SidecarImage, pod.FleetVersion); err != nil {
		slog.WarnContext(ctx, "dashboard warm: recording pod images failed", "sessionId", sessionID, "error", err)
	}
	return pod.PodName, nil
}

// renderGenome renders the session's context packs (docs/adr/0063): a
// definition session's by its definition, a repo session's by its repo.
func (s *Server) renderGenome(ctx context.Context, t *sessions.Session) ([]*agentfleetv1.GenomeFile, *packs.Rendered, error) {
	if s.packs == nil {
		return nil, nil, nil
	}
	kind, name := packs.KindRepo, t.Repo
	if t.DefinitionName != nil {
		kind, name = packs.KindDefinition, *t.DefinitionName
	}
	r, err := s.packs.RenderContext(ctx, kind, name)
	if err != nil {
		return nil, nil, fmt.Errorf("render context packs: %w", err)
	}
	slog.InfoContext(ctx, "dashboard warm: context packs", "sessionId", t.ID, "kind", kind, "target", name, "versions", r.Versions, "skipped", r.Skipped)
	out := make([]*agentfleetv1.GenomeFile, 0, len(r.Files))
	for _, f := range r.Files {
		out = append(out, &agentfleetv1.GenomeFile{Path: f.Path, Body: f.Body})
	}
	return out, &r, nil
}

// recordRendered writes the warm's context_rendered signal (docs/adr/0063 §7):
// what core sent, beside the pod's own context_loaded report. A SYSTEM entry
// from "fleet", so it is never streamed to the agent (only human and session
// entries are) and the console shows it in the session feed. Not fatal.
func (s *Server) recordRendered(ctx context.Context, sessionID string, r *packs.Rendered, applied bool) {
	if r == nil {
		return
	}
	body, err := json.Marshal(map[string]any{
		"sdk": "context_rendered", "versions": nonNilStrings(r.Versions), "skipped": nonNilStrings(r.Skipped), "applied": applied,
	})
	if err == nil {
		_, err = s.transcr.Append(ctx, sessionID, "fleet", string(body), "system", "")
	}
	if err != nil {
		slog.WarnContext(ctx, "dashboard warm: recording the rendered context", "sessionId", sessionID, "error", err)
	}
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// refuseDefinitionPod revokes and removes whatever a definition warm may have
// built without a confirmed profile (docs/adr/0064): its lease first, then the
// pod. Tearing down nothing is a no-op. Off the caller's cancellation: a
// dropped browser request must not leave an ordinary pod holding a live lease.
func (s *Server) refuseDefinitionPod(ctx context.Context, sessionID string, code connect.Code, why string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	msg := why + " (docs/adr/0064); the pod was torn down"
	clearErr := s.sessions.ClearLease(ctx, sessionID)
	if clearErr != nil {
		slog.ErrorContext(ctx, "dashboard warm: revoking an unconfirmed definition pod's lease", "sessionId", sessionID, "error", clearErr)
	}
	if _, err := s.e2e.TearDownSession(ctx, sessionID, agentfleetv1.SessionKind_SESSION_KIND_WORKER); err != nil {
		// The phase stays live: CRASHED would hide a pod that may still run,
		// and reconcile bumps it back while the Job exists anyway. With the
		// lease cleared the pod can report no activity, so the startup-stall
		// sweep retries the teardown. With both failed nothing does: logged.
		slog.ErrorContext(ctx, "dashboard warm: tearing down an unconfirmed definition pod", "sessionId", sessionID, "error", err, "leaseRevoked", clearErr == nil)
		return errkeys.Connect(code, errkeys.KeyInternal, fmt.Errorf("%s (docs/adr/0064), and its teardown failed: %w", why, errors.Join(err, clearErr)))
	}
	// Gone: the phase gate revokes a lease ClearLease missed.
	if err := s.sessions.SetPodPhase(ctx, sessionID, agentfleetv1.PodPhase_POD_PHASE_CRASHED.String(), msg); err != nil {
		slog.ErrorContext(ctx, "dashboard warm: recording the refused definition pod", "sessionId", sessionID, "error", err)
	}
	return errkeys.Connect(code, errkeys.KeyInternal, errors.New(msg))
}

// definitionWarmable refuses to warm a definition session unless its pinned
// version is still the current one (docs/adr/0062's revocation rule), the repo
// has not been flipped to cluster access since creation. All checked BEFORE
// ReserveSlot, so a refusal leaves no lease and no phase. Whether the
// provisioner applied the definition pod profile (docs/adr/0064) is checked on
// the create's own answer in WarmIfIdle, the only answer that comes from the
// replica that built the pod. The profile is built from the current row, which by
// the pin check IS the pinned version.
func (s *Server) definitionWarmable(ctx context.Context, t *sessions.Session, repoCfg *repos.Repo) (*agentfleetv1.DefinitionPodProfile, error) {
	name, pinned := *t.DefinitionName, *t.DefinitionSHA
	if s.definitions == nil {
		return nil, errkeys.Connect(connect.CodeUnavailable, errkeys.KeyInternal, errors.New("defined agents are not configured"))
	}
	head, err := s.definitions.Resolve(name)
	switch {
	case errors.Is(err, definitions.ErrUnknown):
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, decisions.CannotResume(fmt.Errorf("definition %q is no longer defined; start a new session", name)))
	case errors.Is(err, definitions.ErrInvalid):
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, fmt.Errorf("definition %q has no valid version: %w", name, err))
	case err != nil:
		return nil, errkeys.Connect(connect.CodeUnavailable, errkeys.KeyInternal, err)
	}
	if head.SHA != pinned {
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, decisions.CannotResume(
			fmt.Errorf("definition %q changed since this session was created (pinned %.7s, current %.7s); start a new session", name, pinned, head.SHA)))

	}
	if repoCfg.ClusterAccess {
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal, errors.New("definition sessions never run on a cluster-access repo"))
	}
	eff := head.Effective
	if eff == nil || eff.Git == nil || eff.Git.Identity == nil || eff.Credentials == nil || eff.Limits == nil {
		// The validator requires all three on every launchable spec.
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, fmt.Errorf("definition %q has no git identity, credentials or limits", name))
	}
	// docs/adr/0065 §6: before a pod gets a push credential, the repo it clones
	// from and pushes to is the one its credentials name. Exact and
	// case-sensitive, like the vault's own prefix match.
	if r := definitions.GitHubRepo(eff.Credentials); r != "" && repoCfg.URL != "https://github.com/"+r && repoCfg.URL != "https://github.com/"+r+".git" {
		return nil, errkeys.Connect(connect.CodeFailedPrecondition, errkeys.KeyInternal,
			fmt.Errorf("repo %s url %q does not match definition %q's credentials (https://github.com/%s.git)", repoCfg.Name, repoCfg.URL, name, r))

	}
	timeout, err := time.ParseDuration(eff.Limits.Timeout)
	if err != nil {
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, fmt.Errorf("definition %q limits.timeout: %w", name, err))
	}
	id := eff.Git.Identity
	return &agentfleetv1.DefinitionPodProfile{
		Name: name, GitAuthorName: id.Name, GitAuthorEmail: id.Email,
		Vault: definitions.VaultSpec(eff.Credentials), TimeoutSeconds: int64(timeout.Seconds()),
	}, nil
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
		slog.ErrorContext(ctx, "dashboard DeleteTask: worker teardown", "sessionId", sessionID, "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	// A second teardown call for the session's e2e sandbox used to follow.
	// There is no sandbox (docs/adr/0048 §6) — one pod, one teardown.
	if err := s.sessions.Delete(ctx, sessionID); err != nil {
		slog.ErrorContext(ctx, "dashboard DeleteSession", "sessionId", sessionID, "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	slog.InfoContext(ctx, "dashboard DeleteTask", "sessionId", sessionID)
	return connect.NewResponse(&agentfleetv1.DeleteSessionResponse{}), nil
}

// ListWorktrees left-joins the provisioner's raw worktree list against
// `tasks` (reliability-findings.md #2) — core has no PVC access itself,
// so the worktree data comes from a passthrough call to the provisioner;
// task_status/task_error/pr_url are left unset (not an error) for a
// worktree whose task row no longer exists, exactly the orphaned case
// this view exists to surface. An inner join would hide it.

// GetJournal is the read path reliability-findings.md #1/#7 both call out
// as missing — knowledge_journal previously had no Get/List RPC anywhere.
func (s *Server) GetJournal(ctx context.Context, req *connect.Request[agentfleetv1.GetJournalRequest]) (*connect.Response[agentfleetv1.GetJournalResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = defaultListLimit
	}
	entries, err := s.journal.List(ctx, req.Msg.GetRepo(), req.Msg.GetSinceId(), limit)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard GetJournal", "repo", req.Msg.GetRepo(), "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	out := make([]*agentfleetv1.JournalEntry, len(entries))
	nextID := req.Msg.GetSinceId()
	for i, e := range entries {
		out[i] = journalEntryToProto(e)
		nextID = e.ID
	}
	return connect.NewResponse(&agentfleetv1.GetJournalResponse{Entries: out, NextId: nextID}), nil
}

// ListRepos backs the console's repo manager. Rows come from the GitHub sync
// and infra-bootstrap's overrides file (docs/adr/0074); inactive rows are
// listed too, since the console is where they are switched back on.

func (s *Server) ListRepos(ctx context.Context, _ *connect.Request[agentfleetv1.ListReposRequest]) (*connect.Response[agentfleetv1.ListReposResponse], error) {
	list, err := s.repos.List(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard ListRepos", "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	out := make([]*agentfleetv1.Repo, len(list))
	for i, r := range list {
		out[i] = repoToProto(r)
	}
	return connect.NewResponse(&agentfleetv1.ListReposResponse{Repos: out, Models: pickableModels()}), nil
}

// SyncRepo refreshes the provisioner's clone cache for one repo. Until this
// existed, CreateWorkerPod was the only thing that ever advanced that cache,
// so a newly added repo stayed uncloned until its first session paid for a
// cold clone and an idle repo's cache just went stale.
//
// The URL is read from the repo row and never taken from the request — the
// same rule WarmIfIdle follows, and the reason the request carries only a
// name.
//
// "Sync all" is the dashboard looping this call over the list it already
// holds. No batch RPC: a batch would have to invent a partial-failure shape
// for something the caller can attribute for free.
func (s *Server) SyncRepo(ctx context.Context, req *connect.Request[agentfleetv1.SyncRepoRequest]) (*connect.Response[agentfleetv1.SyncRepoResponse], error) {
	name := req.Msg.GetName()
	if name == "" {
		return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, errors.New("name is required"))
	}
	r, err := s.startableRepo(ctx, name)
	if err != nil {
		return nil, err
	}
	resp, err := s.e2e.SyncRepoCache(ctx, r.Name, r.URL)
	if err != nil {
		// err itself, not a generic replacement: git's own stderr is already
		// inside it (provisioner/internal/git.Manager.run), and that line —
		// "fatal: could not read Username", "Repository not found" — is the
		// entire diagnostic the operator gets. Safe to surface here: repo URLs
		// hold no credential (auth is gh's credential helper) and this surface
		// is OIDC admin-only.
		slog.ErrorContext(ctx, "dashboard SyncRepo", "name", name, "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	slog.InfoContext(ctx, "dashboard SyncRepo", "name", name, "cloned", resp.GetCloned(), "advanced", resp.GetCommitsAdvanced())
	return connect.NewResponse(&agentfleetv1.SyncRepoResponse{
		Cloned:          resp.GetCloned(),
		Head:            resp.GetHead(),
		CommitsAdvanced: resp.GetCommitsAdvanced(),
		HeadChanged:     resp.GetHeadChanged(),
		DurationMs:      resp.GetDurationMs(),
	}), nil
}

func repoToProto(r repos.Repo) *agentfleetv1.Repo {
	return &agentfleetv1.Repo{
		Name:          r.Name,
		Url:           r.URL,
		BaseBranch:    r.BaseBranch,
		Image:         r.Image,
		ClusterAccess: r.ClusterAccess,
		FullName:      r.FullName,
		Active:        r.Active,
		CreatedAt:     r.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// startableRepo is the one repo gate for starting work: every path that
// opens a new session (and SyncRepo) must go through it, so an inactive repo
// is refused everywhere at once. Unknown or gone is NotFound, inactive is
// FailedPrecondition. Existing sessions are not gated: WarmIfIdle never
// checks active.
func (s *Server) startableRepo(ctx context.Context, name string) (*repos.Repo, error) {
	r, err := s.repos.Get(ctx, name)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard repo lookup", "name", name, "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	// repos.Store.Get reports "no such repo" as (nil, nil), not
	// pgx.ErrNoRows. Checking for the sentinel here compiles, passes every
	// test with a repo that exists, and panics on the first typo'd name.
	if r == nil {
		return nil, errkeys.Connect(connect.CodeNotFound, errkeys.KeyInternal, fmt.Errorf("unknown repo %q", name))
	}
	if !r.Active {
		return nil, errInactive(name)
	}
	return r, nil
}

func errInactive(name string) error {
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("repo %s is inactive; enable it in Settings → Repos", name))
}

// SetReposActive is the console's one write on repos: whether the fleet uses
// a repo, as opposed to whether it can reach it (the GitHub installation).
func (s *Server) SetReposActive(ctx context.Context, req *connect.Request[agentfleetv1.SetReposActiveRequest]) (*connect.Response[agentfleetv1.SetReposActiveResponse], error) {
	names, active := req.Msg.GetNames(), req.Msg.GetActive()
	n, err := s.repos.SetActive(ctx, names, active)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard SetReposActive", "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	slog.InfoContext(ctx, "dashboard SetReposActive", "by", who(ctx), "active", active, "requested", len(names), "updated", n)
	return connect.NewResponse(&agentfleetv1.SetReposActiveResponse{Updated: int32(n)}), nil
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

// --- shared file space (docs/adr/0030) — core mints presigned URLs, the
// dashboard's browser moves the actual bytes directly against Garage ---

func (s *Server) ListFiles(ctx context.Context, _ *connect.Request[agentfleetv1.ListFilesRequest]) (*connect.Response[agentfleetv1.ListFilesResponse], error) {
	files, err := s.files.List(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "dashboard ListFiles", "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
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
		slog.ErrorContext(ctx, "dashboard GetFileUploadUrl", "filename", req.Msg.GetFilename(), "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.GetFileUploadUrlResponse{UploadUrl: url, Key: key, ExpiresAt: expiresAt.Format(time.RFC3339)}), nil
}

func (s *Server) GetFileDownloadUrl(ctx context.Context, req *connect.Request[agentfleetv1.GetFileDownloadUrlRequest]) (*connect.Response[agentfleetv1.GetFileDownloadUrlResponse], error) {
	url, expiresAt, err := s.files.PresignDownload(ctx, req.Msg.GetKey())
	if err != nil {
		slog.ErrorContext(ctx, "dashboard GetFileDownloadUrl", "key", req.Msg.GetKey(), "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.GetFileDownloadUrlResponse{DownloadUrl: url, ExpiresAt: expiresAt.Format(time.RFC3339)}), nil
}

func (s *Server) DeleteFile(ctx context.Context, req *connect.Request[agentfleetv1.DeleteFileRequest]) (*connect.Response[agentfleetv1.DeleteFileResponse], error) {
	if err := s.files.Delete(ctx, req.Msg.GetKey()); err != nil {
		slog.ErrorContext(ctx, "dashboard DeleteFile", "key", req.Msg.GetKey(), "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, err)
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
		Effort:         t.Effort,
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
		// Which build this session's pod is running. Optional on the wire, and
		// left unset rather than sent as "" so the console can tell "no pod has
		// ever run" from "a pod ran on an unnamed image".
		WorkerImage:    optional(t.WorkerImage),
		SidecarImage:   optional(t.SidecarImage),
		FleetVersion:   optional(t.FleetVersion),
		DefinitionName: t.DefinitionName,
		DefinitionSha:  t.DefinitionSHA,
	}
}

// optional turns a store column that is NOT NULL DEFAULT ” into the wire's
// optional string: empty means "never set", which the UI renders as nothing
// rather than as a blank field.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
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
	case "card":
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_CARD
	default:
		return agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_UNSPECIFIED
	}
}

// QueryLogs queries Loki for log entries from fleet components or deployed
// apps. Used by the dashboard's Logs tab to display logs for a specific task,
// component, or deployed application. Returns structured log entries with
// timestamp, level, message, and other fields.
func (s *Server) QueryLogs(ctx context.Context, req *connect.Request[agentfleetv1.QueryLogsRequest]) (*connect.Response[agentfleetv1.QueryLogsResponse], error) {
	// Time strings are RFC3339 and OPTIONAL. Empty means "the default window",
	// the same convention coreserver's ViewLogs uses — a malformed value is
	// still an error, an absent one is not. Requiring them made the only
	// caller (LogDrawer, which asks for "the newest 200 lines" and has no
	// range picker) fail with an invalid_argument on every open, so the log
	// drawer had never once returned a line.
	//
	// 24h rather than ViewLogs' 1h: this drawer is opened on a session that
	// already failed, usually a while after it did.
	start, end := time.Now().Add(-24*time.Hour), time.Now()
	if raw := req.Msg.GetStartTime(); raw != "" {
		var err error
		if start, err = time.Parse(time.RFC3339, raw); err != nil {
			slog.ErrorContext(ctx, "dashboard QueryLogs: invalid start_time", "start_time", raw, "error", err)
			return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("invalid start_time: %w", err))
		}
	}
	if raw := req.Msg.GetEndTime(); raw != "" {
		var err error
		if end, err = time.Parse(time.RFC3339, raw); err != nil {
			slog.ErrorContext(ctx, "dashboard QueryLogs: invalid end_time", "end_time", raw, "error", err)
			return nil, errkeys.Connect(connect.CodeInvalidArgument, errkeys.KeyInternal, fmt.Errorf("invalid end_time: %w", err))
		}
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
		slog.ErrorContext(ctx, "dashboard QueryLogs: Loki query failed", "error", err)
		return nil, errkeys.Connect(connect.CodeInternal, errkeys.KeyInternal, fmt.Errorf("query Loki: %w", err))
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

	slog.InfoContext(ctx, "dashboard QueryLogs", "component", req.Msg.GetComponent(), "count", len(entries))
	return connect.NewResponse(&agentfleetv1.QueryLogsResponse{
		Entries:    protoEntries,
		TotalCount: int32(len(entries)),
	}), nil
}

// ProtoType is stringToProtoType for AgentService's own Entry.
func ProtoType(s string) agentfleetv1.TranscriptEntryType { return stringToProtoType(s) }
