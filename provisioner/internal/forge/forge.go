// Package forge is the provisioner's forge surface (docs/adr/0075 §2): what
// the GitHub App (internal/github) and Forgejo (internal/forgejo) both do, and
// the types they share. FORGE picks one at startup (internal/config).
package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Forge is every forge call the provisioner makes. grpcserver holds one; the
// vault uses its Token alone (vault.Forge).
type Forge interface {
	// ListRepos is the fleet's repo list, core's repos sync source.
	ListRepos(ctx context.Context) ([]Repo, error)
	// Token is a definition vault's key for one repo, read-only when readOnly.
	Token(ctx context.Context, owner, repo string, readOnly bool) (string, error)
	// OwnerToken is a token over owner's repos, for its repo pods' vault.
	OwnerToken(ctx context.Context, owner string) (string, error)
	// Owners are the allowlisted owners' logins.
	Owners(ctx context.Context) ([]string, error)
	// PostReview posts an agent review and its agent-review status.
	PostReview(ctx context.Context, r Review) (Posted, error)
	// OpenPR returns the pull request of a pushed branch (GitHub: opens it;
	// Forgejo: the AGit push opened it) at an approved commit.
	OpenPR(ctx context.Context, owner, repo, branch, headSHA, title, body string) (string, error)
	// CreateIssue opens an issue for a standing character (docs/adr/0084 §6).
	CreateIssue(ctx context.Context, owner, repo, title, body string) (string, error)
}

// Repo is one repo the fleet may work in. The JSON names are GitHub's, which
// Forgejo shares.
type Repo struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
}

// Review is one agent review of owner/repo#PR at HeadSHA. Verdict is
// approve, changes or comment; core and the grpc handler have checked it.
type Review struct {
	Owner, Repo string
	PR          int64
	HeadSHA     string
	Verdict     string
	Body        string
	Comments    []ReviewComment
}

type ReviewComment struct {
	Path string `json:"path"`
	Line int32  `json:"line"`
	Body string `json:"body"`
}

// Posted is what reached the forge: the review's URL, its event, and the
// agent-review status state ("" for none).
type Posted struct {
	URL, Event, Status string
}

// ErrNotOpen: the PR is closed or merged, so nothing is posted.
var ErrNotOpen = errors.New("pull request is not open")

// ErrStaleHead: the PR moved past the reviewed commit, so nothing is posted.
var ErrStaleHead = errors.New("pull request head moved")

// ErrRefused: the forge's answer fails a check a retry will not change (an
// AGit PR by someone else, or against another base), so nothing is returned.
var ErrRefused = errors.New("refused")

// StatusError is a non-2xx answer from the forge's API: PostReview retries a
// 422, and grpcserver answers a 4xx but 429 FailedPrecondition.
type StatusError struct {
	Status int
	Msg    string
}

func (e *StatusError) Error() string { return e.Msg }

// MaxReviewBody is GitHub's review body limit, in characters; Forgejo's is
// no smaller.
const MaxReviewBody = 65536

const truncated = "\n\n(truncated)"

// FoldComments is body with comments the forge could not place appended,
// cut to MaxReviewBody: the second try after a 422 on inline comments.
func FoldComments(body, forgeName string, comments []ReviewComment) string {
	var b strings.Builder
	b.WriteString(body + "\n\nInline comments " + forgeName + " could not place:\n")
	for _, c := range comments {
		fmt.Fprintf(&b, "\n- `%s:%d`: %s", c.Path, c.Line, c.Body)
	}
	folded := []rune(b.String())
	if len(folded) > MaxReviewBody {
		// ponytail: core caps only the body; cut the folded comments, not the review.
		folded = append(folded[:MaxReviewBody-len(truncated)], []rune(truncated)...)
	}
	return string(folded)
}
