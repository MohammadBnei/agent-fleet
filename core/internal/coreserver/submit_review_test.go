package coreserver

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// The review's arguments are checked before anything reaches the
// provisioner, and name the one PR thread the session must be on.
func TestReviewThread(t *testing.T) {
	sha := strings.Repeat("a1", 20)
	ok := func() *agentfleetv1.SubmitReviewRequest {
		return &agentfleetv1.SubmitReviewRequest{SessionId: "s1", Repo: "o/r", Pr: 7, HeadSha: sha, Verdict: "approve", Body: "fine"}
	}
	key, err := reviewThread(ok())
	if err != nil || key != "github/o/r/pr/7" {
		t.Fatalf("a good review: %q %v", key, err)
	}
	long := ok()
	long.Body = strings.Repeat("é", maxReviewBody) // runes, not bytes
	if _, err := reviewThread(long); err != nil {
		t.Fatalf("a body of exactly the limit: %v", err)
	}
	for name, mut := range map[string]func(*agentfleetv1.SubmitReviewRequest){
		"repo without owner": func(r *agentfleetv1.SubmitReviewRequest) { r.Repo = "r" },
		"repo with a slash":  func(r *agentfleetv1.SubmitReviewRequest) { r.Repo = "o/r/x" },
		"no pr":              func(r *agentfleetv1.SubmitReviewRequest) { r.Pr = 0 },
		"short sha":          func(r *agentfleetv1.SubmitReviewRequest) { r.HeadSha = sha[:39] },
		"upper sha":          func(r *agentfleetv1.SubmitReviewRequest) { r.HeadSha = strings.ToUpper(sha) },
		"non-hex sha":        func(r *agentfleetv1.SubmitReviewRequest) { r.HeadSha = "g" + sha[1:] },
		"unknown verdict":    func(r *agentfleetv1.SubmitReviewRequest) { r.Verdict = "merge" },
		"empty body":         func(r *agentfleetv1.SubmitReviewRequest) { r.Body = "" },
		"body over limit":    func(r *agentfleetv1.SubmitReviewRequest) { r.Body = strings.Repeat("x", maxReviewBody+1) },
	} {
		r := ok()
		mut(r)
		if _, err := reviewThread(r); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", name, err)
		}
	}
}

// A reviewer posts only on the repo its reviewed file names.
func TestOwnRepo(t *testing.T) {
	eff := definitions.Spec{Credentials: &definitions.Credentials{Services: []definitions.Service{
		{Host: "github.com", Paths: []string{"/MohammadBnei/agent-fleet/*", "/MohammadBnei/agent-fleet.git/*"}},
	}}}
	for repo, want := range map[string]bool{
		"MohammadBnei/agent-fleet": true,
		"mohammadbnei/Agent-Fleet": true,
		"MohammadBnei/mountain":    false,
		"someone/agent-fleet":      false,
	} {
		if got := ownRepo(eff, repo); got != want {
			t.Errorf("ownRepo(%s) = %v, want %v", repo, got, want)
		}
	}
	if ownRepo(definitions.Spec{}, "MohammadBnei/agent-fleet") {
		t.Error("a definition without credentials reviews nothing")
	}
}
