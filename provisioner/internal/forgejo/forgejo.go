// Package forgejo is the fleet's Forgejo forge (FORGE=forgejo; ADR-0086
// amendment D3): two bot users, not an App. ukubi-fleet is a read
// collaborator on each fleet repo and proposes through AGit; ukubi-review
// posts the reviews, the only agent approval that counts. Forgejo mints no
// per-repo token, so every token is a mounted file, read per call (a
// rotation needs no restart), and the repo list is the one boundary: a repo
// ukubi-fleet does not collaborate on, under an owner not allowlisted, gets
// nothing.
package forgejo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/forge"
)

// The bots, fixed by the owner's decision (build plan P2.1).
const (
	FleetLogin  = "ukubi-fleet"
	ReviewLogin = "ukubi-review"
)

// The token files in Dir. Read: ukubi-fleet's read:repository token (the
// provisioner's own git, listings, read-only vaults). Write: its
// write:repository (and write:issue) token, which a read collaborator can
// only AGit-push and open issues with. Review: ukubi-review's.
const (
	ReadTokenKey   = "FORGEJO_FLEET_READ_TOKEN"
	WriteTokenKey  = "FORGEJO_FLEET_WRITE_TOKEN"
	ReviewTokenKey = "FORGEJO_REVIEW_TOKEN"
)

const perPage = 50 // Forgejo's default MAX_RESPONSE_ITEMS; an admin may cap it lower

type Client struct {
	URL string // web root, FORGE_URL: https://git.bnei.dev
	Dir string // the mounted token files
	// Allow are the owners whose repos count (FORGEJO_OWNERS): anyone can
	// add ukubi-fleet as a collaborator on a repo of their own.
	Allow []string
	// Trusted are the logins whose PRs a review may formally approve or
	// request changes on (PROVISIONER_TRUSTED_LOGINS), besides ukubi-fleet.
	Trusted []string
	HTTP    *http.Client
}

func New(webURL, dir string, owners []string) *Client {
	return &Client{URL: strings.TrimSuffix(webURL, "/"), Dir: dir, Allow: owners, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

var _ forge.Forge = (*Client)(nil)

// Configured reports whether ukubi-fleet's two tokens are mounted.
func (c *Client) Configured() bool {
	for _, k := range []string{ReadTokenKey, WriteTokenKey} {
		if v, err := c.token(k); err != nil || v == "" {
			return false
		}
	}
	return true
}

func (c *Client) token(key string) (string, error) {
	b, err := os.ReadFile(filepath.Join(c.Dir, key))
	if err != nil {
		return "", fmt.Errorf("forgejo: read %s: %w", key, errors.Unwrap(err))
	}
	if v := strings.TrimSpace(string(b)); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("forgejo: %s is empty", key)
}

type repo struct {
	forge.Repo
	Mirror bool `json:"mirror"`
	Owner  struct {
		Login string `json:"login"`
	} `json:"owner"`
	Permissions struct {
		Admin bool `json:"admin"`
		Push  bool `json:"push"`
	} `json:"permissions"`
}

// ListRepos is the fleet's repo list: every repo ukubi-fleet collaborates on
// (GET /user/repos: owned and collaborative; it owns none) under an
// allowlisted owner, neither archived, a fork nor a mirror. A repo where it
// holds push or admin is refused and logged: the read role is what makes
// Forgejo refuse its branch and tag pushes (D3), so that repo has no such
// guard. All pages or an error, and never an empty list: either would read
// as removals. The listing ends at an empty page, not a short one: Forgejo
// caps limit at its MAX_RESPONSE_ITEMS, which may be below perPage.
func (c *Client) ListRepos(ctx context.Context) ([]forge.Repo, error) {
	tok, err := c.token(ReadTokenKey)
	if err != nil {
		return nil, err
	}
	var out []forge.Repo
	for page := 1; ; page++ {
		var batch []repo
		if err := c.call(ctx, tok, http.MethodGet, fmt.Sprintf("/user/repos?limit=%d&page=%d", perPage, page), nil, &batch); err != nil {
			return nil, err
		}
		for _, r := range batch {
			switch {
			case !slices.ContainsFunc(c.Allow, func(o string) bool { return strings.EqualFold(o, r.Owner.Login) }), r.Archived, r.Fork, r.Mirror:
			case r.Permissions.Admin || r.Permissions.Push:
				slog.Error("forgejo: "+FleetLogin+" can push to a repo, not listed: make it a read collaborator", "repo", r.FullName)
			default:
				out = append(out, r.Repo)
			}
		}
		if len(batch) == 0 {
			break
		}
	}
	if len(out) == 0 {
		return nil, errors.New("forgejo: " + FleetLogin + " collaborates on no repo of " + strings.Join(c.Allow, ", "))
	}
	return out, nil
}

// repo is owner/name from ListRepos, or an error: the check every call
// naming a repo makes, so nothing is written where ukubi-fleet is no
// collaborator.
//
// ponytail: one listing per call. Cache it if vault reconciles get slow.
func (c *Client) repo(ctx context.Context, owner, name string) (forge.Repo, error) {
	list, err := c.ListRepos(ctx)
	if err != nil {
		return forge.Repo{}, err
	}
	for _, r := range list {
		if strings.EqualFold(r.FullName, owner+"/"+name) {
			return r, nil
		}
	}
	return forge.Repo{}, fmt.Errorf("forgejo: %s/%s is not a fleet repo (%s is no collaborator, or the owner is not allowlisted)", owner, name, FleetLogin)
}

// Token is a definition vault's key for owner/repo: the read token when
// readOnly, else the write token.
func (c *Client) Token(ctx context.Context, owner, name string, readOnly bool) (string, error) {
	if _, err := c.repo(ctx, owner, name); err != nil {
		return "", err
	}
	if readOnly {
		return c.token(ReadTokenKey)
	}
	return c.token(WriteTokenKey)
}

// OwnerToken is a repo pods' token: the write token, for an allowlisted owner.
func (c *Client) OwnerToken(_ context.Context, owner string) (string, error) {
	if !slices.ContainsFunc(c.Allow, func(o string) bool { return strings.EqualFold(o, owner) }) {
		return "", fmt.Errorf("forgejo: owner %s is not allowlisted (FORGEJO_OWNERS)", owner)
	}
	return c.token(WriteTokenKey)
}

// Owners lists the allowlisted owners.
func (c *Client) Owners(context.Context) ([]string, error) { return slices.Clone(c.Allow), nil }

// GitEnv authenticates the provisioner's own git (repo cache, definitions,
// fleet-shared) with the read token: one http.<url>.extraHeader per
// allowlisted owner, as GIT_CONFIG_* env, never argv. Unmounted, it adds
// nothing.
func (c *Client) GitEnv(context.Context) ([]string, error) {
	tok, err := c.token(ReadTokenKey)
	if err != nil {
		return nil, nil
	}
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(FleetLogin+":"+tok))
	// git matches the URL path case-sensitively: each owner as written and
	// lowercased, as internal/github's GitEnv does.
	var owners []string
	for _, o := range c.Allow {
		owners = append(owners, slices.Compact([]string{o, strings.ToLower(o)})...)
	}
	env := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(owners))}
	for i, o := range owners {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=http.%s/%s/.extraHeader", i, c.URL, o), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, header))
	}
	return env, nil
}

// Forgejo's review events (its ReviewStateType), per ADR-0082 §9's verdicts.
var verdicts = map[string]struct{ event, label, status string }{
	"approve": {"APPROVED", "Approve", "success"},
	"changes": {"REQUEST_CHANGES", "Changes requested", "failure"},
	"comment": {"COMMENT", "Comment", ""},
}

type pull struct {
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"` // null once a fork is deleted
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

// PostReview posts r as ukubi-review, then the agent-review status on its
// head (ADR-0082 §9). The formal APPROVED/REQUEST_CHANGES goes only on an
// owned PR: by ukubi-fleet or a trusted login, its head in the same repo (an
// AGit PR's is), its base the default branch (an AGit push to
// refs/for/<other>/<topic> opens a ukubi-fleet PR on another base, and an
// agent merge targets the default branch only). Anything else, and ukubi-review's own PR (Forgejo refuses
// an author's approval), is a COMMENT, and only an owned PR gets the status.
// Unlike the GitHub App, the writer and the reviewer are two users, so
// ukubi-fleet's PRs take a formal approval: that approval is the one that
// counts. A status that fails after the review is logged, not returned.
func (c *Client) PostReview(ctx context.Context, r forge.Review) (forge.Posted, error) {
	v, ok := verdicts[r.Verdict]
	if !ok {
		return forge.Posted{}, fmt.Errorf("forgejo: verdict %q", r.Verdict)
	}
	fr, err := c.repo(ctx, r.Owner, r.Repo)
	if err != nil {
		return forge.Posted{}, err
	}
	tok, err := c.token(ReviewTokenKey)
	if err != nil {
		return forge.Posted{}, err
	}
	full := r.Owner + "/" + r.Repo
	base := "/repos/" + full + "/pulls/" + strconv.FormatInt(r.PR, 10)
	var pr pull
	if err := c.call(ctx, tok, http.MethodGet, base, nil, &pr); err != nil {
		return forge.Posted{}, err
	}
	if pr.State != "open" {
		return forge.Posted{}, fmt.Errorf("%s#%d is %s: %w", full, r.PR, pr.State, forge.ErrNotOpen)
	}
	if !strings.EqualFold(pr.Head.SHA, r.HeadSHA) {
		return forge.Posted{}, fmt.Errorf("%s#%d head is %s, reviewed %s: %w", full, r.PR, pr.Head.SHA, r.HeadSHA, forge.ErrStaleHead)
	}
	author := pr.User.Login
	trusted := strings.EqualFold(author, FleetLogin) || slices.ContainsFunc(c.Trusted, func(l string) bool { return strings.EqualFold(l, author) })
	owned := trusted && pr.Head.Repo != nil && strings.EqualFold(pr.Head.Repo.FullName, pr.Base.Repo.FullName) && pr.Base.Ref == fr.DefaultBranch
	event := v.event
	if !owned || strings.EqualFold(author, ReviewLogin) {
		event = "COMMENT"
	}

	body := "Verdict: " + v.label + "\n\n" + r.Body
	post := map[string]any{"commit_id": r.HeadSHA, "body": body, "event": event}
	if len(r.Comments) > 0 {
		var cs []map[string]any
		for _, cm := range r.Comments {
			cs = append(cs, map[string]any{"path": cm.Path, "body": cm.Body, "new_position": cm.Line})
		}
		post["comments"] = cs
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	err = c.call(ctx, tok, http.MethodPost, base+"/reviews", post, &out)
	var se *forge.StatusError
	if errors.As(err, &se) && se.Status == http.StatusUnprocessableEntity && len(r.Comments) > 0 {
		// Usually a line outside the diff: once more, the comments in the body.
		delete(post, "comments")
		post["body"] = forge.FoldComments(body, "Forgejo", r.Comments)
		err = c.call(ctx, tok, http.MethodPost, base+"/reviews", post, &out)
	}
	if err != nil {
		return forge.Posted{}, err
	}
	status := v.status
	if !owned {
		status = ""
	}
	if status != "" {
		st := map[string]string{"state": status, "context": "agent-review", "description": "Agent review: " + v.label, "target_url": out.HTMLURL}
		if err := c.call(ctx, tok, http.MethodPost, "/repos/"+full+"/statuses/"+r.HeadSHA, st, nil); err != nil {
			slog.Warn("forgejo: review posted, agent-review status not set", "review", out.HTMLURL, "err", err)
			status = ""
		}
	}
	return forge.Posted{URL: out.HTMLURL, Event: event, Status: status}, nil
}

// OpenPR returns the AGit PR for topic branch (docs/adr/0086 §4 open_pr).
// The session's `git push origin HEAD:refs/for/<default>/<topic>` already
// opened it, so nothing is opened here: the PR must exist, be open, be
// ukubi-fleet's, target the default branch (read here, never the caller's:
// an agent PR never targets another branch) and sit at the approved commit,
// or it is refused. title and body are the card's; the push set the PR's.
//
// Forgejo names an AGit PR's head "<pusher>/<topic>" unless the topic
// already starts with "<pusher>/" (services/agit), and the base/head lookup
// matches that name.
//
// ponytail: the lookup returns one PR; with a closed PR and an open one on
// the same topic it may return the closed one, refused as not open. Push to
// a fresh topic.
func (c *Client) OpenPR(ctx context.Context, owner, name, branch, headSHA, _, _ string) (string, error) {
	r, err := c.repo(ctx, owner, name)
	if err != nil {
		return "", err
	}
	tok, err := c.token(ReadTokenKey)
	if err != nil {
		return "", err
	}
	head := branch
	if !strings.HasPrefix(head, FleetLogin+"/") {
		head = FleetLogin + "/" + head
	}
	full := owner + "/" + name
	var pr pull
	if err := c.call(ctx, tok, http.MethodGet, "/repos/"+full+"/pulls/"+url.PathEscape(r.DefaultBranch)+"/"+escapeRef(head), nil, &pr); err != nil {
		return "", err
	}
	switch {
	case pr.State != "open":
		return "", fmt.Errorf("%s topic %s: the PR is %s: %w", full, branch, pr.State, forge.ErrNotOpen)
	case !strings.EqualFold(pr.User.Login, FleetLogin):
		return "", fmt.Errorf("%s topic %s: the PR is %s's, not %s's: %w", full, branch, pr.User.Login, FleetLogin, forge.ErrRefused)
	case pr.Base.Ref != r.DefaultBranch || !strings.EqualFold(pr.Base.Repo.FullName, full):
		return "", fmt.Errorf("%s topic %s: the PR targets %s %s, not %s: %w", full, branch, pr.Base.Repo.FullName, pr.Base.Ref, r.DefaultBranch, forge.ErrRefused)
	case !strings.EqualFold(pr.Head.SHA, headSHA):
		return "", fmt.Errorf("%s topic %s is at %s, approved %s: %w", full, branch, pr.Head.SHA, headSHA, forge.ErrStaleHead)
	}
	return pr.HTMLURL, nil
}

// escapeRef path-escapes each segment of a branch, keeping its "/" separators.
func escapeRef(b string) string {
	parts := strings.Split(b, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// CreateIssue opens an issue in owner/repo as ukubi-fleet (docs/adr/0084 §6
// open_issue) and returns its URL. core has checked the definition owns the
// repo; it must be a fleet repo too.
func (c *Client) CreateIssue(ctx context.Context, owner, name, title, body string) (string, error) {
	if _, err := c.repo(ctx, owner, name); err != nil {
		return "", err
	}
	tok, err := c.token(WriteTokenKey)
	if err != nil {
		return "", err
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if err := c.call(ctx, tok, http.MethodPost, "/repos/"+owner+"/"+name+"/issues", map[string]any{"title": title, "body": body}, &out); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

// call is one Forgejo API request. Errors carry Forgejo's message and the
// path without its query, never the request body or the token.
func (c *Client) call(ctx context.Context, tok, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+"/api/v1"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+tok)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	p := strings.SplitN(path, "?", 2)[0]
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &e)
		return &forge.StatusError{Status: resp.StatusCode, Msg: fmt.Sprintf("forgejo: %s %s: %d %s", method, p, resp.StatusCode, e.Message)}
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("forgejo: %s %s: decode: %w", method, p, err)
		}
	}
	return nil
}
