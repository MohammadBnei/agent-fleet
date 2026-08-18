// Package dashboard implements the web dashboard's ConnectRPC API
// (agentfleet.v1.DashboardService, see docs/adr/0015) on top of the exact
// same sessions.Store / transcript.Store / provisionerclient.Client that core's
// Discord commands already use — no new business logic, just a second
// caller of the same store methods.
package dashboard

import (
	"context"
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

	"github.com/MohammadBnei/agent-fleet/core/internal/filestore"
	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/lokiclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/promclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/promptsnippets"
	"github.com/MohammadBnei/agent-fleet/core/internal/proposals"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/schedules"
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
	maxLive   int
	loki      lokiclient.Querier
	prom      promclient.Querier
	schedules *schedules.Store
	// version is core's own build, stamped in via -ldflags (cmd/core/main.go).
	// It covers the dashboard SPA too — that is compiled into this binary.
	version string
}

func NewServer(sessionStore *sessions.Store, proposalStore *proposals.Store, transcr transcript.Store, journalStore *journal.Store, repoStore *repos.Store, snippetStore *promptsnippets.Store, e2e *provisionerclient.Client, files filestore.Store, hub *Hub, maxLive int, loki lokiclient.Querier, prom promclient.Querier, scheduleStore *schedules.Store, version string) *Server {
	return &Server{sessions: sessionStore, proposals: proposalStore, transcr: transcr, journal: journalStore, repos: repoStore, snippets: snippetStore, e2e: e2e, files: files, hub: hub, maxLive: maxLive, loki: loki, prom: prom, schedules: scheduleStore, version: version}
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
	// ErrNotFound first: Get signals a missing row with that error, not with a
	// nil session, so checking nil alone would map every unknown id to a 500.
	// The nil check stays as a belt-and-braces guard against a nil-with-no-error
	// return, which would otherwise panic one line down.
	if errors.Is(err, sessions.ErrNotFound) || (err == nil && t == nil) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("session not found"))
	}
	if err != nil {
		slog.Error("dashboard GetSession", "sessionId", req.Msg.GetId(), "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
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
			model = defaultModel
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
// that is what re-arms the dedup key so a still-firing alert or the next schedule
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

	// Title for both the label fields: the body is not a label, it is the
	// instruction, and it goes out as the first message below. Putting it in
	// `description` made the list render an entire alert payload as a row.
	id, err := s.sessions.Create(ctx, p.Repo, p.Title, p.Title, "")
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
		slog.Error("dashboard OpenFromProposal: warm failed", "sessionId", id, "proposalId", p.ID, "error", err)
		s.rollbackOpen(ctx, p.ID, id)
		return nil, err
	}
	if _, err := s.transcr.Append(ctx, id, "human", p.Body, "discussion", uuid.NewString()); err != nil {
		slog.Error("dashboard OpenFromProposal: append failed", "sessionId", id, "proposalId", p.ID, "error", err)
		s.rollbackOpen(ctx, p.ID, id)
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	slog.Info("dashboard OpenFromProposal", "proposalId", p.ID, "sessionId", id, "repo", p.Repo)
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
		slog.Warn("dashboard OpenFromProposal: rollback teardown failed", "sessionId", sessionID, "error", err)
	}
	if err := s.sessions.Delete(ctx, sessionID); err != nil {
		slog.Warn("dashboard OpenFromProposal: rollback delete failed", "sessionId", sessionID, "error", err)
	}
	if err := s.proposals.Unclaim(ctx, proposalID, sessionID); err != nil {
		slog.Warn("dashboard OpenFromProposal: rollback unclaim failed", "proposalId", proposalID, "error", err)
	}
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

// defaultModel is what a session runs when neither the request nor CLAUDE_MODEL
// names one. Kept in step with worker/src/session.ts's own fallback — the two
// are independent defaults on either side of the pod boundary, and a session
// created here carries its model in the row, so a disagreement shows up only
// for sessions created before this default existed.
const defaultModel = "claude-opus-5"

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
			slog.Error("dashboard GetTranscript latestSeq", "sessionId", sessionID, "error", err)
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	since, page := transcriptWindow(req.Msg.GetSinceSeq(), latest, req.Msg.GetLimit(), req.Msg.BeforeSeq)
	entries, next, err := s.transcr.ReadSince(ctx, sessionID, since, page)
	if err != nil {
		slog.Error("dashboard GetTranscript", "sessionId", sessionID, "error", err)
		return nil, connect.NewError(connect.CodeInternal, err)
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
	// Both from repoCfg, which is the row already read above. This used to
	// re-fetch the same repo through a toolKeysFor helper — two reads of one
	// row, which a concurrent "manage repos" edit could have straddled, so a
	// pod could get one repo's privilege grant and another's image.
	toolKeys := provisionerclient.ToolKeysFor(repoCfg.ClusterAccess)
	pod, err := s.e2e.CreateWorkerPod(ctx, sessionID, t.Repo, repoCfg.URL, repoCfg.BaseBranch, t.Description, leaseID, resumeAgentSessionID, resumeFromSeq, toolKeys, repoCfg.Image)
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	// Record the build this pod is on. Not fatal: the pod exists and the
	// session is live either way, and failing the warm over a metadata write
	// would trade a working session for a cosmetic one.
	if err := s.sessions.SetPodImages(ctx, sessionID, pod.WorkerImage, pod.SidecarImage); err != nil {
		slog.Warn("dashboard warm: recording pod images failed", "sessionId", sessionID, "error", err)
	}
	return pod.PodName, nil
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
	// A second teardown call for the session's e2e sandbox used to follow.
	// There is no sandbox (docs/adr/0048 §6) — one pod, one teardown.
	if err := s.sessions.Delete(ctx, sessionID); err != nil {
		slog.Error("dashboard DeleteSession", "sessionId", sessionID, "error", err)
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
		// Which build this session's pod is running. Optional on the wire, and
		// left unset rather than sent as "" so the console can tell "no pod has
		// ever run" from "a pod ran on an unnamed image".
		WorkerImage:  optional(t.WorkerImage),
		SidecarImage: optional(t.SidecarImage),
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
			slog.Error("dashboard QueryLogs: invalid start_time", "start_time", raw, "error", err)
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid start_time: %w", err))
		}
	}
	if raw := req.Msg.GetEndTime(); raw != "" {
		var err error
		if end, err = time.Parse(time.RFC3339, raw); err != nil {
			slog.Error("dashboard QueryLogs: invalid end_time", "end_time", raw, "error", err)
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid end_time: %w", err))
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
