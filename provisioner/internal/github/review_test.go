package github

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// reviewGitHub serves one PR (MohammadBnei/notes#7), its reviews and the
// statuses endpoint; everything else goes to the App fake.
type reviewGitHub struct {
	*fakeGitHub
	mu       sync.Mutex
	author   string
	headRepo string // "" is a deleted fork: head.repo is null
	state    string
	head     string // the PR's head sha; "" is sha
	failSt   bool   // the statuses endpoint answers 500
	reject   bool   // the first review with inline comments answers 422
	reviews  []map[string]any
	statuses []map[string]any
}

func (g *reviewGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/app":
		g.checkJWT(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{"slug": "ukubi-fleet"})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/MohammadBnei/notes/pulls/7":
		pr := map[string]any{
			"state": g.state, "user": map[string]any{"login": g.author},
			"head": map[string]any{"sha": cmp.Or(g.head, sha), "repo": nil},
			"base": map[string]any{"repo": map[string]any{"full_name": "MohammadBnei/notes"}},
		}
		if g.headRepo != "" {
			pr["head"].(map[string]any)["repo"] = map[string]any{"full_name": g.headRepo}
		}
		_ = json.NewEncoder(w).Encode(pr)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/MohammadBnei/notes/pulls/7/reviews":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.reviews = append(g.reviews, body)
		if c, _ := body["comments"].([]any); g.reject && len(c) > 0 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Line could not be resolved"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"html_url": "https://github.com/MohammadBnei/notes/pull/7#pullrequestreview-1"})
	case r.Method == http.MethodPost && r.URL.Path == "/repos/MohammadBnei/notes/statuses/"+sha:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.statuses = append(g.statuses, body)
		if g.failSt {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	default:
		g.mu.Unlock()
		g.fakeGitHub.ServeHTTP(w, r)
		g.mu.Lock()
	}
}

func newReviewApp(t *testing.T, author, headRepo string) (*App, *reviewGitHub) {
	t.Helper()
	a, f := newApp(t, false)
	g := &reviewGitHub{fakeGitHub: f, author: author, headRepo: headRepo, state: "open"}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	a.API = srv.URL
	a.Trusted = []string{"MohammadBnei"}
	return a, g
}

func review(verdict string) Review {
	return Review{Owner: "MohammadBnei", Repo: "notes", PR: 7, HeadSHA: sha, Verdict: verdict, Body: "Looks fine.",
		Comments: []ReviewComment{{Path: "main.go", Line: 3, Body: "nit"}}}
}

func TestPostReview_EventAndStatus(t *testing.T) {
	const own = "MohammadBnei/notes"
	for _, tc := range []struct {
		name, author, headRepo, verdict string
		event, status, firstLine        string
	}{
		{"trusted approve", "mohammadbnei", own, "approve", "APPROVE", "success", "Verdict: Approve"},
		{"trusted changes", "MohammadBnei", own, "changes", "REQUEST_CHANGES", "failure", "Verdict: Changes requested"},
		{"trusted comment", "MohammadBnei", own, "comment", "COMMENT", "", "Verdict: Comment"},
		// GitHub refuses an App approving its own PR: a comment, the status still says.
		{"bot-authored approve", "ukubi-fleet[bot]", own, "approve", "COMMENT", "success", "Verdict: Approve"},
		// Unowned code ran in the reviewer: its verdict is text, never a status.
		{"stranger approve", "someone", own, "approve", "COMMENT", "", "Verdict: Approve"},
		{"fork by trusted", "MohammadBnei", "someone/notes", "changes", "COMMENT", "", "Verdict: Changes requested"},
		{"deleted fork", "MohammadBnei", "", "approve", "COMMENT", "", "Verdict: Approve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, g := newReviewApp(t, tc.author, tc.headRepo)
			got, err := a.PostReview(context.Background(), review(tc.verdict))
			if err != nil {
				t.Fatal(err)
			}
			if got.Event != tc.event || got.Status != tc.status || !strings.HasSuffix(got.URL, "pullrequestreview-1") {
				t.Errorf("posted %+v, want event %s status %q", got, tc.event, tc.status)
			}
			if len(g.reviews) != 1 {
				t.Fatalf("%d reviews", len(g.reviews))
			}
			rv := g.reviews[0]
			if rv["event"] != tc.event || rv["commit_id"] != sha || !strings.HasPrefix(rv["body"].(string), tc.firstLine+"\n") {
				t.Errorf("review %v", rv)
			}
			if tc.status == "" {
				if len(g.statuses) != 0 {
					t.Errorf("a status was set: %v", g.statuses)
				}
				return
			}
			if len(g.statuses) != 1 {
				t.Fatalf("%d statuses", len(g.statuses))
			}
			st := g.statuses[0]
			if st["state"] != tc.status || st["context"] != "agent-review" || st["target_url"] != got.URL || st["description"] == "" {
				t.Errorf("status %v", st)
			}
		})
	}
}

// The review token is narrow: this repo only, reviews and statuses.
func TestPostReview_NarrowToken(t *testing.T) {
	a, g := newReviewApp(t, "MohammadBnei", "MohammadBnei/notes")
	if _, err := a.PostReview(context.Background(), review("approve")); err != nil {
		t.Fatal(err)
	}
	m := g.mints[len(g.mints)-1]
	perms, _ := m["permissions"].(map[string]any)
	if perms["pull_requests"] != "write" || perms["statuses"] != "write" || perms["metadata"] != "read" || len(perms) != 3 {
		t.Errorf("permissions %v", perms)
	}
	if repos, _ := m["repositories"].([]any); len(repos) != 1 || repos[0] != "notes" {
		t.Errorf("repositories %v", m["repositories"])
	}
	if _, err := a.Token(context.Background(), "MohammadBnei", "notes", false); err != nil {
		t.Fatal(err)
	}
	if len(g.mints) != 2 {
		t.Errorf("the definition token reused the review one: %d mints", len(g.mints))
	}
}

// A 422 on the inline comments (a line outside the diff) retries once with
// the body only, the comments listed in it.
func TestPostReview_422RetriesBodyOnly(t *testing.T) {
	a, g := newReviewApp(t, "MohammadBnei", "MohammadBnei/notes")
	g.reject = true
	if _, err := a.PostReview(context.Background(), review("approve")); err != nil {
		t.Fatal(err)
	}
	if len(g.reviews) != 2 {
		t.Fatalf("%d review posts, want 2", len(g.reviews))
	}
	retry := g.reviews[1]
	if retry["comments"] != nil || !strings.Contains(retry["body"].(string), "main.go:3") || !strings.Contains(retry["body"].(string), "nit") {
		t.Errorf("retry %v", retry)
	}
}

// The folded body stays under GitHub's 65536-character review limit, or the
// retry is refused too and the review is lost.
func TestPostReview_422RetryFitsGitHubsLimit(t *testing.T) {
	a, g := newReviewApp(t, "MohammadBnei", "MohammadBnei/notes")
	g.reject = true
	r := review("approve")
	r.Body = strings.Repeat("é", 60000)
	r.Comments = []ReviewComment{{Path: "main.go", Line: 3, Body: strings.Repeat("x", 20000)}}
	if _, err := a.PostReview(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(g.reviews[1]["body"].(string)); n > 65536 {
		t.Errorf("retry body %d characters", n)
	}
}

func TestPostReview_ClosedPRIsRefused(t *testing.T) {
	a, g := newReviewApp(t, "MohammadBnei", "MohammadBnei/notes")
	g.state = "closed"
	if _, err := a.PostReview(context.Background(), review("approve")); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("err %v, want ErrNotOpen", err)
	}
	if len(g.reviews) != 0 {
		t.Errorf("posted on a closed PR")
	}
}

func TestApp_ReadOnlyTokenIsReadOnlyAndCachedApart(t *testing.T) {
	a, f := newApp(t, false)
	ctx := context.Background()
	rw, _ := a.Token(ctx, "MohammadBnei", "notes", false)
	ro, err := a.Token(ctx, "MohammadBnei", "notes", true)
	if err != nil || ro == rw {
		t.Fatalf("read-only %q (rw %q) %v", ro, rw, err)
	}
	perms, _ := f.mints[1]["permissions"].(map[string]any)
	if perms["contents"] != "read" || perms["pull_requests"] != "read" || perms["metadata"] != "read" || len(perms) != 3 {
		t.Errorf("permissions %v", perms)
	}
}

// The PR moved past the reviewed commit: nothing is posted.
func TestPostReview_StaleHeadIsRefused(t *testing.T) {
	a, g := newReviewApp(t, "MohammadBnei", "MohammadBnei/notes")
	g.head = "fedcba9876543210fedcba9876543210fedcba98"
	if _, err := a.PostReview(context.Background(), review("approve")); !errors.Is(err, ErrStaleHead) {
		t.Fatalf("err %v, want ErrStaleHead", err)
	}
	if len(g.reviews) != 0 || len(g.statuses) != 0 {
		t.Errorf("posted on a stale head: %d reviews, %d statuses", len(g.reviews), len(g.statuses))
	}
}

// A status that fails after the review is posted is not an error: the review
// is on GitHub, and an error would invite the agent to post a second one.
func TestPostReview_StatusFailureKeepsTheReview(t *testing.T) {
	a, g := newReviewApp(t, "MohammadBnei", "MohammadBnei/notes")
	g.failSt = true
	got, err := a.PostReview(context.Background(), review("approve"))
	if err != nil {
		t.Fatal(err)
	}
	if got.URL == "" || got.Status != "" || len(g.reviews) != 1 {
		t.Errorf("posted %+v, %d reviews", got, len(g.reviews))
	}
}
