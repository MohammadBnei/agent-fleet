package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// maxReviewBody is GitHub's review body limit, in characters.
const maxReviewBody = 65536

const truncated = "\n\n(truncated)"

// reviewPermissions is PostReview's token: a review and the agent-review
// status on one repo, nothing else (ADR-0082 slice 5).
var reviewPermissions = map[string]string{"pull_requests": "write", "statuses": "write", "metadata": "read"}

// ErrNotOpen: the PR is closed or merged, so nothing is posted.
var ErrNotOpen = errors.New("pull request is not open")

// ErrStaleHead: the PR moved past the reviewed commit, so nothing is posted.
var ErrStaleHead = errors.New("pull request head moved")

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

// Posted is what reached GitHub: the review's URL, its event, and the
// agent-review status state ("" for none).
type Posted struct {
	URL, Event, Status string
}

var verdicts = map[string]struct{ event, label, status string }{
	"approve": {"APPROVE", "Approve", "success"},
	"changes": {"REQUEST_CHANGES", "Changes requested", "failure"},
	"comment": {"COMMENT", "Comment", ""},
}

// PostReview posts r as a review, then the agent-review status on its head.
// The formal APPROVE/REQUEST_CHANGES goes only on an owned PR: authored by a
// trusted login or the App itself, from a branch of the same repo. Anything
// else, and the App's own PR (GitHub refuses an author's own approval), is a
// COMMENT. The status follows the verdict on an owned PR only: a fork's or a
// stranger's code ran in the reviewer, so its verdict is text, never a green
// check. A status that fails after the review is posted is logged, not
// returned: the review is on GitHub, and an error would invite a second one.
func (a *App) PostReview(ctx context.Context, r Review) (Posted, error) {
	v, ok := verdicts[r.Verdict]
	if !ok {
		return Posted{}, fmt.Errorf("github: verdict %q", r.Verdict)
	}
	full := r.Owner + "/" + r.Repo
	tok, err := a.cached(ctx, "review:"+strings.ToLower(full), func(ctx context.Context) (token, error) {
		return a.mint(ctx, r.Owner, r.Repo, reviewPermissions)
	})
	if err != nil {
		return Posted{}, err
	}
	base := "/repos/" + full
	var pr struct {
		State string `json:"state"`
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			SHA  string `json:"sha"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"` // null once a fork is deleted
		} `json:"head"`
		Base struct {
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	}
	if err := request(ctx, a.HTTP, a.API, tok, http.MethodGet, base+"/pulls/"+strconv.FormatInt(r.PR, 10), nil, &pr); err != nil {
		return Posted{}, err
	}
	if pr.State != "open" {
		return Posted{}, fmt.Errorf("%s#%d is %s: %w", full, r.PR, pr.State, ErrNotOpen)
	}
	if !strings.EqualFold(pr.Head.SHA, r.HeadSHA) {
		return Posted{}, fmt.Errorf("%s#%d head is %s, reviewed %s: %w", full, r.PR, pr.Head.SHA, r.HeadSHA, ErrStaleHead)
	}
	jwt, err := a.jwt()
	if err != nil {
		return Posted{}, err
	}
	var app struct {
		Slug string `json:"slug"`
	}
	if err := a.call(ctx, jwt, http.MethodGet, "/app", nil, &app); err != nil {
		return Posted{}, err
	}
	bot := app.Slug != "" && strings.EqualFold(pr.User.Login, app.Slug+"[bot]")
	trusted := bot || slices.ContainsFunc(a.Trusted, func(l string) bool { return strings.EqualFold(l, pr.User.Login) })
	owned := trusted && pr.Head.Repo != nil && strings.EqualFold(pr.Head.Repo.FullName, pr.Base.Repo.FullName)
	event := v.event
	if !owned || bot {
		event = "COMMENT"
	}

	body := "Verdict: " + v.label + "\n\n" + r.Body
	post := map[string]any{"commit_id": r.HeadSHA, "body": body, "event": event}
	if len(r.Comments) > 0 {
		post["comments"] = r.Comments
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	path := base + "/pulls/" + strconv.FormatInt(r.PR, 10) + "/reviews"
	err = request(ctx, a.HTTP, a.API, tok, http.MethodPost, path, post, &out)
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusUnprocessableEntity && len(r.Comments) > 0 {
		// Usually a line outside the diff: once more, the comments in the body.
		var b strings.Builder
		b.WriteString(body + "\n\nInline comments GitHub could not place:\n")
		for _, c := range r.Comments {
			fmt.Fprintf(&b, "\n- `%s:%d`: %s", c.Path, c.Line, c.Body)
		}
		folded := []rune(b.String())
		if len(folded) > maxReviewBody {
			// ponytail: core caps only the body; cut the folded comments, not the review.
			folded = append(folded[:maxReviewBody-len(truncated)], []rune(truncated)...)
		}
		delete(post, "comments")
		post["body"] = string(folded)
		err = request(ctx, a.HTTP, a.API, tok, http.MethodPost, path, post, &out)
	}
	if err != nil {
		return Posted{}, err
	}
	status := v.status
	if !owned {
		status = ""
	}
	if status != "" {
		st := map[string]string{"state": status, "context": "agent-review", "description": "Agent review: " + v.label, "target_url": out.HTMLURL}
		if err := request(ctx, a.HTTP, a.API, tok, http.MethodPost, base+"/statuses/"+r.HeadSHA, st, nil); err != nil {
			slog.Warn("github: review posted, agent-review status not set", "review", out.HTMLURL, "err", err)
			status = ""
		}
	}
	return Posted{URL: out.HTMLURL, Event: event, Status: status}, nil
}
