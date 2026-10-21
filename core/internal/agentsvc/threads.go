package agentsvc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// Bounds on what a member can write: open threads per owner (CreateThread
// provisions nothing, so no warm-time cap counts them), a title, and one
// message.
const (
	maxOpenThreads = 50
	maxTitle       = 200
	maxText        = 64 << 10
)

// One answer for "not yours" and "does not exist" (docs/adr/0066 §4), so
// private threads cannot be enumerated.
var errNoThread = connect.NewError(connect.CodeNotFound, errors.New("no such thread"))

// owner is the principal's owner key: the member's sub, or client:<name>.
func (p Principal) owner() string {
	if p.Member != nil {
		return p.Member.Sub
	}
	return "client:" + p.Client
}

// thread loads a definition session the caller may see (docs/adr/0067 §2)
// and its definition's current head. A head that does not resolve is
// returned as the zero Head: admins still see the thread, members do not.
func (s *Server) thread(ctx context.Context, p Principal, id string) (*sessions.Session, definitions.Head, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, definitions.Head{}, errNoThread
	}
	t, err := s.sessions.Get(ctx, id)
	if errors.Is(err, sessions.ErrNotFound) || (err == nil && (t.DefinitionName == nil || t.ArchivedAt != nil)) {
		return nil, definitions.Head{}, errNoThread
	}
	if err != nil {
		return nil, definitions.Head{}, connect.NewError(connect.CodeInternal, err)
	}
	one := []sessions.Session{*t}
	if err := s.sessions.Tainted(ctx, one); err != nil {
		return nil, definitions.Head{}, connect.NewError(connect.CodeInternal, err)
	}
	t = &one[0]
	h, _ := s.definitions.Resolve(*t.DefinitionName)
	if !canSee(p, t, h) {
		return nil, definitions.Head{}, errNoThread
	}
	return t, h, nil
}

func canSee(p Principal, t *sessions.Session, h definitions.Head) bool {
	if p.Admin() {
		return true
	}
	if t.Owner == nil {
		return false // console sessions are admin-only
	}
	if p.Client != "" {
		return *t.Owner == p.owner()
	}
	if h.Effective == nil || !mayUse(p, h.Effective) {
		return false
	}
	return *t.Owner == p.owner() || (t.Visibility != nil && *t.Visibility == "shared")
}

// canAnswer is who may post into or answer a thread they can see
// (docs/adr/0067 §2): never another member's or a client's private thread,
// a tainted thread is admins' only, and a locked one stewards' only (§6).
func canAnswer(p Principal, t *sessions.Session) bool {
	if p.Admin() {
		return true
	}
	if t.LockedBy != nil && !p.steward() {
		return false
	}
	if t.Tainted {
		return false
	}
	if t.Owner != nil && *t.Owner == p.owner() {
		return true
	}
	return p.Member != nil && t.Visibility != nil && *t.Visibility == "shared"
}

// wakeWindow is how long after a thread's last activity a member may still
// wake its pod (docs/adr/0067 §6). Past it, a steward wakes it first.
const wakeWindow = 24 * time.Hour

var errWake = connect.NewError(connect.CodeFailedPrecondition, errors.New("This thread has been idle for over 24 hours; an admin must wake it."))

// needsWake: a member who is not a steward would boot a pod for a thread idle
// past wakeWindow. A live pod costs nothing more to post to; clients keep
// their own rules (docs/adr/0066).
func needsWake(p Principal, t *sessions.Session, now time.Time) bool {
	if p.Member == nil || p.steward() || sessions.IsPodPhaseLive(t.PodPhase) {
		return false
	}
	last := t.CreatedAt
	if t.LastActiveAt != nil {
		last = *t.LastActiveAt
	}
	return now.Sub(last) > wakeWindow
}

func deref(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

func timeOrEmpty(t *time.Time) string {
	if t == nil {
		return ""
	}
	return transcript.RFC3339OrEmpty(*t)
}

func threadToProto(p Principal, t *sessions.Session) *agentfleetv1.Thread {
	return &agentfleetv1.Thread{
		Id:           t.ID,
		Definition:   deref(t.DefinitionName, ""),
		Title:        deref(t.Title, ""),
		Visibility:   deref(t.Visibility, "private"),
		Mine:         t.Owner != nil && *t.Owner == p.owner(),
		Interaction:  deref(t.Interaction, "ask"),
		Lifecycle:    deref(t.Lifecycle, "resident"),
		Model:        deref(t.Model, ""),
		Mode:         deref(t.PermissionMode, ""),
		Tainted:      t.Tainted,
		CreatedAt:    transcript.RFC3339OrEmpty(t.CreatedAt),
		LastActiveAt: timeOrEmpty(t.LastActiveAt),
		Status:       status(t),
		LockedBy:     deref(t.LockedBy, ""),
	}
}

func (s *Server) ListThreads(ctx context.Context, req *connect.Request[agentfleetv1.ListThreadsRequest]) (*connect.Response[agentfleetv1.ListThreadsResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	if s.definitions != nil {
		listed, _ := s.definitions.List()
		for _, l := range listed {
			if req.Msg.GetDefinition() != "" && l.Name != req.Msg.GetDefinition() {
				continue
			}
			if _, ok := s.usable(p, l.Name); ok || p.Admin() {
				names = append(names, l.Name)
			}
		}
	}
	out := &agentfleetv1.ListThreadsResponse{Steward: p.steward()}
	if len(names) == 0 {
		return connect.NewResponse(out), nil
	}
	// A client sees only its own threads, so other owners' shared rows never
	// take its slots in the query's limit.
	list, err := s.sessions.ListThreads(ctx, names, p.owner(), p.Member != nil, p.Admin())
	if err == nil {
		err = s.sessions.Tainted(ctx, list)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	for i := range list {
		// The store's filter is "own or shared"; canSee adds the audience
		// and client rules on the current head.
		h, _ := s.definitions.Resolve(*list[i].DefinitionName)
		if canSee(p, &list[i], h) {
			out.Threads = append(out.Threads, threadToProto(p, &list[i]))
		}
	}
	return connect.NewResponse(out), nil
}

// CreateThread provisions nothing (docs/adr/0048 §1): the first PostMessage
// warms. A member's thread is interaction ask and lifecycle resident
// (docs/adr/0067 §1).
func (s *Server) CreateThread(ctx context.Context, req *connect.Request[agentfleetv1.CreateThreadRequest]) (*connect.Response[agentfleetv1.CreateThreadResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	// docs/adr/0067 §6: members join threads an admin started; they start none.
	if p.Member != nil && !p.steward() {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only an admin starts a thread"))
	}
	vis := req.Msg.GetVisibility()
	if vis != "shared" && vis != "private" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(`visibility must be "shared" or "private"`))
	}
	if len(req.Msg.GetTitle()) > maxTitle {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("title is over %d bytes", maxTitle))
	}
	// Creating provisions nothing, so the warm-time caps never see a member
	// who only creates; this bounds the rows instead.
	if !p.Admin() {
		n, err := s.sessions.OpenThreads(ctx, p.owner())
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if n >= maxOpenThreads {
			return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("you have %d open threads; close one first", n))
		}
	}
	h, err := s.definitions.Resolve(req.Msg.GetDefinition())
	switch {
	case errors.Is(err, definitions.ErrUnknown):
		return nil, connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, definitions.ErrInvalid):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case err != nil:
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	if !mayUse(p, h.Effective) {
		return nil, connect.NewError(connect.CodeNotFound, definitions.ErrUnknown)
	}
	if !allows(h.Effective.Interaction, "ask") || !allows(h.Effective.Lifecycle, "resident") {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this definition takes no interactive threads"))
	}
	t, err := s.dash.CreateThread(ctx, h, req.Msg.GetTitle(), sessions.CreateParams{
		Owner: p.owner(), Visibility: vis, Interaction: "ask", Lifecycle: "resident",
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.CreateThreadResponse{Thread: threadToProto(p, t)}), nil
}

// allows: a definition without the list takes the default.
func allows(l *definitions.AllowList, v string) bool { return l == nil || slices.Contains(l.Allow, v) }

// PostMessage keeps the console's warm-then-append with a server-minted key
// (docs/adr/0066 §7). A thread whose definition changed is not moved: the
// member decides to continue in a new one (§6).
func (s *Server) PostMessage(ctx context.Context, req *connect.Request[agentfleetv1.AgentServicePostMessageRequest]) (*connect.Response[agentfleetv1.AgentServicePostMessageResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if req.Msg.GetText() == "" || len(req.Msg.GetText()) > maxText {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("text is required and at most %d bytes", maxText))
	}
	t, h, err := s.thread(ctx, p, req.Msg.GetThreadId())
	if err != nil {
		return nil, err
	}
	if !canAnswer(p, t) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("you cannot post to this thread"))
	}
	if needsWake(p, t, time.Now()) {
		return nil, errWake
	}
	if h.SHA == "" || deref(t.DefinitionSHA, "") != h.SHA {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("DEFINITION_CHANGED: start a new thread"))
	}
	if _, err := s.dash.WarmIfIdle(ctx, t.ID); err != nil {
		return nil, err
	}
	seq, err := s.transcr.AppendAuthored(ctx, t.ID, p.author(), req.Msg.GetText(), "discussion", uuid.NewString(), nil)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.AgentServicePostMessageResponse{Seq: seq}), nil
}

func (p Principal) author() transcript.Author {
	if p.Member != nil {
		return transcript.Author{Sub: p.Member.Sub, Name: p.Member.Email}
	}
	return transcript.Author{Sub: p.owner(), Name: p.owner()}
}

func entryToProto(p Principal, e transcript.Entry) *agentfleetv1.Entry {
	return &agentfleetv1.Entry{
		Seq: e.Seq, Type: dashboard.ProtoType(e.Type), From: e.From, Text: e.Text, ReplyTo: e.ReplyTo,
		CreatedAt: transcript.RFC3339OrEmpty(e.CreatedAt), AuthorName: e.AuthorName,
		ContextOnly: e.ContextOnly, Untrusted: e.Untrusted,
		Mine: e.AuthorSub != "" && e.AuthorSub == p.author().Sub,
	}
}

func (s *Server) GetTranscript(ctx context.Context, req *connect.Request[agentfleetv1.AgentServiceGetTranscriptRequest]) (*connect.Response[agentfleetv1.AgentServiceGetTranscriptResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	t, _, err := s.thread(ctx, p, req.Msg.GetThreadId())
	if err != nil {
		return nil, err
	}
	limit := int(req.Msg.GetLimit())
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	entries, next, err := s.transcr.ReadSince(ctx, t.ID, req.Msg.GetSinceSeq(), limit)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := &agentfleetv1.AgentServiceGetTranscriptResponse{NextSeq: next}
	for _, e := range entries {
		out.Entries = append(out.Entries, entryToProto(p, e))
	}
	return connect.NewResponse(out), nil
}

// streamMaxAge matches the console's: the client reconnects from its cursor.
// Visibility is checked at the start of each stream, so a revocation takes
// effect within it.
var streamMaxAge = 90 * time.Second

const streamPage = 100

// StreamTranscript wakes on the console's hub and reads the store itself,
// because the hub's entries are DashboardService's shape and carry no author.
func (s *Server) StreamTranscript(ctx context.Context, req *connect.Request[agentfleetv1.AgentServiceStreamTranscriptRequest], stream *connect.ServerStream[agentfleetv1.AgentServiceStreamTranscriptResponse]) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	t, _, err := s.thread(ctx, p, req.Msg.GetThreadId())
	if err != nil {
		return err
	}
	cursor := req.Msg.GetSinceSeq()
	// The hub's cursor is shared by every subscriber of a session and seeded
	// by the first, so it gets the store's head, never the caller's since:
	// a member must not be able to stop everyone else's live view.
	head, err := s.transcr.LatestSeq(ctx, t.ID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	wake, cancel := s.hub.Subscribe(t.ID, head)
	defer cancel()
	deadline := time.NewTimer(streamMaxAge)
	defer deadline.Stop()
	for {
		entries, next, err := s.transcr.ReadSince(ctx, t.ID, cursor, streamPage)
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		for _, e := range entries {
			if err := stream.Send(&agentfleetv1.AgentServiceStreamTranscriptResponse{Entry: entryToProto(p, e)}); err != nil {
				return err
			}
		}
		cursor = next
		// A full page means more is waiting: read again before sleeping.
		if len(entries) == streamPage {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return nil
		case _, ok := <-wake:
			if !ok {
				return nil
			}
		}
	}
}
