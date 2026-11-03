package grpcserver

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/github"
)

const headSHA = "0123456789abcdef0123456789abcdef01234567"

func reviewReq() *agentfleetv1.PostReviewRequest {
	return &agentfleetv1.PostReviewRequest{
		Repo: "MohammadBnei/agent-fleet", Pr: 12, HeadSha: headSHA, Verdict: "approve", Body: "ok",
		Comments: []*agentfleetv1.ReviewComment{{Path: "a.go", Line: 4, Body: "nit"}},
	}
}

func TestPostReview_PassesTheReviewToTheApp(t *testing.T) {
	s, _, _ := newTestServer(t)
	app := &fakeApp{post: func(github.Review) (github.Posted, error) {
		return github.Posted{URL: "https://x/review", Event: "APPROVE", Status: "success"}, nil
	}}
	s.SetGitHub(app)
	resp, err := s.PostReview(context.Background(), reviewReq())
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetReviewUrl() != "https://x/review" || resp.GetEvent() != "APPROVE" || resp.GetStatusState() != "success" {
		t.Errorf("resp %v", resp)
	}
	r := app.posted[0]
	if r.Owner != "MohammadBnei" || r.Repo != "agent-fleet" || r.PR != 12 || r.HeadSHA != headSHA || r.Verdict != "approve" ||
		len(r.Comments) != 1 || r.Comments[0] != (github.ReviewComment{Path: "a.go", Line: 4, Body: "nit"}) {
		t.Errorf("review %+v", r)
	}
}

// core checks the request too; this is the trust boundary re-check.
func TestPostReview_RefusesABadRequest(t *testing.T) {
	s, _, _ := newTestServer(t)
	app := &fakeApp{}
	s.SetGitHub(app)
	for name, mut := range map[string]func(*agentfleetv1.PostReviewRequest){
		"repo no slash": func(r *agentfleetv1.PostReviewRequest) { r.Repo = "agent-fleet" },
		"repo dots":     func(r *agentfleetv1.PostReviewRequest) { r.Repo = "../x" },
		"pr zero":       func(r *agentfleetv1.PostReviewRequest) { r.Pr = 0 },
		"short sha":     func(r *agentfleetv1.PostReviewRequest) { r.HeadSha = "abc" },
		"upper sha":     func(r *agentfleetv1.PostReviewRequest) { r.HeadSha = "0123456789ABCDEF0123456789abcdef01234567" },
		"verdict":       func(r *agentfleetv1.PostReviewRequest) { r.Verdict = "merge" },
	} {
		req := reviewReq()
		mut(req)
		if _, err := s.PostReview(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
	if len(app.posted) != 0 {
		t.Errorf("a bad request reached GitHub: %v", app.posted)
	}
}

func TestPostReview_Codes(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetGitHub(&fakeApp{post: func(github.Review) (github.Posted, error) {
		return github.Posted{}, fmt.Errorf("pr 12: %w", github.ErrNotOpen)
	}})
	if _, err := s.PostReview(context.Background(), reviewReq()); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("closed PR: %v", err)
	}
	s.SetGitHub(&fakeApp{post: func(github.Review) (github.Posted, error) { return github.Posted{}, fmt.Errorf("github: 502") }})
	if _, err := s.PostReview(context.Background(), reviewReq()); status.Code(err) != codes.Unavailable {
		t.Errorf("GitHub down: %v", err)
	}
	s.SetGitHub(&fakeApp{post: func(github.Review) (github.Posted, error) {
		return github.Posted{}, fmt.Errorf("pr 12: %w", github.ErrStaleHead)
	}})
	if _, err := s.PostReview(context.Background(), reviewReq()); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("stale head: %v", err)
	}
	s.SetGitHub(&fakeApp{post: func(github.Review) (github.Posted, error) { return github.Posted{}, &github.StatusError{Status: 422} }})
	if _, err := s.PostReview(context.Background(), reviewReq()); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("GitHub 422: %v", err)
	}
	s.SetGitHub(&fakeApp{post: func(github.Review) (github.Posted, error) { return github.Posted{}, &github.StatusError{Status: 429} }})
	if _, err := s.PostReview(context.Background(), reviewReq()); status.Code(err) != codes.Unavailable {
		t.Errorf("GitHub rate limit: %v", err)
	}
	s.SetGitHub(nil)
	if _, err := s.PostReview(context.Background(), reviewReq()); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("no App: %v", err)
	}
}

func TestVaultSpec_CarriesReadOnly(t *testing.T) {
	if !vaultSpec(&agentfleetv1.VaultSpec{Name: "x", ReadOnly: true}).ReadOnly {
		t.Error("read_only dropped on the way to the vault")
	}
}
