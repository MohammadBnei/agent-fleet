package coreserver

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// maxReviewBody is the longest review body in characters: GitHub's limit is
// 65536, and the provisioner adds a verdict line and, when inline comments are
// refused, those comments. A longer body is refused, never truncated.
const maxReviewBody = 60000

var (
	reviewRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	headSHARe    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// SubmitReview posts one GitHub review on the PR this session was woken for
// (docs/adr/0082 slice 5). Only an open definition session whose pinned spec
// grants submit_review may call it, and only on its own thread: a reviewer
// woken on PR 7 cannot review PR 8. Core holds no GitHub credential, so the
// provisioner posts it.
func (s *Server) SubmitReview(ctx context.Context, req *agentfleetv1.SubmitReviewRequest) (*agentfleetv1.SubmitReviewResponse, error) {
	if s.provisioner == nil {
		return nil, status.Error(codes.Unavailable, "the provisioner is not configured")
	}
	key, err := reviewThread(req)
	if err != nil {
		return nil, err
	}
	t, err := s.sessions.Get(ctx, req.GetSessionId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "session %s: %v", req.GetSessionId(), err)
	}
	if t.DefinitionName == nil || t.ArchivedAt != nil {
		return nil, status.Error(codes.PermissionDenied, "only an open definition session may submit reviews")
	}
	spec, _, err := s.agentSpecOf(ctx, req.GetSessionId())
	if err != nil {
		return nil, err
	}
	if spec == nil || !slices.Contains(spec.GetSidecarTools(), "submit_review") {
		return nil, status.Error(codes.PermissionDenied, "this definition does not grant submit_review")
	}
	// The provisioner mints a review token for whatever repo it is handed: a
	// reviewer reviews only the repo its reviewed file names, whatever threads
	// it is routed.
	raw, err := s.sessions.PinnedEffective(ctx, req.GetSessionId())
	if err != nil {
		return nil, err
	}
	var eff definitions.Spec
	if err := json.Unmarshal(raw, &eff); err != nil {
		return nil, fmt.Errorf("definition spec of %s: %w", req.GetSessionId(), err)
	}
	if !ownRepo(eff, req.GetRepo()) {
		return nil, status.Errorf(codes.PermissionDenied, "this definition reviews %s, not %s", definitions.GitHubRepo(eff.Credentials), req.GetRepo())
	}
	// ponytail: the thread is read through OpenOnThreadKey (case-insensitive,
	// as GitHub names are) rather than a thread_key on Session.
	open, err := s.sessions.OpenOnThreadKey(ctx, key)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "thread %s: %v", key, err)
	}
	if !slices.ContainsFunc(open, func(r sessions.ThreadRef) bool { return r.ID == req.GetSessionId() }) {
		return nil, status.Errorf(codes.PermissionDenied, "this session is not on thread %s", key)
	}
	res, err := s.provisioner.PostReview(ctx, &agentfleetv1.PostReviewRequest{
		Repo: req.GetRepo(), Pr: req.GetPr(), HeadSha: req.GetHeadSha(),
		Verdict: req.GetVerdict(), Body: req.GetBody(), Comments: req.GetComments(),
	})
	if err != nil {
		// The provisioner's status as is: its code (FailedPrecondition for a
		// closed PR, say) is the agent's to read.
		return nil, err
	}
	return &agentfleetv1.SubmitReviewResponse{ReviewUrl: res.GetReviewUrl(), Event: res.GetEvent()}, nil
}

// reviewThread checks a review's arguments and names the thread the session
// must be on, github/<owner>/<repo>/pr/<n> (dispatch's prOf).
func reviewThread(req *agentfleetv1.SubmitReviewRequest) (string, error) {
	switch {
	case !reviewRepoRe.MatchString(req.GetRepo()):
		return "", status.Error(codes.InvalidArgument, "repo must be owner/name")
	case req.GetPr() <= 0:
		return "", status.Error(codes.InvalidArgument, "pr must be a positive number")
	case !headSHARe.MatchString(req.GetHeadSha()):
		return "", status.Error(codes.InvalidArgument, "head_sha must be the 40 lowercase hex characters of the commit reviewed")
	case !slices.Contains([]string{"approve", "changes", "comment"}, req.GetVerdict()):
		return "", status.Error(codes.InvalidArgument, "verdict must be approve, changes or comment")
	case req.GetBody() == "":
		return "", status.Error(codes.InvalidArgument, "a review needs a body")
	case utf8.RuneCountInString(req.GetBody()) > maxReviewBody:
		return "", status.Errorf(codes.InvalidArgument, "the body is over %d characters; shorten it", maxReviewBody)
	}
	return fmt.Sprintf("github/%s/pr/%d", req.GetRepo(), req.GetPr()), nil
}

// ownRepo: repo is the one GitHub repo the definition's credentials name.
func ownRepo(eff definitions.Spec, repo string) bool {
	own := definitions.GitHubRepo(eff.Credentials)
	return own != "" && strings.EqualFold(own, repo)
}
