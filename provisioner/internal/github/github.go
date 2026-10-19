// Package github lists the repos argocd-ukubi-bot can reach and accepts its
// pending invitations (docs/adr/0074). The bot's collaborator list IS the
// fleet's repo list; core projects it into the repos table.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const perPage = 100

// Repo is one repo the bot is a collaborator on.
type Repo struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
}

type Client struct {
	API   string
	Token string
	// Inviters are the GitHub logins whose invitations are auto-accepted.
	// Anyone else's stays pending for a human: never declined.
	Inviters map[string]bool
	HTTP     *http.Client
}

func New(token string, inviters []string) *Client {
	m := map[string]bool{}
	for _, l := range inviters {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			m[l] = true
		}
	}
	return &Client{API: "https://api.github.com", Token: token, Inviters: m, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// AcceptInvites accepts every pending invitation from an allowlisted inviter
// and returns the full names it accepted. One failed accept is logged and
// skipped: the next tick retries it.
func (c *Client) AcceptInvites(ctx context.Context) ([]string, error) {
	var invites []struct {
		ID      int64 `json:"id"`
		Inviter struct {
			Login string `json:"login"`
		} `json:"inviter"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := c.do(ctx, http.MethodGet, "/user/repository_invitations?per_page=100", &invites); err != nil {
		return nil, err
	}
	var accepted []string
	for _, inv := range invites {
		if !c.Inviters[strings.ToLower(inv.Inviter.Login)] {
			slog.Warn("github: invitation left pending, inviter not in FLEET_REPO_INVITERS", "repo", inv.Repository.FullName, "inviter", inv.Inviter.Login)
			continue
		}
		if err := c.do(ctx, http.MethodPatch, "/user/repository_invitations/"+strconv.FormatInt(inv.ID, 10), nil); err != nil {
			slog.Error("github: accept invitation", "repo", inv.Repository.FullName, "error", err)
			continue
		}
		slog.Info("github: invitation accepted", "repo", inv.Repository.FullName, "inviter", inv.Inviter.Login)
		accepted = append(accepted, inv.Repository.FullName)
	}
	return accepted, nil
}

// ListRepos returns every non-archived, non-fork repo the bot collaborates
// on. All pages or an error: a partial list would read as removals
// downstream. organization_member is left out because org membership must not
// imply fleet membership, and owner because the bot's token sits in repo
// worker pods: a session could create a bot-owned repo and enlist it.
//
// ponytail: offset pages, so a repo added between two page reads can shift
// out of one listing; it reappears next tick. Follow the Link header if the
// bot ever reaches more than 100 repos.
func (c *Client) ListRepos(ctx context.Context) ([]Repo, error) {
	var out []Repo
	for page := 1; ; page++ {
		var batch []Repo
		path := fmt.Sprintf("/user/repos?affiliation=collaborator&per_page=%d&page=%d", perPage, page)
		if err := c.do(ctx, http.MethodGet, path, &batch); err != nil {
			return nil, err
		}
		for _, r := range batch {
			if !r.Archived && !r.Fork {
				out = append(out, r)
			}
		}
		if len(batch) < perPage {
			return out, nil
		}
	}
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	if c.Token == "" {
		return fmt.Errorf("github: GH_TOKEN is not set")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.API+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &e)
		return fmt.Errorf("github: %s %s: %d %s", method, path, resp.StatusCode, e.Message)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("github: %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}
