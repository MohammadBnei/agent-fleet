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

func TestOpenPullRequest(t *testing.T) {
	s, _, _ := newTestServer(t)
	app := &fakeApp{}
	s.SetGitHub(app)
	req := func() *agentfleetv1.OpenPullRequestRequest {
		return &agentfleetv1.OpenPullRequestRequest{Repo: "MohammadBnei/agent-fleet", Branch: "fix/x", HeadSha: headSHA, Title: "Fix x"}
	}
	resp, err := s.OpenPullRequest(context.Background(), req())
	if err != nil || resp.GetUrl() != "https://x/pull/1" || len(app.prs) != 1 || app.prs[0] != "MohammadBnei/agent-fleet fix/x "+headSHA+" Fix x" {
		t.Fatalf("open: %v %v %v", resp, err, app.prs)
	}
	for name, mut := range map[string]func(*agentfleetv1.OpenPullRequestRequest){
		"repo":      func(r *agentfleetv1.OpenPullRequestRequest) { r.Repo = "../x" },
		"branch":    func(r *agentfleetv1.OpenPullRequestRequest) { r.Branch = "" },
		"flag-like": func(r *agentfleetv1.OpenPullRequestRequest) { r.Branch = "--x" },
		"dotdot":    func(r *agentfleetv1.OpenPullRequestRequest) { r.Branch = "a/../../../x" },
		"lock":      func(r *agentfleetv1.OpenPullRequestRequest) { r.Branch = "a/x.lock" },
		"slashes":   func(r *agentfleetv1.OpenPullRequestRequest) { r.Branch = "a//x" },
		"trailing":  func(r *agentfleetv1.OpenPullRequestRequest) { r.Branch = "a/" },
		"sha":       func(r *agentfleetv1.OpenPullRequestRequest) { r.HeadSha = "abc" },
		"title":     func(r *agentfleetv1.OpenPullRequestRequest) { r.Title = "" },
	} {
		r := req()
		mut(r)
		if _, err := s.OpenPullRequest(context.Background(), r); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A branch that moved past the approved commit is final: FailedPrecondition, not a retry.
	app.prErr = fmt.Errorf("moved: %w", github.ErrStaleHead)
	if _, err := s.OpenPullRequest(context.Background(), req()); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("stale head: %v", err)
	}
}

// docs/adr/0084 §6: core checked the definition owns the repo; the shape is re-checked here.
func TestCreateIssue(t *testing.T) {
	s, _, _ := newTestServer(t)
	app := &fakeApp{}
	s.SetGitHub(app)
	req := func() *agentfleetv1.CreateIssueRequest {
		return &agentfleetv1.CreateIssueRequest{Repo: "MohammadBnei/agent-fleet", Title: "Fix x", Body: "b"}
	}
	resp, err := s.CreateIssue(context.Background(), req())
	if err != nil || resp.GetUrl() != "https://x/issues/1" || len(app.issues) != 1 || app.issues[0] != "MohammadBnei/agent-fleet Fix x" {
		t.Fatalf("open: %v %v %v", resp, err, app.issues)
	}
	for name, mut := range map[string]func(*agentfleetv1.CreateIssueRequest){
		"repo":      func(r *agentfleetv1.CreateIssueRequest) { r.Repo = "../x" },
		"no slash":  func(r *agentfleetv1.CreateIssueRequest) { r.Repo = "agent-fleet" },
		"title":     func(r *agentfleetv1.CreateIssueRequest) { r.Title = "" },
		"multiline": func(r *agentfleetv1.CreateIssueRequest) { r.Title = "a\nb" },
	} {
		r := req()
		mut(r)
		if _, err := s.CreateIssue(context.Background(), r); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(app.issues) != 1 {
		t.Errorf("a refused request reached the App: %v", app.issues)
	}
	app.issueErr = &github.StatusError{Status: 404}
	if _, err := s.CreateIssue(context.Background(), req()); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a 4xx is final: %v", err)
	}
	app.issueErr = &github.StatusError{Status: 502}
	if _, err := s.CreateIssue(context.Background(), req()); status.Code(err) != codes.Unavailable {
		t.Errorf("a 5xx retries: %v", err)
	}
	s.SetGitHub(nil)
	if _, err := s.CreateIssue(context.Background(), req()); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("no App: %v", err)
	}
}
