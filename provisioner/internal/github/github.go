// Package github is the fleet's GitHub forge: the App ukubi-fleet
// (docs/adr/0075). Its allowlisted installations ARE the fleet's repo list
// (amends docs/adr/0074); core projects it into the repos table.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/forge"
)

const perPage = 100

// The forge's shared types, under the names this package always had.
type (
	Repo          = forge.Repo
	Review        = forge.Review
	ReviewComment = forge.ReviewComment
	Posted        = forge.Posted
	StatusError   = forge.StatusError
)

var (
	ErrNotOpen   = forge.ErrNotOpen
	ErrStaleHead = forge.ErrStaleHead
)

// request is one GitHub REST call. Errors carry GitHub's message and the
// path without its query, never the request body or the bearer.
func request(ctx context.Context, hc *http.Client, api, bearer, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, api+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := hc.Do(req)
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
		return &StatusError{Status: resp.StatusCode, Msg: fmt.Sprintf("github: %s %s: %d %s", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode, e.Message)}
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("github: %s %s: decode: %w", method, strings.SplitN(path, "?", 2)[0], err)
		}
	}
	return nil
}
