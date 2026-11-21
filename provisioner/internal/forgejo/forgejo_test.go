package forgejo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/forge"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

// fake is the Forgejo routes the client calls, under /api/v1. Each request is
// recorded as "<token> <METHOD> <path>" with its JSON body.
type fake struct {
	mu    sync.Mutex
	calls []string
	posts map[string]map[string]any
	repos []map[string]any
	pulls map[string]map[string]any // "<path after /repos/o/r/pulls/>" -> PR
	// failReviews answers the first review POST with this status.
	failReviews int
	// maxItems is the server's MAX_RESPONSE_ITEMS, the cap on limit.
	maxItems int
}

func pr(state, poster, base, head string) map[string]any {
	return map[string]any{
		"html_url": "https://git.test/MohammadBnei/notes/pulls/7", "state": state, "user": map[string]any{"login": poster},
		"head": map[string]any{"sha": head, "repo": map[string]any{"full_name": "MohammadBnei/notes"}},
		"base": map[string]any{"ref": base, "repo": map[string]any{"full_name": "MohammadBnei/notes"}},
	}
}

func repoJSON(full string, push bool) map[string]any {
	owner, name, _ := strings.Cut(full, "/")
	return map[string]any{"id": 1, "name": name, "full_name": full, "clone_url": "https://git.test/" + full + ".git", "default_branch": "main",
		"owner": map[string]any{"login": owner}, "permissions": map[string]any{"pull": true, "push": push}}
}

func setup(t *testing.T) (*Client, *fake) {
	t.Helper()
	f := &fake{posts: map[string]map[string]any{}, pulls: map[string]map[string]any{}, maxItems: 50,
		repos: []map[string]any{repoJSON("MohammadBnei/notes", false), repoJSON("MohammadBnei/pushable", true), repoJSON("stranger/notes", false)}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path, _ := strings.CutPrefix(r.URL.EscapedPath(), "/api/v1")
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
		f.calls = append(f.calls, tok+" "+r.Method+" "+path)
		if r.Method == http.MethodPost {
			var body map[string]any
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &body)
			f.posts[path] = body
		}
		var out any
		switch {
		case r.Method == http.MethodGet && path == "/user/repos":
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			limit = min(limit, f.maxItems)
			lo, hi := min((page-1)*limit, len(f.repos)), min(page*limit, len(f.repos))
			out = f.repos[lo:hi]
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/MohammadBnei/notes/pulls/"):
			p, ok := f.pulls[strings.TrimPrefix(path, "/repos/MohammadBnei/notes/pulls/")]
			if !ok {
				http.Error(w, `{"message":"pull request does not exist"}`, http.StatusNotFound)
				return
			}
			out = p
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/reviews"):
			if f.failReviews != 0 {
				w.WriteHeader(f.failReviews)
				f.failReviews = 0
				return
			}
			out = map[string]any{"html_url": "https://git.test/review/1"}
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/repos/MohammadBnei/notes/statuses/"):
			out = map[string]any{}
		case r.Method == http.MethodPost && path == "/repos/MohammadBnei/notes/issues":
			out = map[string]any{"html_url": "https://git.test/MohammadBnei/notes/issues/3"}
		default:
			http.Error(w, `{"message":"no route"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	for k, v := range map[string]string{ReadTokenKey: "fj-read\n", WriteTokenKey: "fj-write", ReviewTokenKey: "fj-review"} {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := New(srv.URL+"/", dir, []string{"MohammadBnei"})
	c.Trusted = []string{"MohammadBnei"}
	return c, f
}

// The repo list is the boundary: only an allowlisted owner's repos where
// ukubi-fleet is a read collaborator, and Token answers for those alone.
func TestListReposAndToken(t *testing.T) {
	c, _ := setup(t)
	ctx := context.Background()
	repos, err := c.ListRepos(ctx)
	if err != nil || len(repos) != 1 || repos[0].FullName != "MohammadBnei/notes" || repos[0].DefaultBranch != "main" {
		t.Fatalf("repos %+v, %v", repos, err)
	}
	if tok, err := c.Token(ctx, "mohammadbnei", "notes", true); err != nil || tok != "fj-read" {
		t.Errorf("read-only token %q, %v", tok, err)
	}
	if tok, err := c.Token(ctx, "MohammadBnei", "notes", false); err != nil || tok != "fj-write" {
		t.Errorf("write token %q, %v", tok, err)
	}
	for _, full := range []string{"MohammadBnei/pushable", "stranger/notes", "MohammadBnei/absent"} {
		o, r, _ := strings.Cut(full, "/")
		if tok, err := c.Token(ctx, o, r, false); err == nil {
			t.Errorf("%s: token %q", full, tok)
		}
	}
	if tok, err := c.OwnerToken(ctx, "MohammadBnei"); err != nil || tok != "fj-write" {
		t.Errorf("owner token %q, %v", tok, err)
	}
	if _, err := c.OwnerToken(ctx, "stranger"); err == nil {
		t.Error("a stranger's owner token")
	}
	env, _ := c.GitEnv(ctx)
	h := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("ukubi-fleet:fj-read"))
	want := []string{"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=http." + c.URL + "/MohammadBnei/.extraHeader", "GIT_CONFIG_VALUE_0=" + h,
		"GIT_CONFIG_KEY_1=http." + c.URL + "/mohammadbnei/.extraHeader", "GIT_CONFIG_VALUE_1=" + h}
	if fmt.Sprint(env) != fmt.Sprint(want) {
		t.Errorf("git env %v", env)
	}
	if !c.Configured() {
		t.Error("not configured with both tokens")
	}
	if err := os.Remove(filepath.Join(c.Dir, WriteTokenKey)); err != nil || c.Configured() {
		t.Errorf("configured without the write token (%v)", err)
	}
}

// Every page, to an empty one: a server capping limit below perPage answers
// short pages that are not the last. Archived, forks and mirrors are not
// fleet repos, and no repo at all is an error, not an empty list.
func TestListRepos_PagesAndFilters(t *testing.T) {
	c, f := setup(t)
	f.maxItems = 2
	with := func(full, k string) map[string]any { r := repoJSON(full, false); r[k] = true; return r }
	f.repos = append(f.repos, with("MohammadBnei/old", "archived"), with("MohammadBnei/forked", "fork"),
		with("MohammadBnei/mirrored", "mirror"), repoJSON("MohammadBnei/last", false))
	repos, err := c.ListRepos(context.Background())
	var got []string
	for _, r := range repos {
		got = append(got, r.FullName)
	}
	if err != nil || fmt.Sprint(got) != "[MohammadBnei/notes MohammadBnei/last]" {
		t.Errorf("repos %v, %v", got, err)
	}
	f.repos = f.repos[1:3] // pushable and a stranger's: nothing listable
	if repos, err := c.ListRepos(context.Background()); err == nil {
		t.Errorf("no fleet repo listed %v, no error", repos)
	}
}

func review(verdict string) forge.Review {
	return forge.Review{Owner: "MohammadBnei", Repo: "notes", PR: 7, HeadSHA: sha, Verdict: verdict, Body: "looks right",
		Comments: []forge.ReviewComment{{Path: "a.go", Line: 3, Body: "nit"}}}
}

// ADR-0082 §9 on Forgejo: ukubi-review formally approves ukubi-fleet's AGit
// PR and sets the status; a stranger's PR gets a COMMENT and no status; its
// own PR a COMMENT; a moved head or a closed PR gets nothing.
func TestPostReview(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, poster, verdict, state, head string
		event, status                      string
		err                                error
		edit                               func(map[string]any)
	}{
		{"fleet approve", "ukubi-fleet", "approve", "open", sha, "APPROVED", "success", nil, nil},
		{"fleet changes", "ukubi-fleet", "changes", "open", sha, "REQUEST_CHANGES", "failure", nil, nil},
		{"owner approve", "MohammadBnei", "approve", "open", sha, "APPROVED", "success", nil, nil},
		{"stranger", "someone", "approve", "open", sha, "COMMENT", "", nil, nil},
		{"its own", "ukubi-review", "approve", "open", sha, "COMMENT", "", nil, nil},
		{"moved head", "ukubi-fleet", "approve", "open", strings.Repeat("f", 40), "", "", forge.ErrStaleHead, nil},
		{"closed", "ukubi-fleet", "approve", "closed", sha, "", "", forge.ErrNotOpen, nil},
		// Not owned, though ukubi-fleet's: another base, a fork's head, a deleted fork's.
		{"another base", "ukubi-fleet", "approve", "open", sha, "COMMENT", "", nil, func(p map[string]any) { p["base"].(map[string]any)["ref"] = "deploy" }},
		{"fork head", "ukubi-fleet", "approve", "open", sha, "COMMENT", "", nil, func(p map[string]any) {
			p["head"].(map[string]any)["repo"] = map[string]any{"full_name": "ukubi-fleet/notes"}
		}},
		{"null head repo", "ukubi-fleet", "approve", "open", sha, "COMMENT", "", nil, func(p map[string]any) { p["head"].(map[string]any)["repo"] = nil }},
	} {
		c, f := setup(t)
		f.pulls["7"] = pr(tc.state, tc.poster, "main", tc.head)
		if tc.edit != nil {
			tc.edit(f.pulls["7"])
		}
		p, err := c.PostReview(ctx, review(tc.verdict))
		if tc.err != nil {
			if !errors.Is(err, tc.err) || len(f.posts) != 0 {
				t.Errorf("%s: %v, posted %v", tc.name, err, f.posts)
			}
			continue
		}
		if err != nil || p.Event != tc.event || p.Status != tc.status {
			t.Errorf("%s: %+v, %v", tc.name, p, err)
			continue
		}
		rv := f.posts["/repos/MohammadBnei/notes/pulls/7/reviews"]
		cs, _ := rv["comments"].([]any)
		if rv["event"] != tc.event || rv["commit_id"] != sha || len(cs) != 1 || cs[0].(map[string]any)["new_position"] != float64(3) {
			t.Errorf("%s: review %v", tc.name, rv)
		}
		st, set := f.posts["/repos/MohammadBnei/notes/statuses/"+sha]
		if set != (tc.status != "") || set && (st["state"] != tc.status || st["context"] != "agent-review") {
			t.Errorf("%s: status %v", tc.name, st)
		}
		for _, call := range f.calls {
			if strings.Contains(call, "/pulls/") && !strings.HasPrefix(call, "fj-review ") {
				t.Errorf("%s: %q not as ukubi-review", tc.name, call)
			}
		}
	}

	// A 422 on the inline comments: once more, folded into the body.
	c, f := setup(t)
	f.pulls["7"] = pr("open", "ukubi-fleet", "main", sha)
	f.failReviews = http.StatusUnprocessableEntity
	if _, err := c.PostReview(ctx, review("approve")); err != nil {
		t.Fatal(err)
	}
	rv := f.posts["/repos/MohammadBnei/notes/pulls/7/reviews"]
	if _, ok := rv["comments"]; ok || !strings.Contains(rv["body"].(string), "- `a.go:3`: nit") {
		t.Errorf("retry %v", rv)
	}
}

// With AGit the push opened the PR: OpenPR finds it by base and
// "ukubi-fleet/<topic>" and returns it only when it is open, ukubi-fleet's,
// on the default branch and at the approved commit.
func TestOpenPR(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, topic string
		pr          map[string]any
		ok          bool
		err         error
	}{
		{"ok", "fix-x", pr("open", "ukubi-fleet", "main", sha), true, nil},
		{"topic already prefixed", "ukubi-fleet/fix-x", pr("open", "ukubi-fleet", "main", sha), true, nil},
		{"someone else's", "fix-x", pr("open", "MohammadBnei", "main", sha), false, forge.ErrRefused},
		{"another base", "fix-x", pr("open", "ukubi-fleet", "deploy", sha), false, forge.ErrRefused},
		{"moved", "fix-x", pr("open", "ukubi-fleet", "main", strings.Repeat("f", 40)), false, forge.ErrStaleHead},
		{"merged", "fix-x", pr("closed", "ukubi-fleet", "main", sha), false, forge.ErrNotOpen},
	} {
		c, f := setup(t)
		f.pulls["main/ukubi-fleet/fix-x"] = tc.pr
		u, err := c.OpenPR(ctx, "MohammadBnei", "notes", tc.topic, sha, "t", "b")
		if tc.ok != (err == nil) || tc.ok && u != "https://git.test/MohammadBnei/notes/pulls/7" || tc.err != nil && !errors.Is(err, tc.err) {
			t.Errorf("%s: %q, %v", tc.name, u, err)
		}
		if len(f.posts) != 0 {
			t.Errorf("%s: wrote %v", tc.name, f.posts)
		}
	}
	c, _ := setup(t)
	var se *forge.StatusError
	if _, err := c.OpenPR(ctx, "MohammadBnei", "notes", "nothing-pushed", sha, "t", "b"); !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Errorf("no PR: %v", err)
	}
	if _, err := c.OpenPR(ctx, "stranger", "notes", "fix-x", sha, "t", "b"); err == nil {
		t.Error("a stranger's repo")
	}
}

func TestCreateIssue(t *testing.T) {
	c, f := setup(t)
	ctx := context.Background()
	u, err := c.CreateIssue(ctx, "MohammadBnei", "notes", "title", "body")
	if err != nil || u != "https://git.test/MohammadBnei/notes/issues/3" {
		t.Fatalf("%q, %v", u, err)
	}
	if got := f.posts["/repos/MohammadBnei/notes/issues"]; got["title"] != "title" || got["body"] != "body" {
		t.Errorf("issue %v", got)
	}
	if last := f.calls[len(f.calls)-1]; last != "fj-write POST /repos/MohammadBnei/notes/issues" {
		t.Errorf("last call %q", last)
	}
	if _, err := c.CreateIssue(ctx, "stranger", "notes", "t", "b"); err == nil {
		t.Error("an issue on a stranger's repo")
	}
}
