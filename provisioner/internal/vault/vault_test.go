package vault_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/vault"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/vault/vaulttest"
)

// setup returns a client on a fake vault with the agent token and the given
// secret files in its secrets dir.
func setup(t *testing.T, secrets map[string]string) (*vault.Client, *vaulttest.Server, string) {
	t.Helper()
	f := vaulttest.New(t)
	dir := t.TempDir()
	writeSecrets(t, dir, map[string]string{"AGENT_VAULT_TOKEN": vaulttest.Token})
	writeSecrets(t, dir, secrets)
	return vault.New(f.URL, "agent-vault.agent-vault.svc.cluster.local:14322", dir), f, dir
}

func writeSecrets(t *testing.T, dir string, m map[string]string) {
	t.Helper()
	for k, v := range m {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func notesSpec() vault.Spec {
	return vault.Spec{Name: "dev-notes", Services: []vault.Service{
		{Host: "github.com", Paths: []string{"/MohammadBnei/notes/*", "/MohammadBnei/notes.git/*"}, Secret: "GH_DEV_NOTES"},
		{Host: "api.github.com", Paths: []string{"/repos/MohammadBnei/notes/*"}, Secret: "GH_DEV_NOTES"},
		{Host: "api.anthropic.com", Secret: "CLAUDE_CODE_OAUTH_TOKEN"},
	}}
}

var notesSecrets = map[string]string{"GH_DEV_NOTES": "ghp-sentinel-value", "CLAUDE_CODE_OAUTH_TOKEN": "oauth-sentinel-value"}

type svc struct {
	Name string `json:"name"`
	Host string `json:"host"`
	Auth struct {
		Type     string `json:"type"`
		Token    string `json:"token,omitempty"`
		Username string `json:"username,omitempty"`
		Password string `json:"password,omitempty"`
	} `json:"auth"`
}

func services(t *testing.T, f *vaulttest.Server, name string) []svc {
	t.Helper()
	v, ok := f.Get(name)
	if !ok {
		t.Fatalf("no vault %s", name)
	}
	var out []svc
	if err := json.Unmarshal(v.Services, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReconcile_CreatesVaultServicesPolicyCredentials(t *testing.T) {
	c, f, _ := setup(t, notesSecrets)
	if _, err := c.Reconcile(context.Background(), notesSpec()); err != nil {
		t.Fatal(err)
	}
	v, _ := f.Get("dev-notes")
	if v.Policy != "deny" {
		t.Errorf("policy %q", v.Policy)
	}
	want := map[string]string{"GH_DEV_NOTES": "ghp-sentinel-value", "CLAUDE_CODE_OAUTH_TOKEN": "oauth-sentinel-value", "FLEET_VAULT_GIT_USERNAME": "x-access-token"}
	if len(v.Creds) != len(want) {
		t.Errorf("%d creds, want %d", len(v.Creds), len(want))
	}
	for k, x := range want {
		if v.Creds[k] != x {
			t.Errorf("cred %s not set to its file's value", k)
		}
	}
	got := map[string]svc{}
	for _, s := range services(t, f, "dev-notes") {
		got[s.Host] = s
	}
	for host, want := range map[string]string{
		"api.anthropic.com:443":                         "bearer/CLAUDE_CODE_OAUTH_TOKEN",
		"github.com:443/MohammadBnei/notes/*":           "basic/FLEET_VAULT_GIT_USERNAME/GH_DEV_NOTES",
		"github.com:443/MohammadBnei/notes.git/*":       "basic/FLEET_VAULT_GIT_USERNAME/GH_DEV_NOTES",
		"api.github.com:443/repos/MohammadBnei/notes/*": "bearer/GH_DEV_NOTES",
	} {
		s, ok := got[host]
		if !ok {
			t.Errorf("no service for %s (got %v)", host, got)
			continue
		}
		var shape string
		if s.Auth.Type == "bearer" {
			shape = "bearer/" + s.Auth.Token
		} else {
			shape = s.Auth.Type + "/" + s.Auth.Username + "/" + s.Auth.Password
		}
		if shape != want {
			t.Errorf("%s auth %s, want %s", host, shape, want)
		}
	}
	if len(got) != 4 {
		t.Errorf("%d services, want 4", len(got))
	}
}

func TestReconcile_Idempotent(t *testing.T) {
	c, f, _ := setup(t, notesSecrets)
	ctx := context.Background()
	if _, err := c.Reconcile(ctx, notesSpec()); err != nil {
		t.Fatal(err)
	}
	f.ResetLog()
	if _, err := c.Reconcile(ctx, notesSpec()); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("an unchanged reconcile wrote: %v", w)
	}
	if calls := f.Calls(); len(calls) != 3 {
		t.Errorf("an unchanged reconcile made %d calls, want 3 GETs: %v", len(calls), calls)
	}
}

func TestReconcile_RotatedValueRewritten(t *testing.T) {
	c, f, dir := setup(t, notesSecrets)
	ctx := context.Background()
	if _, err := c.Reconcile(ctx, notesSpec()); err != nil {
		t.Fatal(err)
	}
	writeSecrets(t, dir, map[string]string{"GH_DEV_NOTES": "rotated"})
	f.ResetLog()
	if _, err := c.Reconcile(ctx, notesSpec()); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != 1 || w[0] != "POST /v1/credentials" {
		t.Errorf("writes %v, want one credentials POST", w)
	}
	if v, _ := f.Get("dev-notes"); v.Creds["GH_DEV_NOTES"] != "rotated" {
		t.Error("rotated value not written")
	}
}

func TestReconcile_RemovesServicesNotInGit(t *testing.T) {
	c, f, _ := setup(t, notesSecrets)
	ctx := context.Background()
	if _, err := c.Reconcile(ctx, notesSpec()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reconcile(ctx, vault.Spec{Name: "dev-notes", Services: notesSpec().Services[2:]}); err != nil {
		t.Fatal(err)
	}
	if s := services(t, f, "dev-notes"); len(s) != 1 || s[0].Host != "api.anthropic.com:443" {
		t.Errorf("services %v, want anthropic only", s)
	}
	if v, _ := f.Get("dev-notes"); len(v.Creds) != 1 || v.Creds["CLAUDE_CODE_OAUTH_TOKEN"] == "" {
		t.Errorf("creds not narrowed: %d left", len(v.Creds))
	}
	if _, err := c.Reconcile(ctx, vault.Spec{Name: "dev-notes"}); err != nil {
		t.Fatal(err)
	}
	if s := services(t, f, "dev-notes"); len(s) != 0 {
		t.Errorf("empty spec left services %v", s)
	}
	if v, _ := f.Get("dev-notes"); len(v.Creds) != 0 {
		t.Errorf("empty spec left %d creds", len(v.Creds))
	}
}

func TestReconcile_EmptySpecAbsentVault_NoOp(t *testing.T) {
	c, f, _ := setup(t, nil)
	if _, err := c.Reconcile(context.Background(), vault.Spec{Name: "gone-one"}); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %v", w)
	}
}

func TestReconcile_MissingSecret_FailsClosed(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c, f, _ := setup(t, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oauth-sentinel-value"})
	_, err := c.Reconcile(context.Background(), notesSpec())
	if err == nil || !strings.Contains(err.Error(), "GH_DEV_NOTES") || !strings.Contains(err.Error(), "/agent-vault") {
		t.Fatalf("got %v, want an error naming GH_DEV_NOTES", err)
	}
	for _, s := range services(t, f, "dev-notes") {
		if s.Host != "api.anthropic.com:443" {
			t.Errorf("a service with a missing secret was kept: %s", s.Host)
		}
	}
	if strings.Contains(err.Error(), "sentinel") || strings.Contains(logs.String(), "sentinel") {
		t.Error("a credential value reached an error or a log line")
	}
}

func TestReconcile_RefusesOffListHostOrPath(t *testing.T) {
	for name, spec := range map[string]vault.Spec{
		"host":           {Name: "dev-notes", Services: []vault.Service{{Host: "attacker.example", Secret: "CLAUDE_CODE_OAUTH_TOKEN"}}},
		"dot segment":    {Name: "dev-notes", Services: []vault.Service{{Host: "github.com", Paths: []string{"/MohammadBnei/../x/*"}, Secret: "GH_DEV_NOTES"}}},
		"glob":           {Name: "dev-notes", Services: []vault.Service{{Host: "github.com", Paths: []string{"/MohammadBnei/notes*"}, Secret: "GH_DEV_NOTES"}}},
		"anthropic path": {Name: "dev-notes", Services: []vault.Service{{Host: "api.anthropic.com", Paths: []string{"/v1/*"}, Secret: "CLAUDE_CODE_OAUTH_TOKEN"}}},
		"api path":       {Name: "dev-notes", Services: []vault.Service{{Host: "api.github.com", Paths: []string{"/user/*"}, Secret: "GH_DEV_NOTES"}}},
		"vault name":     {Name: "Bad_Name", Services: notesSpec().Services},
		"executor vault": {Name: "executor-dev-notes", Services: notesSpec().Services[2:]},
		"agent token":    {Name: "dev-notes", Services: []vault.Service{{Host: "api.anthropic.com", Secret: "AGENT_VAULT_TOKEN"}}},
		"own key":        {Name: "dev-notes", Services: []vault.Service{{Host: "api.anthropic.com", Secret: "FLEET_VAULT_GIT_USERNAME"}}},
		"key shape":      {Name: "dev-notes", Services: []vault.Service{{Host: "api.anthropic.com", Secret: "../etc/passwd"}}},
		"another's PAT":  {Name: "dev-notes", Services: []vault.Service{{Host: "github.com", Paths: []string{"/MohammadBnei/notes/*"}, Secret: "GH_OPUS_WORKSPACE"}}},
		"model key":      {Name: "dev-notes", Services: []vault.Service{{Host: "github.com", Paths: []string{"/MohammadBnei/notes/*"}, Secret: "CLAUDE_CODE_OAUTH_TOKEN"}}},
		"PAT to model":   {Name: "dev-notes", Services: []vault.Service{{Host: "api.anthropic.com", Secret: "GH_DEV_NOTES"}}},
		"two repos": {Name: "dev-notes", Services: []vault.Service{
			{Host: "github.com", Paths: []string{"/MohammadBnei/notes/*"}, Secret: "GH_DEV_NOTES"},
			{Host: "api.github.com", Paths: []string{"/repos/MohammadBnei/agent-fleet/*"}, Secret: "GH_DEV_NOTES"},
		}},
		// An owner starting "repos" is not an api.github.com path.
		"two repos-owners": {Name: "dev-notes", Services: []vault.Service{
			{Host: "github.com", Paths: []string{"/reposx/y/*", "/reposz/y/*"}, Secret: "GH_DEV_NOTES"},
		}},
	} {
		c, f, _ := setup(t, notesSecrets)
		if _, err := c.Reconcile(context.Background(), spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if w := f.Writes(); len(w) != 0 {
			t.Errorf("%s: wrote %v before refusing", name, w)
		}
	}
}

func TestReconcile_RefusesUnexpandedReference(t *testing.T) {
	c, f, _ := setup(t, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "${dev.CLAUDE_CODE_OAUTH_TOKEN}"})
	_, err := c.Reconcile(context.Background(), vault.Spec{Name: "dev-x", Services: []vault.Service{{Host: "api.anthropic.com", Secret: "CLAUDE_CODE_OAUTH_TOKEN"}}})
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN") || !strings.Contains(err.Error(), "unexpanded") {
		t.Fatalf("got %v", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("wrote %v", w)
	}
}

// A 409 on create (another replica made it first) is success. The per-vault
// lock is the server's to hold: grpcserver's TestCreateWorkerPod_WarmHoldsVaultLock.
func TestReconcile_CreateConflictIsSuccess(t *testing.T) {
	c, f, _ := setup(t, notesSecrets)
	f.Configure(func(f *vaulttest.Server) { f.ConflictOnCreate = true })
	if _, err := c.Reconcile(context.Background(), notesSpec()); err != nil {
		t.Fatal(err)
	}
	if n := f.Names(); len(n) != 1 {
		t.Errorf("vaults %v", n)
	}
}

func TestClient_MissingAgentToken(t *testing.T) {
	f := vaulttest.New(t)
	c := vault.New(f.URL, "x:14322", t.TempDir())
	_, err := c.Reconcile(context.Background(), notesSpec())
	if err == nil || !strings.Contains(err.Error(), "agent-vault-bootstrap.sh") {
		t.Fatalf("got %v", err)
	}
}

func TestPrune_EmptiesAbsentKeepsExecutor(t *testing.T) {
	c, f, _ := setup(t, notesSecrets)
	ctx := context.Background()
	for _, n := range []string{"keep-me", "drop-me"} {
		s := vault.Spec{Name: n, Services: notesSpec().Services[2:]}
		if _, err := c.Reconcile(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	// The bridge executor's vault: written by its own code, never this path.
	f.Configure(func(f *vaulttest.Server) {
		f.Vaults["executor-x"] = &vaulttest.Vault{Services: json.RawMessage(`[{"name":"x","host":"h"}]`), Creds: map[string]string{"K": "v"}}
	})
	if err := c.Prune(ctx, map[string]bool{"keep-me": true}); err != nil {
		t.Fatal(err)
	}
	for n, want := range map[string]int{"keep-me": 1, "drop-me": 0, "executor-x": 1} {
		if got := len(services(t, f, n)); got != want {
			t.Errorf("%s: %d services, want %d", n, got, want)
		}
		if v, _ := f.Get(n); (len(v.Creds) == 0) != (want == 0) {
			t.Errorf("%s: %d creds", n, len(v.Creds))
		}
	}
}

func TestMint_TTLFloor(t *testing.T) {
	c, f, _ := setup(t, notesSecrets)
	ctx := context.Background()
	if _, err := c.Reconcile(ctx, notesSpec()); err != nil {
		t.Fatal(err)
	}
	tok, err := c.Mint(ctx, "dev-notes", "sess-1.123", 60*time.Second)
	if err != nil || !strings.HasPrefix(tok, "av_sess_") {
		t.Fatalf("mint: %q %v", tok, err)
	}
	if v, _ := f.Get("dev-notes"); len(v.Sessions) != 1 || v.Sessions[0].TTL != 300 || v.Sessions[0].Label != "sess-1.123" {
		t.Errorf("sessions %+v, want one with ttl 300 and the label", v.Sessions)
	}
	if _, err := c.Mint(ctx, "dev-notes", "sess-1.124", 1860*time.Second); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Get("dev-notes"); v.Sessions[1].TTL != 1860 {
		t.Errorf("ttl %d, want 1860", v.Sessions[1].TTL)
	}
	if _, err := c.Mint(ctx, "nope", "x", time.Hour); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("mint on a missing vault: %v", err)
	}
}

func TestRevoke_ListThenDeleteByLabel(t *testing.T) {
	c, f, _ := setup(t, nil)
	f.AddSession("v1", "sess-1.1")
	f.AddSession("v1", "sess-2.1")
	if err := c.Revoke(context.Background(), "v1", "sess-1.1"); err != nil {
		t.Fatal(err)
	}
	v, _ := f.Get("v1")
	if len(v.Sessions) != 1 || v.Sessions[0].Label != "sess-2.1" {
		t.Errorf("sessions %+v", v.Sessions)
	}
	if !slices.Contains(f.Calls(), "DELETE /v1/sessions/s1?vault=v1") {
		t.Errorf("no vault-scoped DELETE: %v", f.Calls())
	}
	if err := c.Revoke(context.Background(), "absent", "x"); err != nil {
		t.Errorf("a missing vault is nothing to revoke: %v", err)
	}
}

// Exact labels: the bridge executor's "exec-1" must not take "exec-10" with it.
func TestRevoke_ExactLabelAndOwnBudget(t *testing.T) {
	c, f, _ := setup(t, nil)
	f.AddSession("v1", "exec-1")
	f.AddSession("v1", "exec-10")
	if err := c.Revoke(context.Background(), "v1", "exec-1"); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Get("v1"); len(v.Sessions) != 1 || v.Sessions[0].Label != "exec-10" {
		t.Errorf("sessions %+v, want exec-10 kept", v.Sessions)
	}

	f.Configure(func(f *vaulttest.Server) { f.Delay = 5 * time.Second })
	start := time.Now()
	if err := c.Revoke(context.Background(), "v1", "exec-10"); err == nil {
		t.Error("a slow vault returned no error")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("revoke took %s, want its own ~3s budget", d)
	}
}

// docs/adr/0065 §3: a widened vault revokes the tokens minted under the
// narrower spec (an older pin); a narrowed one keeps them.
func TestReconcile_WideningRevokesSessions(t *testing.T) {
	c, f, _ := setup(t, notesSecrets)
	ctx := context.Background()
	narrow := vault.Spec{Name: "dev-notes", Services: notesSpec().Services[2:]}
	if _, err := c.Reconcile(ctx, narrow); err != nil {
		t.Fatal(err)
	}
	f.AddSession("dev-notes", "old-pin.1")
	f.ResetLog()
	if widened, err := c.Reconcile(ctx, notesSpec()); err != nil || !widened {
		t.Fatalf("widened=%v err=%v", widened, err)
	}
	if v, _ := f.Get("dev-notes"); len(v.Sessions) != 0 {
		t.Errorf("a widening kept %d sessions", len(v.Sessions))
	}
	// The revoke lands before the wider services: no old token sees them.
	calls := f.Calls()
	del := slices.IndexFunc(calls, func(l string) bool { return strings.HasPrefix(l, "DELETE /v1/sessions/") })
	put := slices.Index(calls, "PUT /v1/vaults/dev-notes/services")
	if del < 0 || put < 0 || del > put {
		t.Errorf("revoke at %d, PUT at %d, want revoke first: %v", del, put, calls)
	}

	// Narrowings keep sessions, including dropping or reordering a path,
	// which renumbers the positional service names.
	for _, s := range []vault.Spec{
		{Name: "dev-notes", Services: []vault.Service{
			{Host: "github.com", Paths: []string{"/MohammadBnei/notes.git/*", "/MohammadBnei/notes/*"}, Secret: "GH_DEV_NOTES"},
			notesSpec().Services[1], notesSpec().Services[2],
		}},
		{Name: "dev-notes", Services: []vault.Service{
			{Host: "github.com", Paths: []string{"/MohammadBnei/notes.git/*"}, Secret: "GH_DEV_NOTES"},
			notesSpec().Services[1], notesSpec().Services[2],
		}},
		narrow,
	} {
		f.AddSession("dev-notes", "new-pin.1")
		if widened, err := c.Reconcile(ctx, s); err != nil || widened {
			t.Fatalf("%+v: widened=%v err=%v", s.Services, widened, err)
		}
		if v, _ := f.Get("dev-notes"); len(v.Sessions) == 0 {
			t.Errorf("a narrowing revoked sessions: %+v", s.Services)
		}
	}
}

// A credentials POST carries values: the vault's error text is not forwarded.
func TestReconcile_CredentialErrorDropsVaultText(t *testing.T) {
	c, f, _ := setup(t, notesSecrets)
	f.Configure(func(f *vaulttest.Server) { f.CredentialsError = "rejected value ghp-sentinel-value" })
	_, err := c.Reconcile(context.Background(), notesSpec())
	if err == nil || !strings.Contains(err.Error(), "400") || strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("got %v, want the status and no vault text", err)
	}
}

func TestCA(t *testing.T) {
	c, _, _ := setup(t, nil)
	pem, err := c.CA(context.Background())
	if err != nil || string(pem) != vaulttest.CA {
		t.Fatalf("%q %v", pem, err)
	}
}

func TestProxyURL(t *testing.T) {
	c, _, _ := setup(t, nil)
	if u := c.ProxyURL("av_sess_1", "dev-notes"); u != "http://av_sess_1:dev-notes@agent-vault.agent-vault.svc.cluster.local:14322" {
		t.Errorf("proxy url %q", u)
	}
}

// An owner whose name starts "repos" is an ordinary github.com owner.
func TestReconcile_AcceptsOwnerStartingRepos(t *testing.T) {
	c, _, _ := setup(t, notesSecrets)
	spec := vault.Spec{Name: "dev-notes", Services: []vault.Service{
		{Host: "github.com", Paths: []string{"/reposlab/notes/*", "/reposlab/notes.git/*"}, Secret: "GH_DEV_NOTES"},
		{Host: "api.github.com", Paths: []string{"/repos/reposlab/notes/*"}, Secret: "GH_DEV_NOTES"},
	}}
	if _, err := c.Reconcile(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
}
