package coreserver

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
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
// definition is the session's definition name, "" for a repo session, read in
// the same lookup (docs/adr/0064).
type LeaseVerifier interface {
	VerifyLease(ctx context.Context, sessionID, leaseID string) (ok bool, definition, trustSet string, err error)
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
	"/agentfleet.v1.CoreService/AuthorizeMemoryWrite": func(r any) string {
		return r.(*agentfleetv1.AuthorizeMemoryWriteRequest).GetSessionId()
	},
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
	"/agentfleet.v1.CoreService/PublishEvent": func(r any) string {
		return r.(*agentfleetv1.PublishEventRequest).GetSessionId()
	},
	"/agentfleet.v1.CoreService/ConsumeGrant": func(r any) string {
		return r.(*agentfleetv1.ConsumeGrantRequest).GetSessionId()
	},
	"/agentfleet.v1.CoreService/DraftCard": func(r any) string {
		return r.(*agentfleetv1.DraftCardRequest).GetSessionId()
	},
	"/agentfleet.v1.CoreService/SubmitReview": func(r any) string {
		return r.(*agentfleetv1.SubmitReviewRequest).GetSessionId()
	},
	"/agentfleet.v1.CoreService/ExportSpans": func(r any) string {
		return r.(*agentfleetv1.ExportSpansRequest).GetSessionId()
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

// definitionMethods are all a DEFINITION session's lease may call
// (docs/adr/0064): what its worker and sidecar legitimately need, and nothing
// that publishes a URL, provisions infra, reads or writes fleet-wide state
// (journal, files, Loki) or reaches a peer. A method not listed is denied, so
// a new RPC is closed to definitions until someone decides otherwise.
// SetPermissionMode is bounded by the definition's mode.allow in
// sessions.Store.SetPermissionMode itself.
//
// ponytail: a fixed table. P4 derives it from the definition's sidecarTools
// (tool -> method); until then every definition gets the same set.
var definitionMethods = map[string]bool{
	// A long-lived agent's memory_write asks core first (docs/adr/0086 §5).
	"/agentfleet.v1.CoreService/AuthorizeMemoryWrite": true,
	"/agentfleet.v1.CoreService/SendMessage":          true,
	"/agentfleet.v1.CoreService/WaitForMessages":      true,
	"/agentfleet.v1.CoreService/StreamHumanMessages":  true,
	"/agentfleet.v1.CoreService/AskUserQuestion":      true,
	"/agentfleet.v1.CoreService/GetSession":           true,
	"/agentfleet.v1.CoreService/SetPermissionMode":    true,
	"/agentfleet.v1.CoreService/UpdateSessionMeta":    true,
	"/agentfleet.v1.CoreService/SaveAgentSessionId":   true,
	"/agentfleet.v1.CoreService/PushToolTelemetry":    true,
	// Granted per definition: the handler checks the pinned spec's
	// sidecarTools for publish_event (docs/adr/0078 §8).
	"/agentfleet.v1.CoreService/PublishEvent": true,
	// A human's one-time approval of this session's own call
	// (docs/adr/0082 §5).
	"/agentfleet.v1.CoreService/ConsumeGrant": true,
	// Granted per definition, as PublishEvent: the handler checks the
	// pinned spec's sidecarTools for draft_card (docs/adr/0082 §6).
	"/agentfleet.v1.CoreService/DraftCard": true,
	// Granted per definition, as PublishEvent: the handler checks the
	// pinned spec's sidecarTools for submit_review (docs/adr/0082 slice 5).
	"/agentfleet.v1.CoreService/SubmitReview": true,
	// The session's own timings, which every pod's sidecar sends; core
	// derives the trace and the identity itself (docs/adr/0081 §7).
	"/agentfleet.v1.CoreService/ExportSpans": true,
}

// provisionerMethods are callable only by the provisioner. ReportPodEvents
// reports on arbitrary sessions by its nature — it is how core learns a pod
// exists — so it cannot be lease-bound, and a session must not be able to
// forge pod lifecycle events for another one.
var provisionerMethods = map[string]bool{
	"/agentfleet.v1.CoreService/ReportPodEvents": true,
}

// sessionOnlyMethods are never callable with the provisioner token: they
// carry a session's own authority outward (PublishEvent inherits the
// session's trust, docs/adr/0078 §8; ConsumeGrant spends a human's approval
// given to that session; DraftCard spends its definition's card budget;
// SubmitReview speaks on GitHub for the session's own PR thread; ExportSpans
// writes a session's trace, which the provisioner must never do),
// which no component may borrow.
var sessionOnlyMethods = map[string]bool{
	"/agentfleet.v1.CoreService/AuthorizeMemoryWrite": true,
	"/agentfleet.v1.CoreService/PublishEvent":         true,
	"/agentfleet.v1.CoreService/ConsumeGrant":         true,
	"/agentfleet.v1.CoreService/DraftCard":            true,
	"/agentfleet.v1.CoreService/SubmitReview":         true,
	"/agentfleet.v1.CoreService/ExportSpans":          true,
}

var (
	errNoCredential   = status.Error(codes.Unauthenticated, "missing session lease")
	errBadLease       = status.Error(codes.PermissionDenied, "lease does not match session")
	errWrongSession   = status.Error(codes.PermissionDenied, "lease is for a different session")
	errNotProvisioner = status.Error(codes.PermissionDenied, "provisioner credential required")
	errDefinition     = status.Error(codes.PermissionDenied, "method not allowed for definition sessions (docs/adr/0064)")
	errAuthor         = status.Error(codes.PermissionDenied, "sessions may only author as agent or worker")
	errSessionOnly    = status.Error(codes.PermissionDenied, "only the session itself may call this")
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

// caller is WHO is calling: a session ("" for the provisioner) and, for a
// definition session, its definition's name.
type caller struct {
	session    string
	definition string
	trustSet   string // docs/adr/0086 §2: the session's stored letters
}

// authenticate establishes who is calling.
func (a *Authenticator) authenticate(ctx context.Context, method string) (caller, error) {
	md, _ := metadata.FromIncomingContext(ctx)

	if provisionerMethods[method] {
		if !a.checkProvisioner(md) {
			return caller{}, errNotProvisioner
		}
		return caller{}, nil
	}
	// The provisioner is also allowed to call ordinary methods; it is a
	// trusted component, not a session.
	if a.checkProvisioner(md) {
		if sessionOnlyMethods[method] {
			return caller{}, errSessionOnly
		}
		return caller{}, nil
	}

	sid := first(md, mdSessionID)
	lease := first(md, mdLeaseID)
	if sid == "" || lease == "" {
		return caller{}, errNoCredential
	}
	ok, definition, trustSet, err := a.leases.VerifyLease(ctx, sid, lease)
	if err != nil {
		// Deliberately not Unauthenticated: a database blip is not a rejected
		// credential, and the caller should retry rather than give up.
		return caller{}, status.Error(codes.Unavailable, "cannot verify lease")
	}
	if !ok {
		return caller{}, errBadLease
	}
	return caller{session: sid, definition: definition, trustSet: trustSet}, nil
}

func (a *Authenticator) checkProvisioner(md metadata.MD) bool {
	if a.provisionerToken == "" {
		return false
	}
	got := first(md, mdProvisionerToken)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(a.provisionerToken)) == 1
}

// tagSpan puts the verified caller session on the call's server span, so a trace
// can be found by session (docs/adr/0081 §4). It is the lease's session, never a
// request field, and the provisioner (no session) adds nothing. The span is the
// one the gRPC stats handler started before any interceptor ran; with no tracing
// it is a no-op span.
func tagSpan(ctx context.Context, c caller) {
	if c.session != "" {
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("fleet.session.id", c.session))
	}
}

// authorize checks the request does not act on somebody else's session, that a
// definition session stays inside definitionMethods, and that a session writes
// to its transcript only in its own voice.
func authorize(method string, c caller, req any) error {
	if c.session == "" {
		return nil // the provisioner
	}
	if err := bind(method, c.session, req); err != nil {
		return err
	}
	if c.definition != "" && !definitionMethods[method] {
		slog.Warn("coreserver refused a definition session's call", "method", method, "session", c.session, "definition", c.definition)
		return errDefinition
	}
	// docs/adr/0086 §2: a method tagged B or C needs that letter in the session's stored set.
	// A tag "A|C" is a memory write: a set holding A may (to quarantine), else it needs C.
	if tag := methodTags[method]; tag != "" && !holdsAny(c.trustSet, tag) {
		slog.Warn("coreserver refused a method outside the session's trust set", "method", method, "session", c.session, "trustSet", c.trustSet)
		return errkeys.Status(codes.PermissionDenied, errkeys.KeyRuleOfTwo,
			method+" needs "+tag+", outside this session's trust set "+c.trustSet+" (docs/adr/0086): ask the owner through a decision card")
	}
	// docs/adr/0086 §5: quarantined is core's word, never the pod's. A set holding A quarantines every journal row.
	if m, ok := req.(*agentfleetv1.AppendJournalRequest); ok {
		m.Quarantined = sessions.HoldsTrust(c.trustSet, "A")
	}
	// Every pod writer authors as "agent" or "worker". "human" is written only
	// by DashboardService and "session" only by core's own PromptSession, and
	// the worker obeys both (a permission_response approves its pending tool
	// call, a permission_mode switches it, up to auto), so a session speaking
	// as either through the sidecar's localhost /message could approve itself
	// (docs/adr/0064). Fleet-wide: repo sessions included.
	if m, ok := req.(*agentfleetv1.SendMessageRequest); ok {
		if f := m.GetFrom(); f != "agent" && f != "worker" {
			slog.Warn("coreserver refused a session authoring as someone else", "session", c.session, "from", f)
			return errAuthor
		}
	}
	return nil
}

// holdsAny reports whether set holds one of the letters in tag ("B", "C" or "A|C").
func holdsAny(set, tag string) bool {
	for _, l := range strings.Split(tag, "|") {
		if sessions.HoldsTrust(set, l) {
			return true
		}
	}
	return false
}

// bind is ADR-0057's rule: a bound method must name the caller's own session,
// an unbound one is a recorded decision, anything else fails closed.
func bind(method, callerSession string, req any) error {
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
	c, err := a.authenticate(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	tagSpan(ctx, c)
	if err := authorize(info.FullMethod, c, req); err != nil {
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
	c, err := a.authenticate(ss.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	tagSpan(ss.Context(), c)
	return handler(srv, &authorizedStream{ServerStream: ss, method: info.FullMethod, caller: c})
}

type authorizedStream struct {
	grpc.ServerStream
	method string
	caller caller
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
