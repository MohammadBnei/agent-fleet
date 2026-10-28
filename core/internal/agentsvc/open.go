package agentsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

const (
	maxKey   = 512
	maxActor = 200
	// A client's open threads; a member's is maxOpenThreads.
	maxClientThreads = 500
)

// openSlots bounds concurrent Opens (each holds a pooled connection for its
// locks). pgxpool's default is at least 4 connections.
var openSlots = make(chan struct{}, 2)

func threadLock(owner, definition, threadKey string) string {
	return "thread\x00" + owner + "\x00" + definition + "\x00" + threadKey
}

// Open is a client's resume-or-create of a thread (docs/adr/0066 §6-7).
// The request claims its idempotency key before any warm; a retry with the
// same key re-runs only what did not finish, and a finished one returns its
// thread and starts nothing.
//
// ponytail: interaction deny and lifecycle single-shot are refused until the
// headless worker (ADR-0070) ships with Invoke; the worker reads both from
// the definition today, not from the thread.
func (s *Server) Open(ctx context.Context, req *connect.Request[agentfleetv1.OpenRequest]) (*connect.Response[agentfleetv1.OpenResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	return s.open(ctx, p, req.Msg, false)
}

// ErrNeedsWake is Open's FailedPrecondition for an event with no open thread
// that may not open one (docs/adr/0078 §5): the one case the dispatcher turns
// into a proposal. errors.Is sees it through the connect error.
var ErrNeedsWake = errors.New("no open thread for this key, and a new one needs wake")

// AsClient is the context an in-process client (no tokenRef, docs/adr/0066
// §3) calls AgentService with, the bridge's dispatcher. A client reachable
// over :9091 is refused: its rights belong to its bearer.
func (s *Server) AsClient(ctx context.Context, client string) (context.Context, error) {
	for _, c := range s.definitions.Clients() {
		if c.Name == client {
			if c.Spec.TokenRef != "" {
				return nil, fmt.Errorf("client %q is not in-process", client)
			}
			return withPrincipal(ctx, Principal{Client: client}), nil
		}
	}
	return nil, fmt.Errorf("client %q is not registered", client)
}

// OpenAsAdmin opens an event proposal (docs/adr/0078 §5): a platform admin's
// click is the human gate (docs/adr/0048 §1), so the thread opens through
// client's Open, waking, with the admin as the actor and no actors.allow
// check. The caller has checked the admin.
func (s *Server) OpenAsAdmin(ctx context.Context, client, adminSub string, m *agentfleetv1.OpenRequest) (*agentfleetv1.OpenResponse, error) {
	ctx, err := s.AsClient(ctx, client)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	p, _ := FromContext(ctx)
	m.Actor, m.Wake = "admin:"+adminSub, true
	res, err := s.open(ctx, p, m, true)
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func (s *Server) open(ctx context.Context, p Principal, m *agentfleetv1.OpenRequest, adminActor bool) (*connect.Response[agentfleetv1.OpenResponse], error) {
	switch {
	case m.GetThreadKey() == "" || len(m.GetThreadKey()) > maxKey:
		return nil, invalid("thread_key is required and at most %d bytes", maxKey)
	case m.GetIdempotencyKey() == "" || len(m.GetIdempotencyKey()) > maxKey:
		return nil, invalid("idempotency_key is required and at most %d bytes", maxKey)
	case m.GetMessage() == "" || len(m.GetMessage()) > maxText:
		return nil, invalid("message is required and at most %d bytes", maxText)
	case len(m.GetActor()) > maxActor || len(m.GetCausationId()) > maxKey:
		return nil, invalid("actor or causation_id is too long")
	case m.GetVisibility() != "shared" && m.GetVisibility() != "private":
		return nil, invalid(`visibility must be "shared" or "private"`)
	case m.GetInteraction() != "ask" || m.GetLifecycle() != "resident":
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(`Open takes interaction "ask" and lifecycle "resident" until headless invoke ships`))
	}
	head, ok := s.usable(p, m.GetDefinition())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, definitions.ErrUnknown)
	}
	if !allows(head.Effective.Interaction, m.GetInteraction()) || !allows(head.Effective.Lifecycle, m.GetLifecycle()) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this definition does not take that interaction or lifecycle"))
	}
	// A waking message needs an actor the client may claim (§6): exact,
	// fail closed, an empty actor never wakes.
	if m.GetWake() && !adminActor && !s.mayClaim(p.Client, m.GetActor()) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("actor is not in this client's actors.allow"))
	}

	// Each Open holds one pooled connection for its locks while it runs, and
	// its own queries need more: a bound keeps a burst from taking the whole
	// pool (docs/adr/0066 §7's "nothing ever blocks in the database").
	select {
	case openSlots <- struct{}{}:
		defer func() { <-openSlots }()
	default:
		return nil, connect.NewError(connect.CodeAborted, errors.New("too many requests in progress, retry"))
	}
	owner, key := p.owner(), m.GetIdempotencyKey()
	lock, err := s.sessions.TryLock(ctx, "req\x00"+owner+"\x00"+key, threadLock(owner, m.GetDefinition(), m.GetThreadKey()))
	if errors.Is(err, sessions.ErrBusy) {
		return nil, connect.NewError(connect.CodeAborted, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer lock.Release()

	// Every field that decides trust is part of the request's identity, so a
	// retry cannot quietly turn a tainted or non-waking message into another.
	sha := requestSHA(m.GetDefinition(), m.GetThreadKey(), m.GetMessage(), fmt.Sprint(m.GetWake()), fmt.Sprint(m.GetTainted()),
		m.GetActor(), m.GetCausationId(), m.GetVisibility(), m.GetInteraction(), m.GetLifecycle())
	claim, err := s.sessions.ClaimRequest(ctx, owner, key, sha)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Step 3 before step 2: a different request on a used key is refused,
	// never answered with the earlier request's result.
	if claim.RequestSHA != sha {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this idempotency_key was used for a different request"))
	}
	// Step 2: done is final, even when its session row was later deleted.
	if claim.Done {
		if claim.SessionID == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this request was delivered to a thread that no longer exists"))
		}
		seq, _, _ := s.sessions.FinishClaim(ctx, owner, key, claim.SessionID)
		return connect.NewResponse(&agentfleetv1.OpenResponse{ThreadId: claim.SessionID, Seq: seq}), nil
	}
	// Step 4: delivered by an earlier attempt that crashed before marking it.
	if claim.SessionID != "" {
		if seq, found, err := s.sessions.FinishClaim(ctx, owner, key, claim.SessionID); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		} else if found {
			return connect.NewResponse(&agentfleetv1.OpenResponse{ThreadId: claim.SessionID, Seq: seq}), nil
		}
	}

	t, predecessor, err := s.resolveThread(ctx, p, head, claim, m)
	if err != nil {
		return nil, err
	}
	fresh := t == nil
	if fresh {
		if !m.GetWake() {
			// The dispatcher files a proposal instead (ADR-0071 §6).
			return nil, connect.NewError(connect.CodeFailedPrecondition, ErrNeedsWake)
		}
		if n, err := s.sessions.OpenThreads(ctx, owner); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		} else if n >= maxClientThreads {
			return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("this client has %d open threads; close some first", n))
		}
		if t, err = s.dash.CreateThread(ctx, head, m.GetThreadKey(), sessions.CreateParams{
			Owner: owner, Visibility: m.GetVisibility(), Interaction: m.GetInteraction(), Lifecycle: m.GetLifecycle(),
			Invoker: p.Client, ThreadKey: m.GetThreadKey(),
		}); err != nil {
			return nil, err
		}
	}
	if err := s.sessions.PointClaim(ctx, owner, key, t.ID); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// A tainted follow-up on an existing thread never wakes (§6); only an
	// opening message can be tainted and wake. An admin opening a proposal
	// is the human gate itself (docs/adr/0078 §5), so it wakes either way,
	// still marked untrusted.
	contextOnly := !m.GetWake() || (m.GetTainted() && !fresh && !adminActor)
	// Untrusted is the entry's own trust, never whether it woke (docs/adr/0082
	// §3): a tainted cause, or an actor this client may not claim. A waking
	// message has passed mayClaim already; a non-waking one has not.
	untrusted := m.GetTainted() || (!adminActor && !s.mayClaim(p.Client, m.GetActor()))
	text := m.GetMessage()
	if predecessor != "" {
		text = fmt.Sprintf("[continues thread %s, which can no longer run]\n%s", predecessor, text)
	}
	if !contextOnly {
		if _, err := s.dash.WarmIfIdle(ctx, t.ID); err != nil {
			if fresh {
				// A thread this call created and could not start would hold
				// the key with nothing in it, and later events would land
				// in a session that never ran (docs/adr/0078 §5): archive
				// it, whatever refused the warm, and a retry creates the
				// thread again.
				if _, aerr := s.dash.ArchiveSession(ctx, connect.NewRequest(&agentfleetv1.ArchiveSessionRequest{SessionId: t.ID})); aerr != nil {
					slog.Warn("agentsvc Open: archive a thread the warm refused", "sessionId", t.ID, "error", aerr)
				}
			}
			return nil, err // the claim stays pending; a retry resumes here
		}
	}
	seq, err := s.sessions.AppendClaimed(ctx, owner, key, sessions.ClaimedEntry{
		SessionID: t.ID, Text: text, AuthorSub: owner, AuthorName: owner,
		Actor: m.GetActor(), CausationID: m.GetCausationId(),
		ContextOnly: contextOnly, Untrusted: untrusted,
	})
	if errors.Is(err, sessions.ErrArchived) {
		// Closed under us: the claim stays pending, and the retry resolves
		// the thread again.
		return nil, connect.NewError(connect.CodeAborted, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.OpenResponse{ThreadId: t.ID, Seq: seq}), nil
}

// resolveThread is step 4: the claim's session if still open, else the
// thread's open session. A waking Open replaces a thread that can no longer
// warm (§6) and returns the old id as predecessor. nil means create.
func (s *Server) resolveThread(ctx context.Context, p Principal, head definitions.Head, claim sessions.Claim, m *agentfleetv1.OpenRequest) (*sessions.Session, string, error) {
	var t *sessions.Session
	if claim.SessionID != "" {
		got, err := s.sessions.Get(ctx, claim.SessionID)
		if err != nil && !errors.Is(err, sessions.ErrNotFound) {
			return nil, "", connect.NewError(connect.CodeInternal, err)
		}
		if got != nil && got.ArchivedAt == nil {
			t = got
		}
	}
	if t == nil {
		got, err := s.sessions.OpenThread(ctx, p.Client, m.GetDefinition(), m.GetThreadKey())
		if err != nil {
			return nil, "", connect.NewError(connect.CodeInternal, err)
		}
		t = got
	}
	if t == nil {
		return nil, "", nil
	}
	if deref(t.Interaction, "ask") != m.GetInteraction() || deref(t.Lifecycle, "resident") != m.GetLifecycle() || deref(t.Visibility, "") != m.GetVisibility() {
		return nil, "", connect.NewError(connect.CodeFailedPrecondition, errors.New("this thread was opened with a different interaction, lifecycle or visibility"))
	}
	// Only a trusted waking message replaces a thread: a tainted one on an
	// existing thread is context-only (§6), and must neither end the thread
	// nor open a new one.
	if m.GetWake() && !m.GetTainted() && (deref(t.DefinitionSHA, "") != head.SHA || t.SweptAt != nil) {
		if _, err := s.dash.ArchiveSession(ctx, connect.NewRequest(&agentfleetv1.ArchiveSessionRequest{SessionId: t.ID})); err != nil {
			return nil, "", err
		}
		return nil, t.ID, nil
	}
	return t, "", nil
}

// CloseThread archives a client's open thread (the bridge, when a PR merges
// or an alert resolves). Archiving tears down its pod.
func (s *Server) CloseThread(ctx context.Context, req *connect.Request[agentfleetv1.CloseThreadRequest]) (*connect.Response[agentfleetv1.CloseThreadResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	lock, err := s.sessions.TryLock(ctx, threadLock(p.owner(), req.Msg.GetDefinition(), req.Msg.GetThreadKey()))
	if errors.Is(err, sessions.ErrBusy) {
		return nil, connect.NewError(connect.CodeAborted, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer lock.Release()
	t, err := s.sessions.OpenThread(ctx, p.Client, req.Msg.GetDefinition(), req.Msg.GetThreadKey())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if t == nil {
		return nil, errNoThread
	}
	if _, err := s.dash.ArchiveSession(ctx, connect.NewRequest(&agentfleetv1.ArchiveSessionRequest{SessionId: t.ID})); err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.CloseThreadResponse{}), nil
}

func (s *Server) mayClaim(client, actor string) bool {
	if actor == "" {
		return false
	}
	for _, c := range s.definitions.Clients() {
		if c.Name == client {
			return slices.Contains(c.Spec.Actors.Allow, actor)
		}
	}
	return false
}

// requestSHA is §7's request identity: the same key with another request is
// refused. NUL-separated, since a thread key may hold any other byte.
func requestSHA(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func invalid(format string, a ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, a...))
}
