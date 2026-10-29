package agentsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/grants"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// answerable loads a thread the caller may answer, with the request at seq:
// an entry of wantType in this thread's own transcript, which ExitPlanMode
// decides is a plan or a permission. A seq that is anything else is
// InvalidArgument; one already answered is AlreadyExists, from
// decisions.Answer before anything with a side effect (a warm, a mode
// switch).
func (s *Server) answerable(ctx context.Context, p Principal, threadID string, seq int64, wantType string, plan bool) (*sessions.Session, error) {
	t, _, err := s.thread(ctx, p, threadID)
	if err != nil {
		return nil, err
	}
	if err := s.mayAnswer(p, t); err != nil {
		return nil, err
	}
	entries, _, err := s.transcr.ReadSince(ctx, t.ID, seq, 1)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if len(entries) == 0 || entries[0].Seq != seq || entries[0].Type != wantType {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("seq %d is not a %s in this thread", seq, wantType))
	}
	if wantType == "permission_request" {
		var req struct {
			Tool string `json:"tool"`
		}
		_ = json.Unmarshal([]byte(entries[0].Text), &req)
		if (req.Tool == "ExitPlanMode") != plan {
			if plan {
				return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("seq %d is not a plan", seq))
			}
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("seq %d is a plan: use ApprovePlan", seq))
		}
	}
	return t, nil
}

// mayAnswer is canAnswer for a thread the caller can see, while its client
// is still registered, on a thread that takes answers.
func (s *Server) mayAnswer(p Principal, t *sessions.Session) error {
	if !canAnswer(p, t) || !s.stillRegistered(p, t) {
		return connect.NewError(connect.CodePermissionDenied, errors.New("you cannot answer in this thread"))
	}
	// docs/adr/0070: a headless thread has nobody to answer, and its worker
	// asks nothing.
	if deref(t.Interaction, "ask") == "deny" {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("this thread takes no answers"))
	}
	return nil
}

// answerAt is decisions.Answer for the decision t's request at seq opened.
func (s *Server) answerAt(ctx context.Context, p Principal, t *sessions.Session, seq int64, a decisions.Reply) (int64, error) {
	r, err := s.decisions.Request(ctx, t.ID, seq)
	if errors.Is(err, decisions.ErrNotFound) {
		return 0, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("seq %d is not an open request", seq))
	}
	if err != nil {
		return 0, connect.NewError(connect.CodeInternal, err)
	}
	return s.answer(ctx, p, r, a)
}

func (s *Server) answer(ctx context.Context, p Principal, r decisions.Record, a decisions.Reply) (int64, error) {
	a.By = p.author()
	n, err := s.decisions.Answer(ctx, s.dash, s.transcr, r, a)
	if err != nil {
		return 0, dashboard.AnswerErr("agentsvc answer", err)
	}
	return n, nil
}

func validMessage(m string) error {
	if len(m) > maxTitle*10 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("message is too long"))
	}
	return nil
}

func validAnswers(answersJSON string) error {
	var answers map[string]any
	if len(answersJSON) > maxText || json.Unmarshal([]byte(answersJSON), &answers) != nil || answers == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("answers_json must be a JSON object"))
	}
	return nil
}

// RespondToPermission is AnswerDecision for a permission request: {behavior,
// message} and nothing else (docs/adr/0066 §4), no updatedInput, no standing
// rule. No warm, as in the console: a permission belongs to a live pod.
func (s *Server) RespondToPermission(ctx context.Context, req *connect.Request[agentfleetv1.AgentServiceRespondToPermissionRequest]) (*connect.Response[agentfleetv1.AgentServiceRespondToPermissionResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Msg.GetBehavior()
	if b != "allow" && b != "deny" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(`behavior must be "allow" or "deny"`))
	}
	if err := validMessage(req.Msg.GetMessage()); err != nil {
		return nil, err
	}
	t, err := s.answerable(ctx, p, req.Msg.GetThreadId(), req.Msg.GetSeq(), "permission_request", false)
	if err != nil {
		return nil, err
	}
	n, err := s.answerAt(ctx, p, t, req.Msg.GetSeq(), decisions.Reply{Behavior: b, Message: req.Msg.GetMessage()})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.AgentServiceRespondToPermissionResponse{Seq: n}), nil
}

// AnswerQuestion is AnswerDecision for a question: it warms first, as the
// console does, since a question outlives its pod and its answer is
// delivered to the next one.
func (s *Server) AnswerQuestion(ctx context.Context, req *connect.Request[agentfleetv1.AgentServiceAnswerQuestionRequest]) (*connect.Response[agentfleetv1.AgentServiceAnswerQuestionResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := validAnswers(req.Msg.GetAnswersJson()); err != nil {
		return nil, err
	}
	t, err := s.answerable(ctx, p, req.Msg.GetThreadId(), req.Msg.GetSeq(), "question", false)
	if err != nil {
		return nil, err
	}
	n, err := s.answerAt(ctx, p, t, req.Msg.GetSeq(), decisions.Reply{Answers: req.Msg.GetAnswersJson()})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.AgentServiceAnswerQuestionResponse{Seq: n}), nil
}

// ApprovePlan is AnswerDecision allowing a plan: the mode first, within
// mode.allow, then the answer (the console's dashboard/src/approvePlan.ts).
// Unlike a repo session, a definition session's worker resolves nothing on a
// mode switch (worker/src/session.ts), so an existing answer is someone
// else's and AlreadyExists, never success.
func (s *Server) ApprovePlan(ctx context.Context, req *connect.Request[agentfleetv1.ApprovePlanRequest]) (*connect.Response[agentfleetv1.ApprovePlanResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	t, err := s.answerable(ctx, p, req.Msg.GetThreadId(), req.Msg.GetSeq(), "permission_request", true)
	if err != nil {
		return nil, err
	}
	if err := choosesMode(p, t, req.Msg.GetMode()); err != nil {
		return nil, err
	}
	n, err := s.answerAt(ctx, p, t, req.Msg.GetSeq(), decisions.Reply{Behavior: "allow", Mode: req.Msg.GetMode()})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.ApprovePlanResponse{Seq: n}), nil
}

// choosesMode: naming the mode a plan approval switches to is SetConfig's
// power, the owner's or an admin's. Approving without one switches nothing.
func choosesMode(p Principal, t *sessions.Session, mode string) error {
	if mode != "" && !ownsOrAdmin(p, t) {
		return connect.NewError(connect.CodePermissionDenied, errors.New("only the thread's owner changes its mode"))
	}
	return nil
}

// AnswerDecision answers any kind of decision by id (docs/adr/0082 §5). An
// admin in the console answers every decision it can see, console and repo
// sessions included; anyone else, on the threads canAnswer allows. A
// "definition" scope widens a definition for every member: admins only
// (docs/adr/0076 §3).
func (s *Server) AnswerDecision(ctx context.Context, req *connect.Request[agentfleetv1.AnswerDecisionRequest]) (*connect.Response[agentfleetv1.AnswerDecisionResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if uuid.Validate(m.GetDecisionId()) != nil {
		return nil, errNoDecision
	}
	r, err := s.decisions.Get(ctx, m.GetDecisionId())
	if errors.Is(err, decisions.ErrNotFound) || (err == nil && !s.visible(p, r)) {
		return nil, errNoDecision
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := validMessage(m.GetMessage()); err != nil {
		return nil, err
	}
	if r.Kind == decisions.Question {
		if err := validAnswers(m.GetAnswersJson()); err != nil {
			return nil, err
		}
	}
	if !p.Admin() {
		t, _, err := s.thread(ctx, p, r.SessionID)
		if err != nil {
			return nil, err
		}
		if err := s.mayAnswer(p, t); err != nil {
			return nil, err
		}
		if err := choosesMode(p, t, m.GetMode()); err != nil {
			return nil, err
		}
		if m.GetScope() == grants.ScopeDefinition {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only an admin remembers an approval for the whole definition"))
		}
	}
	n, err := s.answer(ctx, p, r, decisions.Reply{
		Behavior: m.GetBehavior(), Message: m.GetMessage(), Answers: m.GetAnswersJson(), Mode: m.GetMode(),
		Scope: m.GetScope(), RuleForm: m.GetRuleForm(),
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.AnswerDecisionResponse{Seq: n}), nil
}

// ownsOrAdmin is who configures a thread. A tainted thread is admins' only
// for configuration too: its mode decides what untrusted input may drive.
// stillRegistered: a client acts on its threads only while the definition
// and the client still list each other (docs/adr/0066 §5), so de-listing a
// client revokes it without a restart. Reading and stopping stay open.
func (s *Server) stillRegistered(p Principal, t *sessions.Session) bool {
	if p.Client == "" {
		return true
	}
	_, ok := s.definitions.MayOpen(p.Client, deref(t.DefinitionName, ""))
	return ok
}

func ownsOrAdmin(p Principal, t *sessions.Session) bool {
	return p.Admin() || (!t.Tainted && t.Owner != nil && *t.Owner == p.owner())
}

// setMode goes through the console's SetPermissionMode, which checks the
// pinned definition's mode.allow before the worker hears of it.
func (s *Server) setMode(ctx context.Context, t *sessions.Session, mode string) error {
	_, err := s.dash.SetPermissionMode(ctx, connect.NewRequest(&agentfleetv1.SetPermissionModeRequest{SessionId: t.ID, Mode: mode}))
	return err
}

// SetConfig is the owner's or an admin's (docs/adr/0067 §2). Mode only:
// the model is fixed per session until per-session model choice lands.
func (s *Server) SetConfig(ctx context.Context, req *connect.Request[agentfleetv1.SetConfigRequest]) (*connect.Response[agentfleetv1.SetConfigResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	t, _, err := s.thread(ctx, p, req.Msg.GetThreadId())
	if err != nil {
		return nil, err
	}
	if !ownsOrAdmin(p, t) || !s.stillRegistered(p, t) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only the thread's owner changes its config"))
	}
	if req.Msg.GetModel() != "" {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("changing a thread's model is not supported yet"))
	}
	if m := req.Msg.GetMode(); m != "" {
		if err := s.setMode(ctx, t, m); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(&agentfleetv1.SetConfigResponse{}), nil
}

// Stop is the owner's or an admin's, through the console's StopSession.
func (s *Server) Stop(ctx context.Context, req *connect.Request[agentfleetv1.StopRequest]) (*connect.Response[agentfleetv1.StopResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	t, _, err := s.thread(ctx, p, req.Msg.GetThreadId())
	if err != nil {
		return nil, err
	}
	// Not ownsOrAdmin: stopping a tainted thread only takes power away.
	if !p.Admin() && (t.Owner == nil || *t.Owner != p.owner()) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only the thread's owner stops it"))
	}
	reason := "stopped by " + p.author().Name
	if _, err := s.dash.StopSession(ctx, connect.NewRequest(&agentfleetv1.StopSessionRequest{SessionId: t.ID, Reason: &reason})); err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.StopResponse{}), nil
}

func (s *Server) GetStatus(ctx context.Context, req *connect.Request[agentfleetv1.GetStatusRequest]) (*connect.Response[agentfleetv1.GetStatusResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	t, _, err := s.thread(ctx, p, req.Msg.GetThreadId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&agentfleetv1.GetStatusResponse{Status: status(t)}), nil
}
