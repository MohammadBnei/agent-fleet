package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// App is the fleet's GitHub identity, the App ukubi-fleet (docs/adr/0075,
// infra-bootstrap ADR-0062). It mints installation tokens for one repo at a
// time. Its id and private key are files in Dir (Infisical project
// fleet-github-app), read on every JWT: never env, never argv.
type App struct {
	API string
	Dir string
	// Accounts are the account ids whose installations count. The App is
	// public, so anyone can install it; every other installation is ignored.
	Accounts map[int64]bool
	HTTP     *http.Client

	now   func() time.Time
	mu    sync.Mutex
	cache map[string]token // owner/repo -> last minted token
}

type token struct {
	value   string
	expires time.Time
}

// refreshBefore: a cached token with less left than this is re-minted. Tokens
// last 1h, so a healthy token is never older than 30m when the vault gets it.
const refreshBefore = 30 * time.Minute

// definitionPermissions is what a definition pod's token may do: push and
// open PRs on its one repo. Never workflows (docs/adr/0075).
var definitionPermissions = map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"}

func NewApp(dir string, accounts []int64) *App {
	m := map[int64]bool{}
	for _, id := range accounts {
		m[id] = true
	}
	return &App{
		API: "https://api.github.com", Dir: dir, Accounts: m,
		HTTP: &http.Client{Timeout: 10 * time.Second}, now: time.Now, cache: map[string]token{},
	}
}

// Configured reports whether the App's id and key are mounted.
func (a *App) Configured() bool {
	for _, k := range []string{"GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY"} {
		if b, err := os.ReadFile(filepath.Join(a.Dir, k)); err != nil || len(bytes.TrimSpace(b)) == 0 {
			return false
		}
	}
	return true
}

// Token returns an installation token for owner/repo, minting one when none
// is cached or the cached one has less than refreshBefore left. A failed
// mint returns the last token, even an expired one: the caller pushes an
// unchanged value instead of losing a service. Only a repo never minted has
// no fallback.
func (a *App) Token(ctx context.Context, owner, repo string) (string, error) {
	key := strings.ToLower(owner + "/" + repo)
	a.mu.Lock()
	defer a.mu.Unlock()
	last, ok := a.cache[key]
	if ok && a.now().Add(refreshBefore).Before(last.expires) {
		return last.value, nil
	}
	t, err := a.mint(ctx, owner, repo)
	if err != nil {
		if ok {
			slog.Error("github app: re-mint failed, serving the last token", "repo", key, "expires", last.expires, "error", err)
			return last.value, nil
		}
		return "", err
	}
	a.cache[key] = t
	return t.value, nil
}

func (a *App) mint(ctx context.Context, owner, repo string) (token, error) {
	jwt, err := a.jwt()
	if err != nil {
		return token{}, err
	}
	id, err := a.installation(ctx, jwt, owner)
	if err != nil {
		return token{}, err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	body := map[string]any{"repositories": []string{repo}, "permissions": definitionPermissions}
	if err := a.call(ctx, jwt, http.MethodPost, "/app/installations/"+strconv.FormatInt(id, 10)+"/access_tokens", body, &out); err != nil {
		return token{}, err
	}
	if out.Token == "" {
		return token{}, fmt.Errorf("github app: mint for %s/%s returned no token", owner, repo)
	}
	return token{value: out.Token, expires: out.ExpiresAt}, nil
}

// installation finds owner's installation among the allowlisted accounts.
// Matched on the account id, never the login alone: a login can be renamed
// and re-registered by someone else.
//
// ponytail: one page of 100 installations; only allowlisted ones matter and
// there are two.
func (a *App) installation(ctx context.Context, jwt, owner string) (int64, error) {
	var list []struct {
		ID      int64 `json:"id"`
		Account struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := a.call(ctx, jwt, http.MethodGet, "/app/installations?per_page=100", nil, &list); err != nil {
		return 0, err
	}
	for _, in := range list {
		if a.Accounts[in.Account.ID] && strings.EqualFold(in.Account.Login, owner) {
			return in.ID, nil
		}
	}
	return 0, fmt.Errorf("github app: no installation for %s among the allowlisted accounts", owner)
}

// jwt signs the App's RS256 JWT: iat backdated 60s for clock skew, exp 9m
// out (GitHub's ceiling is 10m).
func (a *App) jwt() (string, error) {
	id, err := os.ReadFile(filepath.Join(a.Dir, "GITHUB_APP_ID"))
	if err != nil {
		return "", fmt.Errorf("github app: read GITHUB_APP_ID: %w", errors.Unwrap(err))
	}
	raw, err := os.ReadFile(filepath.Join(a.Dir, "GITHUB_APP_PRIVATE_KEY"))
	if err != nil {
		return "", fmt.Errorf("github app: read GITHUB_APP_PRIVATE_KEY: %w", errors.Unwrap(err))
	}
	key, err := parseKey(raw)
	if err != nil {
		return "", err
	}
	now := a.now()
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": strings.TrimSpace(string(id))})
	signed := head + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("github app: sign JWT: %w", err)
	}
	return signed + "." + enc.EncodeToString(sig), nil
}

// parseKey accepts PKCS#1 (what GitHub hands out) and PKCS#8. Errors never
// quote the key.
func parseKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("github app: GITHUB_APP_PRIVATE_KEY is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("github app: GITHUB_APP_PRIVATE_KEY is neither a PKCS#1 nor a PKCS#8 RSA key")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app: GITHUB_APP_PRIVATE_KEY is not an RSA key")
	}
	return rk, nil
}

// call is do() with the App JWT. Error text carries GitHub's message, never
// the request body or a token.
func (a *App) call(ctx context.Context, jwt, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.API+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &e)
		return fmt.Errorf("github app: %s %s: %d %s", method, strings.SplitN(path, "?", 2)[0], resp.StatusCode, e.Message)
	}
	return json.Unmarshal(b, out)
}
