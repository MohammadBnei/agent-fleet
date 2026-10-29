package agentsvc

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// The decision deck's reads (docs/adr/0082 §5). A caller sees a decision
// exactly when it may read its thread's transcript: canSee, on a definition
// session that is not archived, as thread() decides.

var errNoDecision = connect.NewError(connect.CodeNotFound, errors.New("no such decision"))

var kindToProto = map[decisions.Kind]agentfleetv1.DecisionKind{
	decisions.Question:   agentfleetv1.DecisionKind_DECISION_KIND_QUESTION,
	decisions.Permission: agentfleetv1.DecisionKind_DECISION_KIND_PERMISSION,
	decisions.Plan:       agentfleetv1.DecisionKind_DECISION_KIND_PLAN,
}

var stateToProto = map[string]agentfleetv1.DecisionState{
	decisions.Open:      agentfleetv1.DecisionState_DECISION_STATE_OPEN,
	decisions.Answered:  agentfleetv1.DecisionState_DECISION_STATE_ANSWERED,
	decisions.Withdrawn: agentfleetv1.DecisionState_DECISION_STATE_WITHDRAWN,
	decisions.Expired:   agentfleetv1.DecisionState_DECISION_STATE_EXPIRED,
	decisions.Stale:     agentfleetv1.DecisionState_DECISION_STATE_STALE,
}

func (s *Server) visible(p Principal, r decisions.Record) bool {
	if r.Definition == nil || r.Archived {
		return false
	}
	h, _ := s.definitions.Resolve(*r.Definition)
	return canSee(p, &sessions.Session{Owner: r.Owner, Visibility: r.Visibility, DefinitionName: r.Definition}, h)
}

func decisionToProto(r decisions.Record) *agentfleetv1.Decision {
	return &agentfleetv1.Decision{
		Id: r.ID, Kind: kindToProto[r.Kind], State: stateToProto[r.State], ThreadId: r.SessionID,
		RequestSeq: r.RequestSeq, Ask: r.Ask, PayloadJson: r.Payload, Tool: r.Tool,
		AnswerJson: r.Answer, AnsweredBy: r.AnsweredBy, AnsweredAt: timeOrEmpty(r.AnsweredAt),
		CreatedAt: transcript.RFC3339OrEmpty(r.CreatedAt), Version: r.Version, SubjectRepo: r.SubjectRepo,
	}
}

const (
	defaultDecisionPage = 50
	maxDecisionPage     = 200
)

// A page token is the last row's created_at in microseconds and its id.
func pageToken(r decisions.Record) string {
	return strconv.FormatInt(r.CreatedAt.UnixMicro(), 10) + "_" + r.ID
}

func parsePageToken(tok string) (*decisions.Cursor, error) {
	if tok == "" {
		return nil, nil
	}
	us, id, ok := strings.Cut(tok, "_")
	n, err := strconv.ParseInt(us, 10, 64)
	if !ok || err != nil || uuid.Validate(id) != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("page_token is not one this service issued"))
	}
	return &decisions.Cursor{CreatedAt: time.UnixMicro(n), ID: id}, nil
}

// ListDecisions filters in the store as ListThreads does, then by canSee per
// row, so a page can come back short while more follow.
func (s *Server) ListDecisions(ctx context.Context, req *connect.Request[agentfleetv1.ListDecisionsRequest]) (*connect.Response[agentfleetv1.ListDecisionsResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	size := int(m.GetPageSize())
	if size <= 0 {
		size = defaultDecisionPage
	}
	size = min(size, maxDecisionPage)
	before, err := parsePageToken(m.GetPageToken())
	if err != nil {
		return nil, err
	}
	f := decisions.Filter{
		SubjectRepo: m.GetSubjectRepo(), SessionID: m.GetThreadId(),
		Owner: p.owner(), Shared: p.Member != nil, Admin: p.Admin(),
		Before: before, Limit: size + 1,
	}
	for _, st := range m.GetStates() {
		for k, v := range stateToProto {
			if v == st {
				f.States = append(f.States, k)
			}
		}
	}
	for _, kd := range m.GetKinds() {
		for k, v := range kindToProto {
			if v == kd {
				f.Kinds = append(f.Kinds, k)
			}
		}
	}
	// A filter naming only unknown values matches nothing, not everything.
	if len(f.States) < len(m.GetStates()) || len(f.Kinds) < len(m.GetKinds()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unknown decision state or kind"))
	}
	rows, err := s.decisions.List(ctx, f)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := &agentfleetv1.ListDecisionsResponse{}
	if len(rows) > size {
		rows = rows[:size]
		out.NextPageToken = pageToken(rows[size-1])
	}
	for _, r := range rows {
		if s.visible(p, r) {
			out.Decisions = append(out.Decisions, decisionToProto(r))
		}
	}
	return connect.NewResponse(out), nil
}

// GetDecision answers NotFound for "not yours" and "does not exist" alike,
// as thread() does.
func (s *Server) GetDecision(ctx context.Context, req *connect.Request[agentfleetv1.GetDecisionRequest]) (*connect.Response[agentfleetv1.GetDecisionResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if uuid.Validate(req.Msg.GetId()) != nil {
		return nil, errNoDecision
	}
	r, err := s.decisions.Get(ctx, req.Msg.GetId())
	if errors.Is(err, decisions.ErrNotFound) || (err == nil && !s.visible(p, r)) {
		return nil, errNoDecision
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&agentfleetv1.GetDecisionResponse{Decision: decisionToProto(r)}), nil
}

// WatchDecisions polls the store and sends the changes the caller may see.
// Visibility is judged per change, so a revocation takes effect at once; the
// stream ends at streamMaxAge, like StreamTranscript's.
func (s *Server) WatchDecisions(ctx context.Context, req *connect.Request[agentfleetv1.WatchDecisionsRequest], stream *connect.ServerStream[agentfleetv1.WatchDecisionsResponse]) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, streamMaxAge)
	defer cancel()
	err = s.decisions.Watch(ctx, req.Msg.GetSinceVersion(), func(version int64, changed []decisions.Record) error {
		msg := &agentfleetv1.WatchDecisionsResponse{Version: version}
		for _, r := range changed {
			if s.visible(p, r) {
				msg.ChangedIds = append(msg.ChangedIds, r.ID)
			}
		}
		// The baseline always goes out; a change nobody here may see does not.
		if changed != nil && len(msg.ChangedIds) == 0 {
			return nil
		}
		return stream.Send(msg)
	})
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	return nil
}
