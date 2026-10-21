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
	cache map[string]token // "owner/repo" or "install:<id>" -> last minted token

	instMu  sync.Mutex
	insts   []installation // allowlisted installations, cached for instTTL
	instsAt time.Time
}

type installation struct {
	ID      int64 `json:"id"`
	Account struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	} `json:"account"`
}

// instTTL: how long the installation list is reused. Installing the App on a
// new account is rare; a new repo under an existing installation needs no
// refresh of this list.
const instTTL = 10 * time.Minute

// installationPermissions is what the provisioner's own git and the repos
// listing need from an owner-wide token: read only.
var installationPermissions = map[string]string{"contents": "read", "metadata": "read"}

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
// re-mint returns the last token while it is still valid; an expired one is
// an error, never passed on as if it worked.
//
// ponytail: one lock across the mint's network calls, so a slow GitHub
// serialises every vault's mint (10s timeouts). Per-repo locks if warms stall.
func (a *App) Token(ctx context.Context, owner, repo string) (string, error) {
	return a.cached(ctx, strings.ToLower(owner+"/"+repo), func(ctx context.Context) (token, error) {
		return a.mint(ctx, owner, repo)
	})
}

// cached serves key's token while it has refreshBefore left, else mints. A
// failed re-mint returns the last token while it is still valid.
func (a *App) cached(ctx context.Context, key string, mint func(context.Context) (token, error)) (string, error) {
	if !a.Configured() {
		return "", fmt.Errorf("github app: not mounted at %s", a.Dir)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	last, ok := a.cache[key]
	if ok && a.now().Add(refreshBefore).Before(last.expires) {
		return last.value, nil
	}
	t, err := mint(ctx)
	if err != nil {
		if ok && a.now().Before(last.expires) {
			slog.Error("github app: re-mint failed, serving the last token", "key", key, "expires", last.expires, "error", err)
			return last.value, nil
		}
		return "", err
	}
	a.cache[key] = t
	return t.value, nil
}

// installations lists the App's installations by allowlisted accounts, all
// pages, reused for instTTL. Others are logged and skipped: the App is
// public, and only a human removes a foreign installation (infra-bootstrap
// ADR-0062).
func (a *App) installations(ctx context.Context) ([]installation, error) {
	a.instMu.Lock()
	defer a.instMu.Unlock()
	if a.insts != nil && a.now().Before(a.instsAt.Add(instTTL)) {
		return a.insts, nil
	}
	jwt, err := a.jwt()
	if err != nil {
		return nil, err
	}
	var out []installation
	for page := 1; ; page++ {
		var batch []installation
		if err := a.call(ctx, jwt, http.MethodGet, fmt.Sprintf("/app/installations?per_page=%d&page=%d", perPage, page), nil, &batch); err != nil {
			return nil, err
		}
		for _, in := range batch {
			if a.Accounts[in.Account.ID] {
				out = append(out, in)
			} else {
				slog.Error("github app: installation by a non-allowlisted account, ignored; remove it by hand (infra-bootstrap ADR-0062)", "account", in.Account.Login, "accountId", in.Account.ID, "installation", in.ID)
			}
		}
		if len(batch) < perPage {
			break
		}
	}
	a.insts, a.instsAt = out, a.now()
	return out, nil
}

// installationToken is a read-only token over everything installation in
// covers: the provisioner's own git and the repos listing.
func (a *App) installationToken(ctx context.Context, in installation) (string, error) {
	return a.cached(ctx, "install:"+strconv.FormatInt(in.ID, 10), func(ctx context.Context) (token, error) {
		jwt, err := a.jwt()
		if err != nil {
			return token{}, err
		}
		return a.accessToken(ctx, jwt, in.ID, map[string]any{"permissions": installationPermissions})
	})
}

// ListRepos is the fleet's repo list (docs/adr/0075 §4): every non-archived,
// non-fork repo of every allowlisted installation. All pages of all
// installations or an error: a partial list would read as removals.
func (a *App) ListRepos(ctx context.Context) ([]Repo, error) {
	insts, err := a.installations(ctx)
	if err != nil {
		return nil, err
	}
	var out []Repo
	for _, in := range insts {
		tok, err := a.installationToken(ctx, in)
		if err != nil {
			return nil, err
		}
		for page := 1; ; page++ {
			var batch struct {
				Repositories []Repo `json:"repositories"`
			}
			if err := request(ctx, a.HTTP, a.API, tok, http.MethodGet, fmt.Sprintf("/installation/repositories?per_page=%d&page=%d", perPage, page), nil, &batch); err != nil {
				return nil, err
			}
			for _, r := range batch.Repositories {
				if !r.Archived && !r.Fork {
					out = append(out, r)
				}
			}
			if len(batch.Repositories) < perPage {
				break
			}
		}
	}
	return out, nil
}

// GitEnv authenticates the provisioner's own git (repo cache, definitions,
// fleet-shared) per invocation: one http.<url>.extraHeader per allowlisted
// owner, as GIT_CONFIG_* env, never argv (errors quote argv). Read-only.
func (a *App) GitEnv(ctx context.Context) ([]string, error) {
	insts, err := a.installations(ctx)
	if err != nil {
		return nil, err
	}
	env := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(insts))}
	for i, in := range insts {
		tok, err := a.installationToken(ctx, in)
		if err != nil {
			return nil, err
		}
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=http.https://github.com/%s/.extraHeader", i, in.Account.Login),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=Authorization: Basic %s", i, basic))
	}
	return env, nil
}

func (a *App) mint(ctx context.Context, owner, repo string) (token, error) {
	jwt, err := a.jwt()
	if err != nil {
		return token{}, err
	}
	id, err := a.installation(ctx, jwt, owner, repo)
	if err != nil {
		return token{}, err
	}
	return a.accessToken(ctx, jwt, id, map[string]any{"repositories": []string{repo}, "permissions": definitionPermissions})
}

func (a *App) accessToken(ctx context.Context, jwt string, id int64, body map[string]any) (token, error) {
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.call(ctx, jwt, http.MethodPost, "/app/installations/"+strconv.FormatInt(id, 10)+"/access_tokens", body, &out); err != nil {
		return token{}, err
	}
	if out.Token == "" {
		return token{}, fmt.Errorf("github app: installation %d returned no token", id)
	}
	return token{value: out.Token, expires: out.ExpiresAt}, nil
}

// installation is the installation covering owner/repo, if its account is
// allowlisted. Matched on the account id, never the login: the App is
// public, and a login can be renamed and re-registered by someone else.
func (a *App) installation(ctx context.Context, jwt, owner, repo string) (int64, error) {
	var in struct {
		ID      int64 `json:"id"`
		Account struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := a.call(ctx, jwt, http.MethodGet, "/repos/"+owner+"/"+repo+"/installation", nil, &in); err != nil {
		return 0, err
	}
	if !a.Accounts[in.Account.ID] {
		slog.Error("github app: installation by a non-allowlisted account, ignored; remove it by hand (infra-bootstrap ADR-0062)", "repo", owner+"/"+repo, "account", in.Account.Login, "accountId", in.Account.ID, "installation", in.ID)
		return 0, fmt.Errorf("github app: %s/%s is installed by account %d, not an allowlisted one", owner, repo, in.Account.ID)
	}
	return in.ID, nil
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

func (a *App) call(ctx context.Context, jwt, method, path string, in, out any) error {
	return request(ctx, a.HTTP, a.API, jwt, method, path, in, out)
}
