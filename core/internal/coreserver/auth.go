package coreserver

import (
	"context"
	"crypto/subtle"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// CoreService listened on 9090 with no authentication of any kind: any pod in
// the namespace could call it, and every method that names a session took that
// name from the request body on trust. A worker pod could therefore act as any
// other session — read its human messages, answer its permission prompts,
// rewrite its metadata.
//
// The credential is the session's own lease_id. It already existed for a
// different purpose (sessions.ReserveSlot mints gen_random_uuid() on every
// warm, and SaveAgentSessionID already refuses a stale one), which makes it
// strictly better than anything minted for this: it is random rather than
// derived from a session id that gets published in Discord links, it is
// per-pod rather than per-session-forever, it rotates on every warm, it lives
// only in the one component that holds the database, and it needs no key
// distributed to the provisioner.
const (
	mdSessionID        = "x-fleet-session-id"
	mdLeaseID          = "x-fleet-lease-id"
	mdProvisionerToken = "x-fleet-provisioner-token"
)

// LeaseVerifier is the one thing the interceptors need from the session store.
type LeaseVerifier interface {
	VerifyLease(ctx context.Context, sessionID, leaseID string) (bool, error)
}

// callerSessionOf reports which field of a request identifies the CALLER's own
// session, per method. It is an explicit table on purpose.
//
// A generic "find the field called session_id" would be wrong on at least
// three methods here, and wrong in the direction that grants authority:
//
//   - PromptSessionRequest carries caller_session_id AND target_session_id.
//     Both are strings. Binding the target one authorises exactly the thing
//     this is meant to prevent.
//   - SaveAgentSessionIdRequest carries session_id AND agent_session_id. The
//     repo has already shipped a bug from confusing that pair once
//     (docs/adr/0048's rename; see TestSaveAgentSessionId_StoresTheAgentIdNot-
//     TheSessionId) — the SDK's conversation id is not a fleet session id.
//   - GetSessionRequest calls it `id`, so a name-based rule would silently not
//     bind it at all and leave it open.
//
// A method absent from this map is authenticated but unbound: the caller must
// still present a valid lease, but the request names no session of its own.
// That is a real decision per method, listed in unboundMethods below, not a
// default anything falls into by being forgotten — unknownMethodPolicy makes a
// new method fail closed.
var callerSessionOf = map[string]func(any) string{
	"/agentfleet.v1.CoreService/SendMessage": func(r any) string {
		return r.(*agentfleetv1.SendMessageRequest).GetSessionId()
	},
	"/agentfleet.v1.CoreService/WaitForMessages": func(r any) string {
		return r.(*agentfleetv1.ReadTranscriptSinceRequest).GetSessionId()
	},
	"/agentfleet.v1.CoreService/AskUserQuestion": func(r any) string {
		return r.(*agentfleetv1.AskUserQuestionRequest).GetSessionId()
	},
	"/agentfleet.v1.CoreService/Expose": func(r any) string {
		return r.(*agentfleetv1.ExposeRequest).GetSessionId()
	},
	"/agentfleet.v1.CoreService/Unexpose": func(r any) string {
		return r.(*agentfleetv1.UnexposeRequest).GetSessionId()
	},
	"/agentfleet.v1.CoreService/RequestService": func(r any) string {
		return r.(*agentfleetv1.RequestServiceRequest).GetSessionId()
	},
	"/agentfleet.v1.CoreService/GetSession": func(r any) string {
		return r.(*agentfleetv1.GetSessionRequest).GetId() // `id`, not `session_id`
	},
	"/agentfleet.v1.CoreService/SetPermissionMode": func(r any) string {
		return r.(*agentfleetv1.SetPermissionModeRequest).GetSessionId()
	},
	"/agentfleet.v1.CoreService/UpdateSessionMeta": func(r any) string {
		return r.(*agentfleetv1.UpdateSessionMetaRequest).GetSessionId()
	},
	"/agentfleet.v1.CoreService/SaveAgentSessionId": func(r any) string {
		return r.(*agentfleetv1.SaveAgentSessionIdRequest).GetSessionId() // NOT GetAgentSessionId
	},
	"/agentfleet.v1.CoreService/PushToolTelemetry": func(r any) string {
		return r.(*agentfleetv1.PushToolTelemetryRequest).GetSessionId()
	},
	"/agentfleet.v1.CoreService/StreamHumanMessages": func(r any) string {
		return r.(*agentfleetv1.StreamHumanMessagesRequest).GetSessionId()
	},
	"/agentfleet.v1.CoreService/ListPeerSessions": func(r any) string {
		return r.(*agentfleetv1.ListPeerSessionsRequest).GetCallerSessionId()
	},
	"/agentfleet.v1.CoreService/PromptSession": func(r any) string {
		return r.(*agentfleetv1.PromptSessionRequest).GetCallerSessionId() // NOT GetTargetSessionId
	},
}

// unboundMethods name no session belonging to the caller, so a lease proves
// only that the caller is *a* live session. Each is a decision:
//
//   - WaitForSessionState names someone else's session by design (the peer
//     feature) and carries no caller field at all. Reaching it now requires a
//     live lease, where before it required nothing.
//   - AppendJournal/SearchJournal are repo-scoped, and ViewLogs is
//     component-scoped: fleet-wide reads, deliberately. Narrowing them is a
//     product decision about what a peer session may see, not part of closing
//     an authentication hole, so it is left alone and written down here rather
//     than quietly widened or quietly broken.
//   - The file methods key on an opaque object key rather than a session.
var unboundMethods = map[string]bool{
	"/agentfleet.v1.CoreService/WaitForSessionState": true,
	"/agentfleet.v1.CoreService/AppendJournal":       true,
	"/agentfleet.v1.CoreService/SearchJournal":       true,
	"/agentfleet.v1.CoreService/ViewLogs":            true,
	"/agentfleet.v1.CoreService/ListFiles":           true,
	"/agentfleet.v1.CoreService/GetFileUploadUrl":    true,
	"/agentfleet.v1.CoreService/GetFileDownloadUrl":  true,
	"/agentfleet.v1.CoreService/DeleteFile":          true,
}

// provisionerMethods are callable only by the provisioner. ReportPodEvents
// reports on arbitrary sessions by its nature — it is how core learns a pod
// exists — so it cannot be lease-bound, and a session must not be able to
// forge pod lifecycle events for another one.
var provisionerMethods = map[string]bool{
	"/agentfleet.v1.CoreService/ReportPodEvents": true,
}

var (
	errNoCredential   = status.Error(codes.Unauthenticated, "missing session lease")
	errBadLease       = status.Error(codes.PermissionDenied, "lease does not match session")
	errWrongSession   = status.Error(codes.PermissionDenied, "lease is for a different session")
	errNotProvisioner = status.Error(codes.PermissionDenied, "provisioner credential required")
)

// Authenticator verifies the credential on every CoreService call.
type Authenticator struct {
	leases LeaseVerifier
	// provisionerToken authenticates the provisioner, which holds no lease:
	// it exists before any pod does. Shared rather than per-call because the
	// provisioner is a single deployment, not a fleet of them.
	provisionerToken string
}

func NewAuthenticator(leases LeaseVerifier, provisionerToken string) *Authenticator {
	return &Authenticator{leases: leases, provisionerToken: provisionerToken}
}

// authenticate establishes WHO is calling. It returns the caller's session id,
// or "" for the provisioner.
func (a *Authenticator) authenticate(ctx context.Context, method string) (sessionID string, err error) {
	md, _ := metadata.FromIncomingContext(ctx)

	if provisionerMethods[method] {
		if !a.checkProvisioner(md) {
			return "", errNotProvisioner
		}
		return "", nil
	}
	// The provisioner is also allowed to call ordinary methods; it is a
	// trusted component, not a session.
	if a.checkProvisioner(md) {
		return "", nil
	}

	sid := first(md, mdSessionID)
	lease := first(md, mdLeaseID)
	if sid == "" || lease == "" {
		return "", errNoCredential
	}
	ok, err := a.leases.VerifyLease(ctx, sid, lease)
	if err != nil {
		// Deliberately not Unauthenticated: a database blip is not a rejected
		// credential, and the caller should retry rather than give up.
		return "", status.Error(codes.Unavailable, "cannot verify lease")
	}
	if !ok {
		return "", errBadLease
	}
	return sid, nil
}

func (a *Authenticator) checkProvisioner(md metadata.MD) bool {
	if a.provisionerToken == "" {
		return false
	}
	got := first(md, mdProvisionerToken)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(a.provisionerToken)) == 1
}

// authorize checks the request does not act on somebody else's session.
func authorize(method, callerSession string, req any) error {
	if callerSession == "" {
		return nil // the provisioner
	}
	extract, bound := callerSessionOf[method]
	if bound {
		if claimed := extract(req); claimed != callerSession {
			slog.Warn("coreserver rejected a cross-session call",
				"method", method, "lease_session", callerSession, "claimed_session", claimed)
			return errWrongSession
		}
		return nil
	}
	if unboundMethods[method] {
		return nil
	}
	// unknownMethodPolicy: a method in neither table is new, and nobody
	// decided what it may reach. Fail closed — the alternative is that adding
	// an RPC silently ships it unauthorised, which is exactly how the
	// "wire it into EVERY path" traps in this repo happen.
	slog.Error("coreserver: method is in no authorization table, refusing", "method", method)
	return status.Error(codes.PermissionDenied, "method has no authorization rule")
}

// UnaryInterceptor authenticates the caller and binds it to its own session.
func (a *Authenticator) UnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	caller, err := a.authenticate(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	if err := authorize(info.FullMethod, caller, req); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// StreamInterceptor is not optional. core's gRPC server had ONLY a unary
// chain, so a unary-only check would leave StreamHumanMessages — the live feed
// of everything a human types to a session, permission answers included — and
// ReportPodEvents completely open, while every test still passed.
//
// The request message of a server-stream arrives after the interceptor runs,
// so authorization is applied in RecvMsg rather than here.
func (a *Authenticator) StreamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	caller, err := a.authenticate(ss.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	return handler(srv, &authorizedStream{ServerStream: ss, method: info.FullMethod, caller: caller})
}

type authorizedStream struct {
	grpc.ServerStream
	method string
	caller string
}

func (s *authorizedStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	return authorize(s.method, s.caller, m)
}

func first(md metadata.MD, key string) string {
	v := md.Get(key)
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
