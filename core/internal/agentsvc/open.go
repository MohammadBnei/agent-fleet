package agentsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

const (
	maxKey   = 512
	maxActor = 200
)

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
	m := req.Msg
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
	if m.GetWake() && !s.mayClaim(p.Client, m.GetActor()) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("actor is not in this client's actors.allow"))
	}

	owner, key := p.owner(), m.GetIdempotencyKey()
	lock, err := s.sessions.TryLock(ctx, "req\x00"+owner+"\x00"+key, "thread\x00"+owner+"\x00"+m.GetDefinition()+"\x00"+m.GetThreadKey())
	if errors.Is(err, sessions.ErrBusy) {
		return nil, connect.NewError(connect.CodeAborted, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer lock.Release()

	claim, err := s.sessions.ClaimRequest(ctx, owner, key, requestSHA(m.GetDefinition(), m.GetThreadKey(), m.GetMessage()))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if claim.RequestSHA != requestSHA(m.GetDefinition(), m.GetThreadKey(), m.GetMessage()) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this idempotency_key was used for a different request"))
	}
	// Steps 2 and 4: delivered before, by this or an earlier attempt.
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
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no open thread for this key, and a new one needs wake"))
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
	// opening message can be tainted and wake.
	contextOnly := !m.GetWake() || (m.GetTainted() && !fresh)
	text := m.GetMessage()
	if predecessor != "" {
		text = fmt.Sprintf("[continues thread %s, which can no longer run]\n%s", predecessor, text)
	}
	if !contextOnly {
		if _, err := s.dash.WarmIfIdle(ctx, t.ID); err != nil {
			return nil, err // the claim stays pending; a retry resumes here
		}
	}
	seq, err := s.sessions.AppendClaimed(ctx, owner, key, sessions.ClaimedEntry{
		SessionID: t.ID, Text: text, AuthorSub: owner, AuthorName: owner,
		Actor: m.GetActor(), CausationID: m.GetCausationId(),
		ContextOnly: contextOnly, Untrusted: contextOnly || m.GetTainted(),
	})
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
	if m.GetWake() && (deref(t.DefinitionSHA, "") != head.SHA || t.SweptAt != nil) {
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
