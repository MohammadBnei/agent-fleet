package coreserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// A standing character's reach (docs/adr/0084 §6). Every method here is granted per definition through
// sidecarTools and acts for the character's own session, on the lease.

// Spawner is agentsvc.Server's spawn half.
type Spawner interface {
	SpawnWorker(ctx context.Context, characterID, definition, input, key string) (string, error)
	SpawnResult(ctx context.Context, characterID, threadID string, wait time.Duration) (*agentfleetv1.WaitForResultResponse, error)
}

func (s *Server) SetSpawner(sp Spawner) { s.spawner = sp }

// SetIssueFunc is the provisioner's CreateIssue, injected so core holds no GitHub credential.
func (s *Server) SetIssueFunc(f func(ctx context.Context, req *agentfleetv1.CreateIssueRequest) (*agentfleetv1.CreateIssueResponse, error)) {
	s.createIssue = f
}

// SetResponderCards is the typed read of the owner's alert cards and the remediations (responder.Cards).
func (s *Server) SetResponderCards(f func(ctx context.Context, since time.Time, limit int) ([]*agentfleetv1.ResponderCard, error)) {
	s.responderCards = f
}

// granted is the session's pinned spec when it is an open definition session whose spec lists tool in sidecarTools.
func (s *Server) granted(ctx context.Context, sessionID, tool string) (*definitions.Spec, error) {
	t, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "session %s: %v", sessionID, err)
	}
	if t.DefinitionName == nil || t.ArchivedAt != nil {
		return nil, status.Error(codes.PermissionDenied, "only an open definition session may do this")
	}
	raw, _, err := s.sessions.PinnedEffective(ctx, sessionID)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "definition of %s: %v", sessionID, err)
	}
	var eff definitions.Spec
	if err := json.Unmarshal(raw, &eff); err != nil {
		return nil, fmt.Errorf("definition spec of %s: %w", sessionID, err)
	}
	if !slices.Contains(eff.SidecarTools, tool) {
		return nil, status.Errorf(codes.PermissionDenied, "this definition does not grant %s", tool)
	}
	return &eff, nil
}

// grpcErr gives a connect error (agentsvc's) its gRPC status, code for code.
func grpcErr(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return status.Error(codes.Code(ce.Code()), ce.Message())
	}
	return err
}

// SpawnWorker starts a run of a definition on the character's spawns.allow.
func (s *Server) SpawnWorker(ctx context.Context, req *agentfleetv1.SpawnWorkerRequest) (*agentfleetv1.SpawnWorkerResponse, error) {
	if s.spawner == nil {
		return nil, status.Error(codes.Unavailable, "spawning is not wired")
	}
	spec, err := s.granted(ctx, req.GetSessionId(), "spawn_worker")
	if err != nil {
		return nil, err
	}
	if !spec.MaySpawn(req.GetDefinition()) {
		return nil, status.Errorf(codes.PermissionDenied, "%s is not on this definition's spawns.allow", req.GetDefinition())
	}
	if req.GetInput() == "" {
		return nil, status.Error(codes.InvalidArgument, "input is required")
	}
	if req.GetIdempotencyKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "idempotency_key is required")
	}
	s.noteEffect(ctx, req.GetSessionId(), req.GetIdempotencyKey())
	id, err := s.spawner.SpawnWorker(ctx, req.GetSessionId(), req.GetDefinition(), req.GetInput(), req.GetIdempotencyKey())
	if err != nil {
		return nil, grpcErr(err)
	}
	return &agentfleetv1.SpawnWorkerResponse{ThreadId: id}, nil
}

// WaitForSpawn long-polls one of the character's own spawned runs.
func (s *Server) WaitForSpawn(ctx context.Context, req *agentfleetv1.WaitForSpawnRequest) (*agentfleetv1.WaitForResultResponse, error) {
	if s.spawner == nil {
		return nil, status.Error(codes.Unavailable, "spawning is not wired")
	}
	if _, err := s.granted(ctx, req.GetSessionId(), "wait_for_result"); err != nil {
		return nil, err
	}
	res, err := s.spawner.SpawnResult(ctx, req.GetSessionId(), req.GetThreadId(), time.Duration(req.GetWaitSeconds())*time.Second)
	if err != nil {
		return nil, grpcErr(err)
	}
	return res, nil
}

const (
	maxIssueTitle = 256
	maxIssueBody  = 60000
)

// OpenIssue opens a GitHub issue in one of the definition's owns.repos. The repo check is the grant: the
// provisioner mints a token for whatever repo it is handed.
func (s *Server) OpenIssue(ctx context.Context, req *agentfleetv1.OpenIssueRequest) (*agentfleetv1.OpenIssueResponse, error) {
	if s.createIssue == nil {
		return nil, status.Error(codes.Unavailable, "the provisioner is not configured")
	}
	spec, err := s.granted(ctx, req.GetSessionId(), "open_issue")
	if err != nil {
		return nil, err
	}
	switch {
	case !reviewRepoRe.MatchString(req.GetRepo()):
		return nil, status.Error(codes.InvalidArgument, "repo must be owner/name")
	case req.GetTitle() == "" || utf8.RuneCountInString(req.GetTitle()) > maxIssueTitle || strings.ContainsAny(req.GetTitle(), "\r\n"):
		return nil, status.Errorf(codes.InvalidArgument, "title is required, one line, at most %d characters", maxIssueTitle)
	case utf8.RuneCountInString(req.GetBody()) > maxIssueBody:
		return nil, status.Errorf(codes.InvalidArgument, "the body is over %d characters", maxIssueBody)
	}
	if spec.Owns == nil || !slices.ContainsFunc(spec.Owns.Repos, func(r string) bool { return strings.EqualFold(r, req.GetRepo()) }) {
		return nil, status.Errorf(codes.PermissionDenied, "this definition does not own %s", req.GetRepo())
	}
	s.noteEffect(ctx, req.GetSessionId(), req.GetIdempotencyKey())
	res, err := s.createIssue(ctx, &agentfleetv1.CreateIssueRequest{Repo: req.GetRepo(), Title: req.GetTitle(), Body: req.GetBody()})
	if err != nil {
		// An Unavailable or a timeout can follow a create that landed, and the sidecar retries Unavailable: report it
		// as Unknown so a possibly-made issue is never opened again by a retry (docs/adr/0084 §4).
		if c := status.Code(err); c == codes.Unavailable || c == codes.DeadlineExceeded {
			return nil, status.Errorf(codes.Unknown, "the issue may have been opened (%s: %s); check the repo before trying again", c, status.Convert(err).Message())
		}
		return nil, err // the provisioner's status as is, like SubmitReview
	}
	return &agentfleetv1.OpenIssueResponse{Url: res.GetUrl()}, nil
}

const (
	defaultCardsLimit = 20
	maxCardsLimit     = 50
	defaultCardsSince = 24 * time.Hour
)

// ListResponderCards reads the first responders' cards and remediations as typed fields (docs/adr/0084 §5).
func (s *Server) ListResponderCards(ctx context.Context, req *agentfleetv1.ListResponderCardsRequest) (*agentfleetv1.ListResponderCardsResponse, error) {
	if s.responderCards == nil {
		return nil, status.Error(codes.Unavailable, "responder cards are not wired")
	}
	if _, err := s.granted(ctx, req.GetSessionId(), "list_responder_cards"); err != nil {
		return nil, err
	}
	since := time.Now().Add(-defaultCardsSince)
	if v := req.GetSince(); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "since must be RFC3339")
		}
		since = t
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultCardsLimit
	}
	limit = min(limit, maxCardsLimit)
	cards, err := s.responderCards(ctx, since, limit)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "responder cards: %v", err)
	}
	return &agentfleetv1.ListResponderCardsResponse{Cards: cards}, nil
}

// callerSession is the lease-verified session of a call whose request names none (the authenticator checked
// x-fleet-session-id against the lease before the handler ran), "" for the provisioner.
func callerSession(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	return first(md, mdSessionID)
}

// scopeLogs is ViewLogs' rule for a definition session (docs/adr/0084 §6): it needs the view_logs grant, and the
// namespace must be named and on cluster.logNamespaces. A repo session or the provisioner is unchanged.
func (s *Server) scopeLogs(ctx context.Context, namespace string) error {
	sid := callerSession(ctx)
	if sid == "" {
		return nil
	}
	t, err := s.sessions.Get(ctx, sid)
	if err != nil {
		return status.Errorf(codes.NotFound, "session %s: %v", sid, err)
	}
	if t.DefinitionName == nil {
		return nil
	}
	spec, err := s.granted(ctx, sid, "view_logs")
	if err != nil {
		return err
	}
	if spec.Cluster == nil || !slices.Contains(spec.Cluster.LogNamespaces, namespace) {
		return status.Errorf(codes.PermissionDenied, "namespace %q is not on this definition's cluster.logNamespaces", namespace)
	}
	return nil
}
