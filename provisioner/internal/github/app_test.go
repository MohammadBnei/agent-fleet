package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub serves /app/installations and access_tokens, records each mint.
type fakeGitHub struct {
	t       *testing.T
	pub     *rsa.PublicKey
	mu      sync.Mutex
	mints   []map[string]any
	expires time.Time
	fail    bool
	// tokenInstall maps a minted token to the installation it was minted on.
	tokenInstall map[string]string
	instLists    int
	failPage2    bool // /installation/repositories page 2 answers 502
	suspendOpus  bool
}

func (f *fakeGitHub) serveInstallationRepos(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	inst := f.tokenInstall[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	f.mu.Unlock()
	var repos []map[string]any
	switch {
	case inst == "/app/installations/2/access_tokens" && r.URL.Query().Get("page") == "1":
		for i := range perPage {
			repos = append(repos, map[string]any{"id": i + 1, "name": fmt.Sprintf("r%d", i), "full_name": fmt.Sprintf("MohammadBnei/r%d", i), "archived": i == 0, "fork": i == 1})
		}
	case inst == "/app/installations/2/access_tokens" && r.URL.Query().Get("page") == "2" && f.failPage2:
		http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
		return
	case inst == "/app/installations/2/access_tokens" && r.URL.Query().Get("page") == "2":
		repos = []map[string]any{{"id": 999, "name": "last", "full_name": "MohammadBnei/last"}}
	case inst == "/app/installations/3/access_tokens":
		repos = []map[string]any{{"id": 1424343192, "name": "workspace", "full_name": "opus-tech-hq/workspace"}}
	case inst == "":
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"repositories": repos})
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/installation/repositories" {
		f.serveInstallationRepos(w, r)
		return
	}
	f.checkJWT(r.Header.Get("Authorization"))
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation"):
		switch strings.ToLower(r.URL.Path) {
		case "/repos/mohammadbnei/notes/installation":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2, "account": map[string]any{"id": 43962695, "login": "MohammadBnei"}})
		case "/repos/mohammadbnei/squatted/installation": // same login, foreign account id
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "account": map[string]any{"id": 666, "login": "MohammadBnei"}})
		case "/repos/opus-tech-hq/workspace/installation", "/repos/opus-tech-hq/other/installation":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 3, "account": map[string]any{"id": 344349211, "login": "opus-tech-hq"}})
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
		f.mu.Lock()
		f.instLists++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "account": map[string]any{"id": 666, "login": "MohammadBnei"}}, // foreign, same login
			{"id": 2, "account": map[string]any{"id": 43962695, "login": "MohammadBnei"}},
			opus(f.suspendOpus),
		})
		return
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/"):
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["path"] = r.URL.Path
		f.mints = append(f.mints, body)
		tok := fmt.Sprintf("ghs_%d", len(f.mints))
		if f.tokenInstall == nil {
			f.tokenInstall = map[string]string{}
		}
		f.tokenInstall[tok] = r.URL.Path
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "expires_at": f.expires.Format(time.RFC3339)})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) checkJWT(h string) {
	parts := strings.Split(strings.TrimPrefix(h, "Bearer "), ".")
	if len(parts) != 3 {
		f.t.Errorf("not a JWT: %q", h)
		return
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.pub, crypto.SHA256, sum[:], sig); err != nil {
		f.t.Errorf("JWT signature: %v", err)
	}
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(b, &claims)
	// iat backdated, exp within GitHub's 10m window (relative: tests shift now).
	if claims.Iss != "5395733" || claims.Exp-claims.Iat > 660 || claims.Exp-claims.Iat < 540 {
		f.t.Errorf("claims %+v", claims)
	}
}

func newApp(t *testing.T, pkcs8 bool) (*App, *fakeGitHub) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, typ := x509.MarshalPKCS1PrivateKey(key), "RSA PRIVATE KEY"
	if pkcs8 {
		der, _ = x509.MarshalPKCS8PrivateKey(key)
		typ = "PRIVATE KEY"
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "GITHUB_APP_ID"), []byte("5395733\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "GITHUB_APP_PRIVATE_KEY"), pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600)
	f := &fakeGitHub{t: t, pub: &key.PublicKey, expires: time.Now().Add(time.Hour)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	a := NewApp(dir, []int64{43962695, 344349211})
	a.API = srv.URL
	return a, f
}

func TestApp_MintsOneRepoTokenFromTheAllowlistedInstallation(t *testing.T) {
	for _, pkcs8 := range []bool{false, true} {
		a, f := newApp(t, pkcs8)
		tok, err := a.Token(context.Background(), "mohammadbnei", "notes")
		if err != nil || tok == "" {
			t.Fatalf("pkcs8=%v: %q %v", pkcs8, tok, err)
		}
		m := f.mints[0]
		if m["path"] != "/app/installations/2/access_tokens" {
			t.Errorf("minted on %v, want the allowlisted installation 2", m["path"])
		}
		if repos, _ := m["repositories"].([]any); len(repos) != 1 || repos[0] != "notes" {
			t.Errorf("repositories %v", m["repositories"])
		}
		perms, _ := m["permissions"].(map[string]any)
		if perms["contents"] != "write" || perms["pull_requests"] != "write" || perms["metadata"] != "read" || perms["workflows"] != nil || len(perms) != 3 {
			t.Errorf("permissions %v", perms)
		}
	}
}

func TestApp_RefusesAnInstallationByANonAllowlistedAccount(t *testing.T) {
	a, f := newApp(t, false)
	if _, err := a.Token(context.Background(), "MohammadBnei", "squatted"); err == nil {
		t.Fatal("want an error: the repo's installation is by a foreign account id")
	}
	if len(f.mints) != 0 {
		t.Errorf("minted %d tokens", len(f.mints))
	}
}

func TestApp_ReusesUntilThirtyMinutesRemain(t *testing.T) {
	a, f := newApp(t, false)
	ctx := context.Background()
	t1, _ := a.Token(ctx, "opus-tech-hq", "workspace")
	t2, _ := a.Token(ctx, "opus-tech-hq", "workspace")
	if t1 != t2 || len(f.mints) != 1 {
		t.Fatalf("a fresh token was re-minted: %d mints", len(f.mints))
	}
	a.now = func() time.Time { return time.Now().Add(31 * time.Minute) }
	t3, _ := a.Token(ctx, "opus-tech-hq", "workspace")
	if t3 == t1 || len(f.mints) != 2 {
		t.Fatalf("a token with <30m left was served: %d mints", len(f.mints))
	}
}

// A failed re-mint serves the last token, even expired: the vault keeps what
// it has instead of losing a service (docs/adr/0075 §5).
func TestApp_FailedRemintServesTheLastToken(t *testing.T) {
	a, f := newApp(t, false)
	ctx := context.Background()
	t1, _ := a.Token(ctx, "opus-tech-hq", "workspace")
	f.fail = true
	a.now = func() time.Time { return time.Now().Add(45 * time.Minute) } // <30m left, still valid
	t2, err := a.Token(ctx, "opus-tech-hq", "workspace")
	if err != nil || t2 != t1 {
		t.Fatalf("got %q %v, want the last token", t2, err)
	}
	a.now = func() time.Time { return time.Now().Add(2 * time.Hour) } // expired
	if _, err := a.Token(ctx, "opus-tech-hq", "workspace"); err == nil {
		t.Fatal("an expired token was served as valid")
	}
	if _, err := a.Token(ctx, "opus-tech-hq", "other"); err == nil {
		t.Fatal("no token yet and the mint fails: want an error")
	}
}

func TestApp_ConfiguredOnlyWithBothFiles(t *testing.T) {
	if NewApp(t.TempDir(), nil).Configured() {
		t.Error("empty dir reads as configured")
	}
	a, _ := newApp(t, false)
	if !a.Configured() {
		t.Error("both files present, not configured")
	}
}

// docs/adr/0075 §4: the fleet's repos are the allowlisted installations'
// repos, all pages, never archived or forks; a foreign installation with the
// same login contributes nothing.
func TestApp_ListReposFromAllowlistedInstallations(t *testing.T) {
	a, f := newApp(t, false)
	repos, err := a.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range repos {
		got[r.FullName] = true
	}
	if len(repos) != perPage-2+2 || !got["MohammadBnei/last"] || !got["opus-tech-hq/workspace"] || got["MohammadBnei/r0"] || got["MohammadBnei/r1"] {
		t.Fatalf("repos %d: %v", len(repos), got)
	}
	for _, m := range f.mints {
		if m["path"] == "/app/installations/1/access_tokens" {
			t.Error("minted on the foreign installation")
		}
		if m["repositories"] != nil {
			t.Errorf("listing token narrowed to repositories: %v", m)
		}
		if perms, _ := m["permissions"].(map[string]any); perms["contents"] != "read" || perms["metadata"] != "read" || len(perms) != 2 {
			t.Errorf("listing token permissions %v, want read-only", perms)
		}
	}
	if _, err := a.ListRepos(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.instLists != 1 {
		t.Errorf("installations listed %d times within instTTL", f.instLists)
	}
}

// The provisioner's own git gets one read-only extraHeader per allowlisted
// owner, as env, never argv.
func TestApp_GitEnvPerOwner(t *testing.T) {
	a, _ := newApp(t, false)
	env, err := a.GitEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=http.https://github.com/MohammadBnei/.extraHeader",
		"GIT_CONFIG_KEY_1=http.https://github.com/mohammadbnei/.extraHeader",
		"GIT_CONFIG_KEY_2=http.https://github.com/opus-tech-hq/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q:\n%s", want, joined)
		}
	}
}

func opus(suspended bool) map[string]any {
	in := map[string]any{"id": 3, "account": map[string]any{"id": 344349211, "login": "opus-tech-hq"}}
	if suspended {
		in["suspended_at"] = "2026-10-21T00:00:00Z"
	}
	return in
}

// A failed later page is an error, never a short list: core would read the
// missing repos as removed.
func TestApp_ListReposPartialIsError(t *testing.T) {
	a, f := newApp(t, false)
	f.failPage2 = true
	if repos, err := a.ListRepos(context.Background()); err == nil {
		t.Fatalf("partial listing returned %d repos and no error", len(repos))
	}
}

// No allowlisted installation is an error, never an empty fleet.
func TestApp_NoAllowlistedInstallationIsError(t *testing.T) {
	a, _ := newApp(t, false)
	a.Accounts = map[int64]bool{}
	if _, err := a.ListRepos(context.Background()); err == nil {
		t.Fatal("empty allowlist listed as an empty fleet")
	}
}

// A suspended installation is skipped, so the other one still syncs.
func TestApp_SuspendedInstallationIsSkipped(t *testing.T) {
	a, f := newApp(t, false)
	f.suspendOpus = true
	repos, err := a.ListRepos(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range repos {
		if r.FullName == "opus-tech-hq/workspace" {
			t.Fatal("a suspended installation's repo was listed")
		}
	}
	if len(repos) == 0 {
		t.Fatal("the healthy installation's repos were lost too")
	}
}

// Unmounted, GitEnv adds nothing and calls nothing.
func TestApp_GitEnvUnmountedIsNoop(t *testing.T) {
	env, err := NewApp(t.TempDir(), []int64{1}).GitEnv(context.Background())
	if err != nil || env != nil {
		t.Fatalf("env %v err %v", env, err)
	}
}

// docs/adr/0075 §7: a repo pods' token covers the owner's whole allowlisted
// installation, never the foreign one with the same login, never workflows.
func TestApp_OwnerTokenIsInstallationWide(t *testing.T) {
	a, f := newApp(t, false)
	tok, err := a.OwnerToken(context.Background(), "mohammadbnei")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := a.OwnerToken(context.Background(), "MohammadBnei"); again != tok || len(f.mints) != 1 {
		t.Fatalf("not cached: %q then %q, %d mints", tok, again, len(f.mints))
	}
	m := f.mints[0]
	if m["path"] != "/app/installations/2/access_tokens" || m["repositories"] != nil {
		t.Errorf("mint %v, want installation 2 unnarrowed", m)
	}
	perms, _ := m["permissions"].(map[string]any)
	if perms["contents"] != "write" || perms["issues"] != "write" || perms["checks"] != "read" || perms["workflows"] != nil {
		t.Errorf("permissions %v", perms)
	}
	if _, err := a.OwnerToken(context.Background(), "someone-else"); err == nil {
		t.Error("a non-allowlisted owner got a token")
	}
	owners, err := a.Owners(context.Background())
	if err != nil || strings.Join(owners, ",") != "MohammadBnei,opus-tech-hq" {
		t.Errorf("owners %v %v", owners, err)
	}
}
