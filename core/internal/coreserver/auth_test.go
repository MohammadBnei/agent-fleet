package coreserver

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// fakeLeases accepts exactly one (session, lease) pair, the way the sessions
// table does: a lease is current for one session and nothing else.
type fakeLeases struct {
	session string
	lease   string
	err     error
}

func (f fakeLeases) VerifyLease(_ context.Context, sessionID, leaseID string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return sessionID == f.session && leaseID == f.lease, nil
}

func ctxWith(pairs ...string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...))
}

func callUnary(t *testing.T, a *Authenticator, ctx context.Context, method string, req any) error {
	t.Helper()
	_, err := a.UnaryInterceptor(ctx, req, &grpc.UnaryServerInfo{FullMethod: method}, func(context.Context, any) (any, error) {
		return nil, nil
	})
	return err
}

func codeOf(err error) codes.Code {
	return status.Code(err)
}

// The point of the whole change: before it, CoreService authenticated nobody.
func TestAuth_RejectsACallWithNoCredential(t *testing.T) {
	a := NewAuthenticator(fakeLeases{session: "s1", lease: "L1"}, "provtoken")

	err := callUnary(t, a, context.Background(),
		"/agentfleet.v1.CoreService/SendMessage",
		&agentfleetv1.SendMessageRequest{SessionId: "s1"})

	if codeOf(err) != codes.Unauthenticated {
		t.Errorf("got %v (%v), want Unauthenticated", err, codeOf(err))
	}
}

func TestAuth_RejectsAWrongLease(t *testing.T) {
	a := NewAuthenticator(fakeLeases{session: "s1", lease: "L1"}, "provtoken")

	err := callUnary(t, a, ctxWith(mdSessionID, "s1", mdLeaseID, "not-the-lease"),
		"/agentfleet.v1.CoreService/SendMessage",
		&agentfleetv1.SendMessageRequest{SessionId: "s1"})

	if codeOf(err) != codes.PermissionDenied {
		t.Errorf("got %v (%v), want PermissionDenied", err, codeOf(err))
	}
}

func TestAuth_AcceptsItsOwnSession(t *testing.T) {
	a := NewAuthenticator(fakeLeases{session: "s1", lease: "L1"}, "provtoken")

	if err := callUnary(t, a, ctxWith(mdSessionID, "s1", mdLeaseID, "L1"),
		"/agentfleet.v1.CoreService/SendMessage",
		&agentfleetv1.SendMessageRequest{SessionId: "s1"}); err != nil {
		t.Errorf("a session calling with its own valid lease was rejected: %v", err)
	}
}

// THE test. A pod holding a perfectly valid lease for its OWN session must not
// be able to act on a different one. This is the case a shared fleet-wide token
// would pass and #200's original hole allowed outright — the session id came
// from the request body and nothing checked it against the caller.
func TestAuth_RejectsAValidLeaseUsedForAnotherSession(t *testing.T) {
	a := NewAuthenticator(fakeLeases{session: "s1", lease: "L1"}, "provtoken")
	ownLease := ctxWith(mdSessionID, "s1", mdLeaseID, "L1")

	cases := []struct {
		method string
		req    any
	}{
		{"/agentfleet.v1.CoreService/SendMessage", &agentfleetv1.SendMessageRequest{SessionId: "victim"}},
		{"/agentfleet.v1.CoreService/SetPermissionMode", &agentfleetv1.SetPermissionModeRequest{SessionId: "victim"}},
		{"/agentfleet.v1.CoreService/UpdateSessionMeta", &agentfleetv1.UpdateSessionMetaRequest{SessionId: "victim"}},
		{"/agentfleet.v1.CoreService/GetSession", &agentfleetv1.GetSessionRequest{Id: "victim"}},
		{"/agentfleet.v1.CoreService/Expose", &agentfleetv1.ExposeRequest{SessionId: "victim"}},
		{"/agentfleet.v1.CoreService/WaitForMessages", &agentfleetv1.ReadTranscriptSinceRequest{SessionId: "victim"}},
	}
	for _, c := range cases {
		if err := callUnary(t, a, ownLease, c.method, c.req); codeOf(err) != codes.PermissionDenied {
			t.Errorf("%s: got %v (%v), want PermissionDenied", c.method, err, codeOf(err))
		}
	}
}

// PromptSessionRequest carries caller_session_id AND target_session_id, both
// strings. Binding the wrong one authorises precisely what this is meant to
// stop, and it would compile, lint and pass a naive test — the same shape as
// the session_id/agent_session_id swap already in this repo's trap list.
func TestAuth_PromptSessionBindsTheCallerNotTheTarget(t *testing.T) {
	a := NewAuthenticator(fakeLeases{session: "s1", lease: "L1"}, "provtoken")
	ownLease := ctxWith(mdSessionID, "s1", mdLeaseID, "L1")

	// Prompting a peer is the feature: caller is me, target is somebody else.
	if err := callUnary(t, a, ownLease, "/agentfleet.v1.CoreService/PromptSession",
		&agentfleetv1.PromptSessionRequest{CallerSessionId: "s1", TargetSessionId: "peer"}); err != nil {
		t.Errorf("prompting a peer with my own caller id was rejected: %v", err)
	}

	// Claiming to BE somebody else is not.
	if err := callUnary(t, a, ownLease, "/agentfleet.v1.CoreService/PromptSession",
		&agentfleetv1.PromptSessionRequest{CallerSessionId: "victim", TargetSessionId: "peer"}); codeOf(err) != codes.PermissionDenied {
		t.Errorf("impersonating another caller: got %v (%v), want PermissionDenied", err, codeOf(err))
	}
}

// SaveAgentSessionIdRequest also carries two same-typed ids. agent_session_id
// is the Agent SDK's own conversation id and is NOT a fleet session id, so
// binding it would reject every legitimate call while looking correct.
func TestAuth_SaveAgentSessionIdBindsTheFleetSessionNotTheSdkConversation(t *testing.T) {
	a := NewAuthenticator(fakeLeases{session: "s1", lease: "L1"}, "provtoken")

	if err := callUnary(t, a, ctxWith(mdSessionID, "s1", mdLeaseID, "L1"),
		"/agentfleet.v1.CoreService/SaveAgentSessionId",
		&agentfleetv1.SaveAgentSessionIdRequest{SessionId: "s1", AgentSessionId: "sdk-conversation-uuid"}); err != nil {
		t.Errorf("saving my own agent session id was rejected: %v", err)
	}
}

// ReportPodEvents names arbitrary sessions by nature — it is how core learns a
// pod exists — so it cannot be lease-bound, and a session must not be able to
// forge pod lifecycle events for another one.
func TestAuth_ReportPodEventsIsProvisionerOnly(t *testing.T) {
	a := NewAuthenticator(fakeLeases{session: "s1", lease: "L1"}, "provtoken")
	const method = "/agentfleet.v1.CoreService/ReportPodEvents"

	if err := callUnary(t, a, ctxWith(mdSessionID, "s1", mdLeaseID, "L1"), method, &agentfleetv1.PodEvent{SessionId: "victim"}); codeOf(err) != codes.PermissionDenied {
		t.Errorf("a session reached a provisioner-only method: got %v (%v)", err, codeOf(err))
	}
	if err := callUnary(t, a, ctxWith(mdProvisionerToken, "provtoken"), method, &agentfleetv1.PodEvent{SessionId: "anyone"}); err != nil {
		t.Errorf("the provisioner was rejected on its own method: %v", err)
	}
	if err := callUnary(t, a, ctxWith(mdProvisionerToken, "guessed"), method, &agentfleetv1.PodEvent{SessionId: "anyone"}); codeOf(err) != codes.PermissionDenied {
		t.Errorf("a wrong provisioner token was accepted: %v (%v)", err, codeOf(err))
	}
}

// An unset token must not turn into "anyone is the provisioner". This is the
// empty-string-is-not-an-error family that has bitten this repo before, and
// the failure would be silent and total.
func TestAuth_AnUnsetProvisionerTokenAuthenticatesNobody(t *testing.T) {
	a := NewAuthenticator(fakeLeases{session: "s1", lease: "L1"}, "")

	if err := callUnary(t, a, ctxWith(mdProvisionerToken, ""),
		"/agentfleet.v1.CoreService/ReportPodEvents", &agentfleetv1.PodEvent{SessionId: "x"}); codeOf(err) != codes.PermissionDenied {
		t.Errorf("empty token matched empty config: got %v (%v)", err, codeOf(err))
	}
	if err := callUnary(t, a, context.Background(),
		"/agentfleet.v1.CoreService/ReportPodEvents", &agentfleetv1.PodEvent{SessionId: "x"}); codeOf(err) != codes.PermissionDenied {
		t.Errorf("no token matched empty config: got %v (%v)", err, codeOf(err))
	}
}

// A database blip is not a rejected credential. Returning PermissionDenied
// there would be unretryable (it is not in either client's retry policy) and
// would silently strand a live session on a transient error.
func TestAuth_AStoreErrorIsUnavailableNotDenied(t *testing.T) {
	a := NewAuthenticator(fakeLeases{err: errors.New("connection refused")}, "provtoken")

	err := callUnary(t, a, ctxWith(mdSessionID, "s1", mdLeaseID, "L1"),
		"/agentfleet.v1.CoreService/SendMessage", &agentfleetv1.SendMessageRequest{SessionId: "s1"})

	if codeOf(err) != codes.Unavailable {
		t.Errorf("got %v (%v), want Unavailable", err, codeOf(err))
	}
}

// Every method of the service must appear in exactly one authorization table.
// Adding an RPC and forgetting this is the "wire it into EVERY path" trap;
// here the consequence is that the method either fails closed in production
// (an outage) or, if the default were flipped, ships unauthorised. This makes
// it a build-time conversation instead.
func TestAuth_EveryMethodHasAnAuthorizationRule(t *testing.T) {
	svc := agentfleetv1.File_agentfleet_v1_core_proto.Services().ByName("CoreService")
	if svc == nil {
		t.Fatal("CoreService not found in the proto descriptor")
	}
	for i := 0; i < svc.Methods().Len(); i++ {
		full := "/agentfleet.v1.CoreService/" + string(svc.Methods().Get(i).Name())
		n := 0
		if _, ok := callerSessionOf[full]; ok {
			n++
		}
		if unboundMethods[full] {
			n++
		}
		if provisionerMethods[full] {
			n++
		}
		if n != 1 {
			t.Errorf("%s appears in %d authorization tables, want exactly 1 — decide what it may reach", full, n)
		}
	}
}

// The stream chain is not optional. core's gRPC server had only a unary chain,
// so a unary-only check leaves StreamHumanMessages — the live feed of
// everything a human types to a session, permission answers included — wide
// open, with every test still green.
func TestAuth_StreamInterceptorBindsTheRequestToTheCaller(t *testing.T) {
	a := NewAuthenticator(fakeLeases{session: "s1", lease: "L1"}, "provtoken")
	const method = "/agentfleet.v1.CoreService/StreamHumanMessages"

	run := func(ctx context.Context, want string) error {
		return a.StreamInterceptor(nil, fakeStream{ctx: ctx, msg: &agentfleetv1.StreamHumanMessagesRequest{SessionId: want}},
			&grpc.StreamServerInfo{FullMethod: method},
			func(_ any, ss grpc.ServerStream) error {
				return ss.RecvMsg(&agentfleetv1.StreamHumanMessagesRequest{})
			})
	}

	if err := run(context.Background(), "s1"); codeOf(err) != codes.Unauthenticated {
		t.Errorf("uncredentialed stream: got %v (%v), want Unauthenticated", err, codeOf(err))
	}
	if err := run(ctxWith(mdSessionID, "s1", mdLeaseID, "L1"), "victim"); codeOf(err) != codes.PermissionDenied {
		t.Errorf("stream onto another session: got %v (%v), want PermissionDenied", err, codeOf(err))
	}
	if err := run(ctxWith(mdSessionID, "s1", mdLeaseID, "L1"), "s1"); err != nil {
		t.Errorf("stream onto my own session was rejected: %v", err)
	}
}

type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
	msg *agentfleetv1.StreamHumanMessagesRequest
}

func (f fakeStream) Context() context.Context { return f.ctx }

func (f fakeStream) RecvMsg(m any) error {
	if got, ok := m.(*agentfleetv1.StreamHumanMessagesRequest); ok {
		got.SessionId = f.msg.GetSessionId()
	}
	return nil
}
