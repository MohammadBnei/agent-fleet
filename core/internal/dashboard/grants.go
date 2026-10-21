package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/grants"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// Remembered approvals (docs/adr/0076 §3). Every DashboardService caller is
// a platform-admin today, so every caller may grant at definition scope; the
// member principal's AgentService enforces the narrower rule.

func (s *Server) SetGrants(g *grants.Store) { s.grants = g }

func grantErr(op string, err error) error {
	switch {
	case errors.Is(err, grants.ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, grants.ErrNotFound), errors.Is(err, sessions.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	}
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce
	}
	slog.Error("dashboard "+op, "error", err)
	return connect.NewError(connect.CodeInternal, err)
}

// requestRules renders the rules for a definition session's permission_request
// at seq. It never reads an answer.
func (s *Server) requestRules(ctx context.Context, sessionID string, seq int64) (grants.Rules, *sessions.Session, error) {
	if s.grants == nil {
		return grants.Rules{}, nil, connect.NewError(connect.CodeUnavailable, errors.New("grants are not configured"))
	}
	t, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		return grants.Rules{}, nil, err
	}
	if t.DefinitionName == nil {
		return grants.Rules{}, nil, fmt.Errorf("%w: only definition sessions remember approvals", grants.ErrInvalid)
	}
	entries, _, err := s.transcr.ReadSince(ctx, sessionID, seq, 1)
	if err != nil {
		return grants.Rules{}, nil, err
	}
	if len(entries) == 0 || entries[0].Seq != seq || entries[0].Type != "permission_request" {
		return grants.Rules{}, nil, fmt.Errorf("%w: seq %d is not a permission request", grants.ErrInvalid, seq)
	}
	r, err := grants.Render(entries[0].Text)
	return r, t, err
}

func (s *Server) PreviewGrant(ctx context.Context, req *connect.Request[agentfleetv1.PreviewGrantRequest]) (*connect.Response[agentfleetv1.PreviewGrantResponse], error) {
	r, _, err := s.requestRules(ctx, req.Msg.GetSessionId(), req.Msg.GetSeq())
	if err != nil {
		return nil, grantErr("PreviewGrant", err)
	}
	return connect.NewResponse(&agentfleetv1.PreviewGrantResponse{Exact: r.Exact, Prefix: r.Prefix}), nil
}

// rememberGrant stores the rule a remembered allow names and returns the
// decision the worker receives: {behavior, message} as the answerer sent it,
// plus core's own "grant". Stored before the answer is appended, so a grant
// the worker applies is never one the store lacks.
func (s *Server) rememberGrant(ctx context.Context, sessionID string, seq int64, decisionJSON, scope, form string) (string, error) {
	var d struct {
		Behavior string `json:"behavior"`
		Message  string `json:"message,omitempty"`
	}
	if err := json.Unmarshal([]byte(decisionJSON), &d); err != nil || d.Behavior != "allow" {
		return "", fmt.Errorf("%w: only an allow is remembered", grants.ErrInvalid)
	}
	r, t, err := s.requestRules(ctx, sessionID, seq)
	if err != nil {
		return "", err
	}
	rule, err := r.Pick(form)
	if err != nil {
		return "", err
	}
	if err := s.grants.Add(ctx, sessionID, *t.DefinitionName, scope, rule, who(ctx)); err != nil {
		return "", err
	}
	b, err := json.Marshal(struct {
		Behavior string `json:"behavior"`
		Message  string `json:"message,omitempty"`
		Grant    string `json:"grant"`
	}{d.Behavior, d.Message, rule})
	return string(b), err
}

func (s *Server) ListGrants(ctx context.Context, _ *connect.Request[agentfleetv1.ListGrantsRequest]) (*connect.Response[agentfleetv1.ListGrantsResponse], error) {
	if s.grants == nil {
		return connect.NewResponse(&agentfleetv1.ListGrantsResponse{}), nil
	}
	list, err := s.grants.List(ctx)
	if err != nil {
		return nil, grantErr("ListGrants", err)
	}
	out := &agentfleetv1.ListGrantsResponse{}
	for _, g := range list {
		out.Grants = append(out.Grants, &agentfleetv1.DefinitionGrant{
			Id: g.ID, Definition: g.Definition, Rule: g.Rule, GrantedBy: g.GrantedBy, GrantedAt: g.GrantedAt.UTC().Format(time.RFC3339),
		})
	}
	return connect.NewResponse(out), nil
}

func (s *Server) RevokeGrant(ctx context.Context, req *connect.Request[agentfleetv1.RevokeGrantRequest]) (*connect.Response[agentfleetv1.RevokeGrantResponse], error) {
	if s.grants == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("grants are not configured"))
	}
	if err := s.grants.Revoke(ctx, req.Msg.GetId(), who(ctx)); err != nil {
		return nil, grantErr("RevokeGrant", err)
	}
	return connect.NewResponse(&agentfleetv1.RevokeGrantResponse{}), nil
}
