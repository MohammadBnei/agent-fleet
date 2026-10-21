package agentsvc

import (
	"context"
	"errors"
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
// and a tainted thread is admins' only.
func canAnswer(p Principal, t *sessions.Session) bool {
	if p.Admin() {
		return true
	}
	if t.Tainted {
		return false
	}
	if t.Owner != nil && *t.Owner == p.owner() {
		return true
	}
	return p.Member != nil && t.Visibility != nil && *t.Visibility == "shared"
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
			if h, err := s.definitions.Resolve(l.Name); p.Admin() || (err == nil && p.Member != nil && mayUse(p, h.Effective)) || p.Client != "" {
				names = append(names, l.Name)
			}
		}
	}
	out := &agentfleetv1.ListThreadsResponse{}
	if len(names) == 0 {
		return connect.NewResponse(out), nil
	}
	list, err := s.sessions.ListThreads(ctx, names, p.owner(), p.Admin())
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
	vis := req.Msg.GetVisibility()
	if vis != "shared" && vis != "private" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(`visibility must be "shared" or "private"`))
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
	if req.Msg.GetText() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("text is required"))
	}
	t, h, err := s.thread(ctx, p, req.Msg.GetThreadId())
	if err != nil {
		return nil, err
	}
	if !canAnswer(p, t) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("you cannot post to this thread"))
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

func entryToProto(e transcript.Entry) *agentfleetv1.Entry {
	return &agentfleetv1.Entry{
		Seq: e.Seq, Type: dashboard.ProtoType(e.Type), From: e.From, Text: e.Text, ReplyTo: e.ReplyTo,
		CreatedAt: transcript.RFC3339OrEmpty(e.CreatedAt), AuthorName: e.AuthorName,
		ContextOnly: e.ContextOnly, Untrusted: e.Untrusted,
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
		out.Entries = append(out.Entries, entryToProto(e))
	}
	return connect.NewResponse(out), nil
}

// streamMaxAge matches the console's: the client reconnects from its cursor.
var streamMaxAge = 90 * time.Second

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
	wake, cancel := s.hub.Subscribe(t.ID, cursor)
	defer cancel()
	deadline := time.NewTimer(streamMaxAge)
	defer deadline.Stop()
	for {
		entries, next, err := s.transcr.ReadSince(ctx, t.ID, cursor, 100)
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		for _, e := range entries {
			if err := stream.Send(&agentfleetv1.AgentServiceStreamTranscriptResponse{Entry: entryToProto(e)}); err != nil {
				return err
			}
		}
		cursor = next
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
