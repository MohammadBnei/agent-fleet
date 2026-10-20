// Package vault is the provisioner's Agent Vault client (docs/adr/0065). It
// holds the fleet's only vault credential, a member-role agent token, and
// reads every credential value from the provisioner's own mounted Secret:
// core sends key names, never values.
package vault

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// TokenKey is the agent token's file in SecretsDir.
const TokenKey = "AGENT_VAULT_TOKEN"

// gitUsernameKey holds "x-access-token" for basic git auth. FLEET_VAULT_ is
// the provisioner's own prefix: a definition may never name one.
const gitUsernameKey = "FLEET_VAULT_GIT_USERNAME"

// revokeBudget bounds every revoke: best-effort, the token's TTL bounds a miss.
const revokeBudget = 3 * time.Second

// MinTTL is the vault's floor on a session token's TTL, and so a definition
// pod's shortest lifetime.
const MinTTL = 300 * time.Second

type Client struct {
	API        string // http://agent-vault.agent-vault.svc.cluster.local:14321
	Proxy      string // agent-vault.agent-vault.svc.cluster.local:14322
	SecretsDir string // the mounted Secret: AGENT_VAULT_TOKEN + credential values
	HTTP       *http.Client
	// Forge, when set, supplies each vault's GitHub key: a token minted for
	// the vault's one repo (docs/adr/0075 §5). Nil reads it from SecretsDir.
	Forge Forge

	mu    sync.Mutex
	set   map[string]map[string][32]byte // vault -> key -> sha256 of the value last written
	locks map[string]*sync.Mutex
}

func New(api, proxy, secretsDir string) *Client {
	return &Client{
		API: strings.TrimSuffix(api, "/"), Proxy: proxy, SecretsDir: secretsDir,
		HTTP: &http.Client{Timeout: 10 * time.Second},
		set:  map[string]map[string][32]byte{}, locks: map[string]*sync.Mutex{},
	}
}

// Forge mints repo-scoped credentials. GitHub's App is the one
// implementation; Forgejo is the planned second (docs/adr/0075 §2).
type Forge interface {
	Token(ctx context.Context, owner, repo string) (string, error)
}

// Spec is one vault's desired state (a definition's credentials).
type Spec struct {
	Name     string
	Services []Service
}

type Service struct {
	Host   string
	Paths  []string
	Secret string
}

// service and auth are Agent Vault's wire shapes (broker.Service, v0.40.0:
// host is "host/path" joined both ways: ingest splits it, broker.Service's
// MarshalJSON rejoins it on GET; credential fields are KEY names).
type service struct {
	Name string `json:"name"`
	Host string `json:"host"`
	Auth auth   `json:"auth"`
}

type auth struct {
	Type     string `json:"type"`
	Token    string `json:"token,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// gitAuth is basic x-access-token:<PAT>. Picked by a wire check
// (docs/adr/0065 §2, 2026-10-19): github.com's info/refs?service=
// git-receive-pack answered 200 to Basic and 401 to Bearer with the same
// valid token, and git ls-remote went through a v0.40.0 vault with this shape.
// TestReconcile_CreatesVaultServicesPolicyCredentials pins the shape.
func gitAuth(key string) auth {
	return auth{Type: "basic", Username: gitUsernameKey, Password: key}
}

// The trust boundary: core's spec is re-checked here, so a buggy or
// compromised core cannot point a credential at another host, another repo
// or another vault's key. Ported from core/internal/definitions (separate
// module): anchored, exactly one trailing "/*", no "." / ".." segment (the
// vault matches by prefix, unnormalised), one <owner>/<repo> per vault, and
// a GitHub key bound to its vault by name (GitHubKey).
var (
	vaultNameRe = regexp.MustCompile(`^[a-z0-9-]{3,64}$`)
	keyRe       = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	githubPath  = regexp.MustCompile(`^/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(\.git)?/\*$`)
	apiPath     = regexp.MustCompile(`^/repos/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/\*$`)
	referenceRe = regexp.MustCompile(`^\$\{.*\}$`)
)

// GitHubKey is the only secret key a vault's GitHub services may name:
// "GH_" + the vault name upper-snaked (dev-notes -> GH_DEV_NOTES). Core's
// validator enforces the same, so core cannot hand one vault another's PAT.
func GitHubKey(vault string) string {
	return "GH_" + strings.ToUpper(strings.ReplaceAll(vault, "-", "_"))
}

func checkSpec(s Spec) error {
	if !vaultNameRe.MatchString(s.Name) || strings.HasPrefix(s.Name, "executor-") {
		return fmt.Errorf("vault name %q is not a definition vault slug", s.Name)
	}
	repos := map[string]bool{}
	for _, svc := range s.Services {
		if !keyRe.MatchString(svc.Secret) || strings.HasPrefix(svc.Secret, "AGENT_VAULT_") || strings.HasPrefix(svc.Secret, "FLEET_VAULT_") || strings.HasPrefix(svc.Secret, "GITHUB_APP_") {
			return fmt.Errorf("vault %s: secret %q is not a definition credential key", s.Name, svc.Secret)
		}
		re := githubPath
		switch svc.Host {
		case "api.anthropic.com":
			if len(svc.Paths) > 0 {
				return fmt.Errorf("vault %s: api.anthropic.com takes no paths", s.Name)
			}
			if strings.HasPrefix(svc.Secret, "GH_") {
				return fmt.Errorf("vault %s: api.anthropic.com cannot take a GitHub key (%s)", s.Name, svc.Secret)
			}
			continue
		case "api.github.com":
			re = apiPath
		case "github.com":
		default:
			return fmt.Errorf("vault %s: host %q is not allowed", s.Name, svc.Host)
		}
		if svc.Secret != GitHubKey(s.Name) {
			return fmt.Errorf("vault %s: %s takes secret %s only, not %s", s.Name, svc.Host, GitHubKey(s.Name), svc.Secret)
		}
		if len(svc.Paths) == 0 {
			return fmt.Errorf("vault %s: %s needs a path", s.Name, svc.Host)
		}
		for _, p := range svc.Paths {
			seg := strings.Split(p, "/")
			if !re.MatchString(p) || slices.Contains(seg, ".") || slices.Contains(seg, "..") {
				return fmt.Errorf("vault %s: path %q on %s is not an exact repo glob", s.Name, p, svc.Host)
			}
			if svc.Host == "api.github.com" {
				seg = seg[1:] // "", "repos", owner, repo, "*"
			}
			repos[seg[1]+"/"+strings.TrimSuffix(seg[2], ".git")] = true // "", owner, repo[.git], "*"
		}
	}
	if len(repos) > 1 {
		return fmt.Errorf("vault %s: GitHub paths name %d repos, want one <owner>/<repo>", s.Name, len(repos))
	}
	return nil
}

// render turns a spec into the vault's services and the credential keys they
// use. The username key is the provisioner's own. Every host is pinned to
// :443: a service without a port matches any port, and the vault's plain
// http:// forward path injects too, so http://github.com/... would carry the
// PAT in cleartext (docs/adr/0065 accepted risks).
func render(s Spec) []service {
	var out []service
	for _, svc := range s.Services {
		switch svc.Host {
		case "api.anthropic.com":
			out = append(out, service{Name: "anthropic", Host: svc.Host + ":443", Auth: auth{Type: "bearer", Token: svc.Secret}})
		case "github.com":
			for i, p := range svc.Paths {
				out = append(out, service{Name: fmt.Sprintf("github-%d", i), Host: svc.Host + ":443" + p, Auth: gitAuth(svc.Secret)})
			}
		case "api.github.com":
			for i, p := range svc.Paths {
				out = append(out, service{Name: fmt.Sprintf("github-api-%d", i), Host: svc.Host + ":443" + p, Auth: auth{Type: "bearer", Token: svc.Secret}})
			}
		}
	}
	return out
}

func (a auth) keys() []string {
	var out []string
	for _, k := range []string{a.Token, a.Username, a.Password} {
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

// readSecret returns a credential value from the mounted Secret, "" if absent
// or empty. Errors name the key only.
func (c *Client) readSecret(key string) (string, error) {
	b, err := os.ReadFile(filepath.Join(c.SecretsDir, key))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read secret %s: %w", key, errors.Unwrap(err))
	}
	return strings.TrimSpace(string(b)), nil
}

// Lock takes vault name's lock and returns its unlock. Every Reconcile runs
// under it, and so does whatever must not interleave with a widening: a warm
// holds it across reconcile, mint and Job create, a widening across revoke-all,
// PUT and ending the definition's pods (docs/adr/0065 §3). Not re-entrant.
func (c *Client) Lock(name string) (unlock func()) {
	c.mu.Lock()
	l, ok := c.locks[name]
	if !ok {
		l = &sync.Mutex{}
		c.locks[name] = l
	}
	c.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Reconcile makes vault s.Name match s: services (a PUT replaces the set, so
// a service removed from git is removed), credentials, and
// unmatched_host_policy=deny. A service whose secret is missing is dropped
// (fail closed) and the call errors naming the key. Unchanged inputs cost
// three GETs and no write (checked against a real v0.40.0:
// vault_live_test.go). A credential is rewritten when its Infisical value
// changes or after a provisioner restart (the memo is in-process); a value
// changed in the vault itself is not detected until then. A WIDENED service
// set first revokes every session on the vault, so a pod minted under the
// narrower spec (an older pin) loses access instead of gaining it
// (docs/adr/0065 §3); widened reports it, so the caller ends those pods.
// Widened compares (host, auth) pairs, not service names: render names by
// position, so dropping or reordering a path is a narrowing. The caller holds
// Lock(s.Name).
func (c *Client) Reconcile(ctx context.Context, s Spec) (widened bool, err error) {
	if err := checkSpec(s); err != nil {
		return false, err
	}
	err = c.reconcile(ctx, s, &widened)
	return widened, err
}

func (c *Client) reconcile(ctx context.Context, s Spec, widened *bool) error {
	values := map[string]string{gitUsernameKey: "x-access-token"}
	// Every value is gathered before the first write. A forge error returns
	// here, so it can never narrow the services the way a missing key does
	// (docs/adr/0075 §5).
	if c.Forge != nil {
		if owner, repo, ok := specRepo(s); ok {
			v, err := c.Forge.Token(ctx, owner, repo)
			if err != nil {
				return fmt.Errorf("vault %s: mint %s/%s: %w", s.Name, owner, repo, err)
			}
			values[GitHubKey(s.Name)] = v
		}
	}
	var missing []string
	var desired []service
	for _, svc := range render(s) {
		ok := true
		for _, k := range svc.Auth.keys() {
			if _, seen := values[k]; seen {
				continue
			}
			v, err := c.readSecret(k)
			if err != nil {
				return fmt.Errorf("vault %s: %w", s.Name, err)
			}
			if referenceRe.MatchString(v) {
				return fmt.Errorf("vault %s: secret %s holds an unexpanded Infisical reference", s.Name, k)
			}
			if v == "" {
				ok = false
				if !slices.Contains(missing, k) {
					missing = append(missing, k)
				}
				continue
			}
			values[k] = v
		}
		if ok {
			desired = append(desired, svc)
		}
	}
	want := map[string]string{}
	for _, svc := range desired {
		for _, k := range svc.Auth.keys() {
			want[k] = values[k]
		}
	}
	sort.Slice(desired, func(i, j int) bool { return desired[i].Name < desired[j].Name })

	var current struct {
		Services []service `json:"services"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/vaults/"+s.Name+"/services", nil, &current)
	switch {
	case isStatus(err, http.StatusNotFound) && len(desired) == 0:
		return missingErr(s.Name, missing)
	case isStatus(err, http.StatusNotFound):
		if err := c.do(ctx, http.MethodPost, "/v1/vaults", map[string]string{"name": s.Name}, nil); err != nil && !isStatus(err, http.StatusConflict) {
			return fmt.Errorf("vault %s: create: %w", s.Name, err)
		}
	case err != nil:
		return fmt.Errorf("vault %s: services: %w", s.Name, err)
	}

	var listed struct {
		Keys []string `json:"keys"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/credentials?vault="+url.QueryEscape(s.Name), nil, &listed); err != nil {
		return fmt.Errorf("vault %s: credentials: %w", s.Name, err)
	}
	c.mu.Lock()
	memo := c.set[s.Name]
	if memo == nil {
		memo = map[string][32]byte{}
		c.set[s.Name] = memo
	}
	changed := map[string]string{}
	for k, v := range want {
		if sum := sha256.Sum256([]byte(v)); !slices.Contains(listed.Keys, k) || memo[k] != sum {
			changed[k] = v
		}
	}
	c.mu.Unlock()
	if len(changed) > 0 {
		if err := c.do(ctx, http.MethodPost, "/v1/credentials", map[string]any{"vault": s.Name, "credentials": changed}, nil); err != nil {
			// The request carried values: the vault's own text is not
			// forwarded, in case it ever echoes one.
			return fmt.Errorf("vault %s: set credentials: %w", s.Name, statusOnly(err))
		}
		c.mu.Lock()
		for k, v := range changed {
			memo[k] = sha256.Sum256([]byte(v))
		}
		c.mu.Unlock()
		slog.Info("vault: credentials set", "vault", s.Name, "keys", sortedKeys(changed))
	}

	sort.Slice(current.Services, func(i, j int) bool { return current.Services[i].Name < current.Services[j].Name })
	if !slices.Equal(current.Services, desired) {
		if slices.ContainsFunc(desired, func(d service) bool {
			return !slices.ContainsFunc(current.Services, func(c service) bool { return c.Host == d.Host && c.Auth == d.Auth })
		}) {
			if _, err := c.revokeWhere(ctx, s.Name, func(string) bool { return true }); err != nil {
				return fmt.Errorf("vault %s: widening, so revoking its sessions first: %w", s.Name, err)
			}
			*widened = true
		}
		if desired == nil {
			desired = []service{}
		}
		if err := c.do(ctx, http.MethodPut, "/v1/vaults/"+s.Name+"/services", map[string]any{"services": desired}, nil); err != nil {
			return fmt.Errorf("vault %s: set services: %w", s.Name, err)
		}
		slog.Info("vault: services set", "vault", s.Name, "count", len(desired))
	}

	var settings struct {
		Policy string `json:"unmatched_host_policy"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/vaults/"+s.Name+"/settings", nil, &settings); err != nil {
		return fmt.Errorf("vault %s: settings: %w", s.Name, err)
	}
	if settings.Policy != "deny" {
		if err := c.do(ctx, http.MethodPatch, "/v1/vaults/"+s.Name+"/settings", map[string]string{"unmatched_host_policy": "deny"}, nil); err != nil {
			return fmt.Errorf("vault %s: deny policy: %w", s.Name, err)
		}
	}

	// After the PUT, so no service still names a deleted key.
	var stale []string
	for _, k := range listed.Keys {
		if _, ok := want[k]; !ok {
			stale = append(stale, k)
		}
	}
	if len(stale) > 0 {
		if err := c.do(ctx, http.MethodDelete, "/v1/credentials", map[string]any{"vault": s.Name, "keys": stale}, nil); err != nil {
			return fmt.Errorf("vault %s: delete credentials: %w", s.Name, err)
		}
		c.mu.Lock()
		for _, k := range stale {
			delete(memo, k)
		}
		c.mu.Unlock()
		slog.Info("vault: credentials removed", "vault", s.Name, "keys", stale)
	}
	return missingErr(s.Name, missing)
}

// specRepo is the one <owner>/<repo> a checked spec's GitHub paths name.
func specRepo(s Spec) (owner, repo string, ok bool) {
	for _, svc := range s.Services {
		if len(svc.Paths) == 0 || (svc.Host != "github.com" && svc.Host != "api.github.com") {
			continue
		}
		seg := strings.Split(svc.Paths[0], "/") // "", owner, repo[.git], "*"
		if svc.Host == "api.github.com" {
			seg = seg[1:] // "", "repos", owner, repo, "*"
		}
		return seg[1], strings.TrimSuffix(seg[2], ".git"), true
	}
	return "", "", false
}

func missingErr(vault string, missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("vault %s: secret %s not set in Infisical agent-vault /agent-vault", vault, strings.Join(missing, ", "))
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (c *Client) vaultNames(ctx context.Context) ([]string, error) {
	var list struct {
		Vaults []struct {
			Name string `json:"name"`
		} `json:"vaults"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/vaults", nil, &list); err != nil {
		return nil, fmt.Errorf("list vaults: %w", err)
	}
	out := make([]string, 0, len(list.Vaults))
	for _, v := range list.Vaults {
		out = append(out, v.Name)
	}
	return out, nil
}

// Prune empties every vault this agent administers that is not in keep and
// not "executor-"-prefixed (reserved for the bridge executor, ADR 0072). The
// vault itself stays: harmless, and cheap to re-fill. Only for the FULL set.
func (c *Client) Prune(ctx context.Context, keep map[string]bool) error {
	names, err := c.vaultNames(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range names {
		if keep[n] || strings.HasPrefix(n, "executor-") || !vaultNameRe.MatchString(n) {
			continue
		}
		unlock := c.Lock(n)
		_, err := c.Reconcile(ctx, Spec{Name: n}) // an empty set never widens
		unlock()
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Mint issues a proxy-role session token on vault, labelled label, for at
// least MinTTL. Generic: the bridge executor reuses it.
func (c *Client) Mint(ctx context.Context, vault, label string, ttl time.Duration) (string, error) {
	ttl = max(ttl, MinTTL)
	var out struct {
		Token string `json:"token"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/sessions", map[string]any{"vault": vault, "ttl_seconds": int(ttl.Seconds()), "label": label}, &out); err != nil {
		return "", fmt.Errorf("vault %s: mint: %w", vault, err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("vault %s: mint returned no token", vault)
	}
	return out.Token, nil
}

// Revoke deletes the sessions on vault labelled exactly label (the mint
// response has no id, so: list, then delete). A missing vault is nothing to
// revoke. Own budget, off the caller's cancellation. Generic: the bridge
// executor reuses it.
func (c *Client) Revoke(ctx context.Context, vault, label string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeBudget)
	defer cancel()
	n, err := c.revokeWhere(ctx, vault, func(l string) bool { return l == label })
	if err == nil {
		slog.Info("vault: revoked", "vault", vault, "label", label, "revoked", n)
	}
	return err
}

// revokeWhere returns how many sessions it deleted.
func (c *Client) revokeWhere(ctx context.Context, vault string, match func(label string) bool) (int, error) {
	var list struct {
		Sessions []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"sessions"`
	}
	q := "?vault=" + url.QueryEscape(vault)
	err := c.do(ctx, http.MethodGet, "/v1/sessions"+q, nil, &list)
	if isStatus(err, http.StatusNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("vault %s: list sessions: %w", vault, err)
	}
	var errs []error
	n := 0
	for _, s := range list.Sessions {
		if !match(s.Label) {
			continue
		}
		if err := c.do(ctx, http.MethodDelete, "/v1/sessions/"+url.PathEscape(s.ID)+q, nil, nil); err != nil && !isStatus(err, http.StatusNotFound) {
			errs = append(errs, fmt.Errorf("vault %s: revoke %s: %w", vault, s.Label, err))
		} else {
			n++
		}
	}
	return n, errors.Join(errs...)
}

// CA is the vault's MITM root certificate (public).
func (c *Client) CA(ctx context.Context) ([]byte, error) {
	var buf bytes.Buffer
	if err := c.do(ctx, http.MethodGet, "/v1/mitm/ca.pem", nil, &buf); err != nil {
		return nil, fmt.Errorf("vault CA: %w", err)
	}
	return buf.Bytes(), nil
}

// ProxyURL is a definition pod's HTTPS_PROXY.
func (c *Client) ProxyURL(token, vault string) string {
	return "http://" + url.UserPassword(token, vault).String() + "@" + c.Proxy
}

type statusError struct {
	Status int
	Msg    string
}

func (e *statusError) Error() string { return strings.TrimSpace(fmt.Sprintf("%d %s", e.Status, e.Msg)) }

// statusOnly drops the vault's own error text, keeping the status.
func statusOnly(err error) error {
	var se *statusError
	if errors.As(err, &se) {
		return &statusError{Status: se.Status}
	}
	return err
}

func isStatus(err error, code int) bool {
	var se *statusError
	return errors.As(err, &se) && se.Status == code
}

// do sends one API call with the agent token, read per call (kubelet
// refreshes the mounted Secret, so a rotation needs no restart). out is JSON,
// or raw bytes into a *bytes.Buffer. Errors carry the status and the vault's
// own `error` text, never a request body.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	token, err := c.readSecret(TokenKey)
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("vault: " + TokenKey + " is not set (run gitops/scripts/agent-vault-bootstrap.sh in infra-bootstrap)")
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.API+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
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
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		return &statusError{Status: resp.StatusCode, Msg: e.Error}
	}
	switch o := out.(type) {
	case nil:
		return nil
	case *bytes.Buffer:
		o.Write(b)
		return nil
	default:
		return json.Unmarshal(b, out)
	}
}
