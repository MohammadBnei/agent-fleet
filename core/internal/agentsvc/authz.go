// Package agentsvc is AgentService (docs/adr/0066): the API members and
// registered clients drive definition sessions through.
package agentsvc

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"
)

// Principal is who is calling, set by the listener's interceptor (docs/adr/0066
// §1): a member on :8080, a client on :9091. Exactly one of Member and Client
// is set.
type Principal struct {
	Member *auth.Identity
	Client string
}

// Admin is a member in platform-admins on this request's cookie.
func (p Principal) Admin() bool { return p.Member != nil && auth.AdminOnly(*p.Member) }

type principalKey struct{}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext is the request's principal. Every handler refuses without one.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

var errNoPrincipal = errors.New("no caller identity")

// access is one method's row in the table (docs/adr/0066 §4): which
// principals may call it at all. Per-definition and per-session decisions
// are the handler's.
type access struct{ member, client bool }

// table fails closed: a method missing from it is refused, and
// TestAuthzTable_CoversEveryMethod fails when the proto gains one.
var table = map[string]access{
	agentfleetv1connect.AgentServiceListDefinitionsProcedure:     {member: true, client: true},
	agentfleetv1connect.AgentServiceListThreadsProcedure:         {member: true, client: true},
	agentfleetv1connect.AgentServiceCreateThreadProcedure:        {member: true},
	agentfleetv1connect.AgentServicePostMessageProcedure:         {member: true},
	agentfleetv1connect.AgentServiceGetTranscriptProcedure:       {member: true, client: true},
	agentfleetv1connect.AgentServiceStreamTranscriptProcedure:    {member: true, client: true},
	agentfleetv1connect.AgentServiceRespondToPermissionProcedure: {member: true, client: true},
	agentfleetv1connect.AgentServiceAnswerQuestionProcedure:      {member: true, client: true},
	agentfleetv1connect.AgentServiceApprovePlanProcedure:         {member: true, client: true},
	agentfleetv1connect.AgentServiceSetConfigProcedure:           {member: true, client: true},
	agentfleetv1connect.AgentServiceStopProcedure:                {member: true, client: true},
	agentfleetv1connect.AgentServiceGetStatusProcedure:           {member: true, client: true},
	agentfleetv1connect.AgentServiceOpenProcedure:                {client: true},
	agentfleetv1connect.AgentServiceCloseThreadProcedure:         {client: true},
	agentfleetv1connect.AgentServiceInvokeProcedure:              {client: true},
	agentfleetv1connect.AgentServiceWaitForResultProcedure:       {client: true},
}

func allowed(procedure string, p Principal) bool {
	a, ok := table[procedure]
	if !ok {
		return false
	}
	return (p.Member != nil && a.member) || (p.Client != "" && a.client)
}

// MemberInterceptor is the :8080 listener's: the principal is the cookie
// identity auth's ConnectInterceptor established, and a cookie without a
// stable sub is refused so the widget logs in again (docs/adr/0066 §2).
// It must run after that interceptor.
func MemberInterceptor() connect.Interceptor { return memberAuth{} }

type memberAuth struct{}

func (memberAuth) principal(ctx context.Context, procedure string) (context.Context, error) {
	id, ok := auth.FromContext(ctx)
	if !ok || id.Sub == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errNoPrincipal)
	}
	p := Principal{Member: &id}
	if !allowed(procedure, p) {
		slog.Warn("agentsvc: refused member call", "email", id.Email, "procedure", procedure)
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("not available to members"))
	}
	return withPrincipal(ctx, p), nil
}

func (m memberAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		ctx, err := m.principal(ctx, req.Spec().Procedure)
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (memberAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (m memberAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := m.principal(ctx, conn.Spec().Procedure)
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}
