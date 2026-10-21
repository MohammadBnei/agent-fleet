package agentsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// answerable loads a thread the caller may answer, with the request at seq:
// an entry of wantType in this thread's own transcript, which ExitPlanMode
// decides is a plan or a permission. A seq that is anything else is
// InvalidArgument; one already answered is AlreadyExists at the append
// (migration 000015's one-reply index).
func (s *Server) answerable(ctx context.Context, p Principal, threadID string, seq int64, wantType string, plan bool) (*sessions.Session, error) {
	t, _, err := s.thread(ctx, p, threadID)
	if err != nil {
		return nil, err
	}
	if !canAnswer(p, t) || !s.stillRegistered(p, t) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("you cannot answer in this thread"))
	}
	// docs/adr/0070: a headless thread has nobody to answer, and its worker
	// asks nothing.
	if deref(t.Interaction, "ask") == "deny" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this thread takes no answers"))
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
	// Checked before anything with a side effect (a warm, a mode switch):
	// the one-reply index still decides a race at the append.
	answered, err := s.sessions.Answered(ctx, t.ID, seq)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if answered {
		return nil, connect.NewError(connect.CodeAlreadyExists, transcript.ErrAlreadyAnswered)
	}
	return t, nil
}

func replyErr(err error) error {
	if errors.Is(err, transcript.ErrAlreadyAnswered) {
		return connect.NewError(connect.CodeAlreadyExists, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

// RespondToPermission appends {behavior, message} and nothing else
// (docs/adr/0066 §4): no updatedInput, no standing rule. Remembered grants
// stay a console action (docs/adr/0076 §3) until the member rule for them
// lands. No warm, as in the console: a permission belongs to a live pod.
func (s *Server) RespondToPermission(ctx context.Context, req *connect.Request[agentfleetv1.AgentServiceRespondToPermissionRequest]) (*connect.Response[agentfleetv1.AgentServiceRespondToPermissionResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Msg.GetBehavior()
	if b != "allow" && b != "deny" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(`behavior must be "allow" or "deny"`))
	}
	if len(req.Msg.GetMessage()) > maxTitle*10 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("message is too long"))
	}
	t, err := s.answerable(ctx, p, req.Msg.GetThreadId(), req.Msg.GetSeq(), "permission_request", false)
	if err != nil {
		return nil, err
	}
	decision, _ := json.Marshal(struct {
		Behavior string `json:"behavior"`
		Message  string `json:"message,omitempty"`
	}{b, req.Msg.GetMessage()})
	seq := req.Msg.GetSeq()
	n, err := s.transcr.AppendAuthored(ctx, t.ID, p.author(), string(decision), "permission_response", uuid.NewString(), &seq)
	if err != nil {
		return nil, replyErr(err)
	}
	return connect.NewResponse(&agentfleetv1.AgentServiceRespondToPermissionResponse{Seq: n}), nil
}

// AnswerQuestion warms first, as the console does: a question outlives its
// pod, and its answer is delivered to the next one.
func (s *Server) AnswerQuestion(ctx context.Context, req *connect.Request[agentfleetv1.AgentServiceAnswerQuestionRequest]) (*connect.Response[agentfleetv1.AgentServiceAnswerQuestionResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	var answers map[string]any
	if len(req.Msg.GetAnswersJson()) > maxText || json.Unmarshal([]byte(req.Msg.GetAnswersJson()), &answers) != nil || answers == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("answers_json must be a JSON object"))
	}
	t, err := s.answerable(ctx, p, req.Msg.GetThreadId(), req.Msg.GetSeq(), "question", false)
	if err != nil {
		return nil, err
	}
	if _, err := s.dash.WarmIfIdle(ctx, t.ID); err != nil {
		return nil, err
	}
	seq := req.Msg.GetSeq()
	n, err := s.transcr.AppendAuthored(ctx, t.ID, p.author(), req.Msg.GetAnswersJson(), "answer", uuid.NewString(), &seq)
	if err != nil {
		return nil, replyErr(err)
	}
	return connect.NewResponse(&agentfleetv1.AgentServiceAnswerQuestionResponse{Seq: n}), nil
}

// ApprovePlan is the console's approvePlan (dashboard/src/approvePlan.ts):
// switch the mode first, within mode.allow, then answer the plan. Unlike a
// repo session, a definition session's worker resolves nothing on a mode
// switch (worker/src/session.ts), so an existing answer is someone else's
// and AlreadyExists, never success.
func (s *Server) ApprovePlan(ctx context.Context, req *connect.Request[agentfleetv1.ApprovePlanRequest]) (*connect.Response[agentfleetv1.ApprovePlanResponse], error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	t, err := s.answerable(ctx, p, req.Msg.GetThreadId(), req.Msg.GetSeq(), "permission_request", true)
	if err != nil {
		return nil, err
	}
	mode := req.Msg.GetMode()
	if mode != "" {
		// Switching mode is SetConfig's power: the owner's or an admin's.
		if !ownsOrAdmin(p, t) {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only the thread's owner changes its mode"))
		}
		if err := s.setMode(ctx, t, mode); err != nil {
			return nil, err
		}
	}
	seq := req.Msg.GetSeq()
	n, err := s.transcr.AppendAuthored(ctx, t.ID, p.author(), `{"behavior":"allow"}`, "permission_response", uuid.NewString(), &seq)
	if err != nil {
		return nil, replyErr(err)
	}
	return connect.NewResponse(&agentfleetv1.ApprovePlanResponse{Seq: n}), nil
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
