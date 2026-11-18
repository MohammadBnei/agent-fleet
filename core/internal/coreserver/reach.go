package coreserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/promclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
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

// Speaker is agentsvc.Server's game channel: the line goes to the owner's open browser streams, never anywhere else
// (docs/adr/0084 §7). delivered is whether one was watching.
type Speaker interface {
	Speak(ctx context.Context, characterID, text, face, key string) (delivered bool, err error)
}

func (s *Server) SetSpeaker(sp Speaker) { s.speaker = sp }

// SetIssueFunc is the provisioner's CreateIssue, injected so core holds no GitHub credential.
func (s *Server) SetIssueFunc(f func(ctx context.Context, req *agentfleetv1.CreateIssueRequest) (*agentfleetv1.CreateIssueResponse, error)) {
	s.createIssue = f
}

// SetResponderCards is the typed read of the owner's alert cards and the remediations (responder.Cards).
func (s *Server) SetResponderCards(f func(ctx context.Context, since time.Time, limit int) ([]*agentfleetv1.ResponderCard, error)) {
	s.responderCards = f
}

// SetMetrics is core's promclient, for QueryMetrics.
func (s *Server) SetMetrics(q promclient.Querier) { s.metrics = q }

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

// Speak is a character's line for the owner's browser (docs/adr/0084 §7), granted per definition through the speak
// tool. It is an effect (C) with no store: a line spoken with nobody watching is dropped, and delivered says so.
func (s *Server) Speak(ctx context.Context, req *agentfleetv1.SpeakRequest) (*agentfleetv1.SpeakResponse, error) {
	if s.speaker == nil {
		return nil, status.Error(codes.Unavailable, "the game channel is not wired")
	}
	if _, err := s.granted(ctx, req.GetSessionId(), "speak"); err != nil {
		return nil, err
	}
	// No noteEffect: a line is live-only and a replayed key is dropped by the hub, so a crash replaying the turn is
	// harmless, and a cosmetic line must not close the owner's turn with a crash result (docs/adr/0084 §4).
	delivered, err := s.speaker.Speak(ctx, req.GetSessionId(), req.GetText(), req.GetFace(), req.GetIdempotencyKey())
	if err != nil {
		return nil, grpcErr(err)
	}
	return &agentfleetv1.SpeakResponse{Delivered: delivered}, nil
}

// AuthorizeKubeRead answers the sidecar's kubectl_read (ADR-0086 §2). The interceptor has already required B in the
// stored set; here the pinned definition must grant cluster.read. The call is noted as an effect of the turn.
func (s *Server) AuthorizeKubeRead(ctx context.Context, req *agentfleetv1.AuthorizeKubeReadRequest) (*agentfleetv1.AuthorizeKubeReadResponse, error) {
	t, err := s.sessions.Get(ctx, req.GetSessionId())
	if errors.Is(err, sessions.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "session not found")
	}
	if err != nil {
		return nil, fmt.Errorf("AuthorizeKubeRead: %w", err)
	}
	if t.DefinitionName == nil || t.ArchivedAt != nil {
		return nil, status.Error(codes.PermissionDenied, "only an open definition session may read the cluster")
	}
	if !sessions.HoldsTrust(t.TrustSet, "B") {
		return nil, errkeys.Status(codes.PermissionDenied, errkeys.KeyRuleOfTwo,
			"kubectl_read needs B, outside this session's trust set "+t.TrustSet+" (docs/adr/0086): ask the owner through a decision card")
	}
	raw, _, err := s.sessions.PinnedEffective(ctx, req.GetSessionId())
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "definition of %s: %v", req.GetSessionId(), err)
	}
	var eff definitions.Spec
	if err := json.Unmarshal(raw, &eff); err != nil {
		return nil, fmt.Errorf("definition spec of %s: %w", req.GetSessionId(), err)
	}
	if eff.Cluster == nil || !eff.Cluster.Read {
		return nil, status.Error(codes.PermissionDenied, "this definition does not grant cluster.read")
	}
	s.noteEffect(ctx, req.GetSessionId(), req.GetIdempotencyKey())
	return &agentfleetv1.AuthorizeKubeReadResponse{}, nil
}

const (
	maxMetricsQuery    = 2000 // characters of PromQL
	maxMetricsRange    = 24 * time.Hour
	defaultMetricsStep = time.Minute
	minMetricsStep     = 15 * time.Second
	maxMetricsPoints   = 500 // per series, endpoints included: a coarser step is chosen rather than a refusal
	metricsTimeout     = 10 * time.Second
)

// QueryMetrics is a read-only PromQL instant or range query over core's promclient (docs/adr/0084 §6), tagged B.
// It is a named exception to "no passthrough RPC": the egress lock blocks a definition pod from Prometheus. Prometheus
// is only ever read (GET /api/v1/query*), and the range and step arguments are bounded here. The query text is not: PromQL can widen its own window
// (x[90d]), so the cost ceiling is the 10s timeout here, the promclient body cap and Prometheus' own limits.
func (s *Server) QueryMetrics(ctx context.Context, req *agentfleetv1.CoreQueryMetricsRequest) (*agentfleetv1.CoreQueryMetricsResponse, error) {
	if s.metrics == nil {
		return nil, status.Error(codes.Unavailable, "metrics are not wired")
	}
	if _, err := s.granted(ctx, req.GetSessionId(), "query_metrics"); err != nil {
		return nil, err
	}
	q := req.GetQuery()
	if strings.TrimSpace(q) == "" || utf8.RuneCountInString(q) > maxMetricsQuery {
		return nil, status.Errorf(codes.InvalidArgument, "query is required, at most %d characters", maxMetricsQuery)
	}
	ctx, cancel := context.WithTimeout(ctx, metricsTimeout)
	defer cancel()
	var data json.RawMessage
	var err error
	if req.GetStart() == "" {
		data, err = s.metrics.Query(ctx, q)
	} else {
		start, e1 := time.Parse(time.RFC3339, req.GetStart())
		end := time.Now()
		var e2 error
		if v := req.GetEnd(); v != "" {
			end, e2 = time.Parse(time.RFC3339, v)
		}
		if e1 != nil || e2 != nil {
			return nil, status.Error(codes.InvalidArgument, "start and end must be RFC3339")
		}
		if !end.After(start) || end.Sub(start) > maxMetricsRange {
			return nil, status.Errorf(codes.InvalidArgument, "end must be after start, at most %s apart", maxMetricsRange)
		}
		step := time.Duration(req.GetStepSeconds()) * time.Second
		if step <= 0 {
			step = defaultMetricsStep
		}
		step = max(step, minMetricsStep, end.Sub(start)/(maxMetricsPoints-1))
		data, err = s.metrics.QueryRange(ctx, q, start, end, step)
	}
	if err != nil {
		var qe *promclient.QueryError
		switch {
		case errors.As(err, &qe):
			// Prometheus' own reason (a parse error) is the useful part, and the query came from the character.
			return nil, status.Error(codes.InvalidArgument, qe.Error())
		case errors.Is(err, promclient.ErrTooLarge):
			return nil, status.Error(codes.InvalidArgument, "the result is too large. Narrow the query: fewer series, a shorter range, an aggregation.")
		}
		slog.Warn("query_metrics failed", "err", err)
		return nil, status.Error(codes.Unavailable, "metrics are unavailable right now")
	}
	return &agentfleetv1.CoreQueryMetricsResponse{ResultJson: string(data)}, nil
}
