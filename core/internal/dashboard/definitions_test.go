//go:build integration

package dashboard

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/packs"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// definitionsProvisioner serves definitions/ and records pod creates.
type definitionsProvisioner struct {
	agentfleetv1.UnimplementedProvisionerServiceServer
	mu    sync.Mutex
	files map[string][]byte
	sha   string
	// dropsProfile is an older provisioner serving the create: it ignores the
	// request's profile and does not confirm it (docs/adr/0064).
	dropsProfile bool
	// noVaultToken is a P1 provisioner: it applies the profile but mints no
	// vault token (docs/adr/0065).
	noVaultToken bool
	// noGenome is a provisioner from before docs/adr/0063: it drops the
	// request's context packs and does not confirm them.
	noGenome bool
	// createErr fails the create AFTER recording it: the Job may exist.
	createErr error
	// onCreate runs inside the create (a caller going away mid-call).
	onCreate    func()
	teardownErr error
	creates     []*agentfleetv1.CreateWorkerPodRequest
	teardowns   []string
}

func (p *definitionsProvisioner) ListDefinitionFiles(context.Context, *agentfleetv1.ListDefinitionFilesRequest) (*agentfleetv1.ListDefinitionFilesResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := &agentfleetv1.ListDefinitionFilesResponse{Sha: p.sha}
	for path, b := range p.files {
		out.Files = append(out.Files, &agentfleetv1.DefinitionFile{Path: path, Content: b})
	}
	return out, nil
}

func (p *definitionsProvisioner) CreateWorkerPod(_ context.Context, req *agentfleetv1.CreateWorkerPodRequest) (*agentfleetv1.CreateWorkerPodResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates = append(p.creates, req)
	if p.onCreate != nil {
		p.onCreate()
	}
	if p.createErr != nil {
		return nil, p.createErr
	}
	applied := req.GetDefinition() != nil && !p.dropsProfile
	return &agentfleetv1.CreateWorkerPodResponse{PodName: "worker-test", DefinitionProfileApplied: applied, VaultTokenApplied: applied && !p.noVaultToken, GenomeApplied: !p.noGenome}, nil
}

func (p *definitionsProvisioner) TearDownSession(_ context.Context, req *agentfleetv1.TearDownSessionRequest) (*agentfleetv1.TearDownSessionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.teardowns = append(p.teardowns, req.GetSessionId())
	return &agentfleetv1.TearDownSessionResponse{}, p.teardownErr
}

func (p *definitionsProvisioner) set(f func(*definitionsProvisioner)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

func (p *definitionsProvisioner) createCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.creates)
}

// The repo's own definitions/ plus one invalid fixture.
func repoDefinitionFiles(t *testing.T) map[string][]byte {
	t.Helper()
	root := os.DirFS("../../..")
	files := map[string][]byte{}
	if err := fs.WalkDir(root, "definitions", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(root, p)
		files[p] = b
		return err
	}); err != nil {
		t.Fatal(err)
	}
	files["definitions/broken.yaml"] = []byte(strings.Replace(string(files["definitions/dev-dream-analyst.yaml"]),
		"name: dev-dream-analyst }", "name: broken }\n", 1) + "  mode: { default: auto, allow: [auto] }\n")
	return files
}

func newDefinitionsServer(t *testing.T) (*Server, *definitionsProvisioner, *definitions.Registry, *sessions.Store) {
	t.Helper()
	s, prov, reg, store, _ := newDefinitionsServerPool(t)
	return s, prov, reg, store
}

// setRepo writes a repos row directly: the console has no repo editor since
// docs/adr/0074, and these tests only need the row's url and image.
func setRepo(t *testing.T, pool *pgxpool.Pool, r *repos.Repo) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE repos SET url = $2, image = $3 WHERE name = $1`, r.Name, r.URL, r.Image); err != nil {
		t.Fatal(err)
	}
}

func newDefinitionsServerPool(t *testing.T) (*Server, *definitionsProvisioner, *definitions.Registry, *sessions.Store, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.NewPool(t)
	prov := &definitionsProvisioner{files: repoDefinitionFiles(t), sha: "sha-one"}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	agentfleetv1.RegisterProvisionerServiceServer(gs, prov)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	client, err := provisionerclient.New(lis.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	reg := definitions.NewRegistry(pool, client)
	if err := reg.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := sessions.NewStore(pool)
	s := &Server{sessions: store, repos: repos.NewStore(pool), transcr: transcript.NewPostgresStore(pool), e2e: client,
		caps: sessions.Caps{MaxLive: 5, MaxDefinition: 2}, definitions: reg}
	return s, prov, reg, store, pool
}

func code(err error) connect.Code {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code()
	}
	return connect.CodeUnknown
}

func createDef(s *Server, name string, mut func(*agentfleetv1.CreateSessionRequest)) (*agentfleetv1.Session, error) {
	req := &agentfleetv1.CreateSessionRequest{Definition: &name}
	if mut != nil {
		mut(req)
	}
	res, err := s.CreateSession(context.Background(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetSession(), nil
}

func TestCreateSession_Definition(t *testing.T) {
	s, _, _, _ := newDefinitionsServer(t)

	for _, tc := range []struct {
		name string
		mut  func(*agentfleetv1.CreateSessionRequest)
		want connect.Code
		msg  string
	}{
		{"nope", nil, connect.CodeNotFound, ""},
		{"broken", nil, connect.CodeFailedPrecondition, "skips canUseTool"},
		{"opus-workspace", nil, connect.CodeFailedPrecondition, `targets repo "opus-workspace", which is not onboarded`},
		{"dev-dream-analyst", func(r *agentfleetv1.CreateSessionRequest) { r.Repo = "dream-analyst" }, connect.CodeInvalidArgument, ""},
		{"dev-dream-analyst", func(r *agentfleetv1.CreateSessionRequest) { m := "auto"; r.PermissionMode = &m }, connect.CodeInvalidArgument, ""},
	} {
		_, err := createDef(s, tc.name, tc.mut)
		if code(err) != tc.want || !strings.Contains(errText(err), tc.msg) {
			t.Errorf("%s: got %v, want %v containing %q", tc.name, err, tc.want, tc.msg)
		}
	}

	sess, err := createDef(s, "dev-dream-analyst", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.GetDefinitionName() != "dev-dream-analyst" || sess.GetDefinitionSha() != "sha-one" {
		t.Fatalf("pin: %q %q", sess.GetDefinitionName(), sess.GetDefinitionSha())
	}
	// Explicit, never the 'auto' column default (docs/adr/0061).
	if sess.GetPermissionMode() != "default" || sess.GetModel() != "claude-opus-5" || sess.GetRepo() != "dream-analyst" {
		t.Fatalf("got mode %q model %q repo %q", sess.GetPermissionMode(), sess.GetModel(), sess.GetRepo())
	}

	// A definition only narrows, for admins too, and a refusal appends
	// nothing: the transcript entry is what reaches the running worker.
	_, err = s.SetPermissionMode(context.Background(), connect.NewRequest(&agentfleetv1.SetPermissionModeRequest{SessionId: sess.GetId(), Mode: "auto"}))
	if code(err) != connect.CodeInvalidArgument {
		t.Fatalf("auto on a definition session: %v", err)
	}
	if entries, _, err := s.transcr.ReadSince(context.Background(), sess.GetId(), 0, 100); err != nil || len(entries) != 0 {
		t.Fatalf("a refused mode reached the transcript: %v %v", entries, err)
	}
	if _, err := s.SetPermissionMode(context.Background(), connect.NewRequest(&agentfleetv1.SetPermissionModeRequest{SessionId: sess.GetId(), Mode: "plan"})); err != nil {
		t.Fatalf("plan is in mode.allow: %v", err)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestListDefinitions(t *testing.T) {
	s, _, _, _ := newDefinitionsServer(t)
	res, err := s.ListDefinitions(context.Background(), connect.NewRequest(&agentfleetv1.ListDefinitionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*agentfleetv1.AgentDefinition{}
	for _, d := range res.Msg.GetDefinitions() {
		got[d.GetName()] = d
	}
	if _, ok := got["workspace-base"]; ok {
		t.Error("an abstract definition is never listed")
	}
	if d := got["dev-dream-analyst"]; !d.GetValid() || d.GetRepo() != "dream-analyst" || d.GetPermissionMode() != "default" || d.GetAudienceGroup() != "platform-admins" {
		t.Errorf("dev-dream-analyst: %v", d)
	}
	if d := got["opus-workspace"]; !d.GetValid() {
		t.Errorf("opus-workspace validates; only create fails: %v", d)
	}
	if d := got["broken"]; d.GetValid() || !strings.Contains(d.GetError(), "skips canUseTool") {
		t.Errorf("broken: %v", d)
	}
}

func assertNoLease(t *testing.T, store *sessions.Store, id string) {
	t.Helper()
	sess, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if sess.LeaseID != "" || sess.PodPhase != nil {
		t.Fatalf("a refused warm reserved: lease=%q phase=%v", sess.LeaseID, sess.PodPhase)
	}
}

// leaseValid reports whether the lease the i-th create carried still
// authenticates (a definition lease lives in profile_lease_id, never read back).
func leaseValid(t *testing.T, store *sessions.Store, prov *definitionsProvisioner, i int) bool {
	t.Helper()
	prov.mu.Lock()
	req := prov.creates[i]
	prov.mu.Unlock()
	ok, _, err := store.VerifyLease(context.Background(), req.GetSessionId(), req.GetLeaseId())
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// docs/adr/0064: a definition create that errors may still have built the
// ordinary pod (the call's timeout after the Job exists, an older replica dying
// mid-call), and a caller that went away must not stop the revocation.
func TestWarmIfIdle_DefinitionUnconfirmedCreateFailsClosed(t *testing.T) {
	s, prov, _, store := newDefinitionsServer(t)
	sess, err := createDef(s, "dev-dream-analyst", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range []struct {
		name string
		set  func(p *definitionsProvisioner, cancel func())
	}{
		{"create error", func(p *definitionsProvisioner, _ func()) { p.createErr = errors.New("replica went away") }},
		{"caller gone", func(p *definitionsProvisioner, cancel func()) {
			p.createErr, p.dropsProfile, p.onCreate = nil, true, cancel
		}},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		prov.set(func(p *definitionsProvisioner) { c.set(p, cancel) })
		if _, err := s.WarmIfIdle(ctx, sess.GetId()); err == nil {
			t.Fatalf("%s: an unconfirmed definition pod warmed", c.name)
		}
		cancel()
		if got, err := store.Get(context.Background(), sess.GetId()); err != nil || sessions.IsPodPhaseLive(got.PodPhase) || leaseValid(t, store, prov, i) {
			t.Fatalf("%s: kept its lease or a live phase: %+v %v", c.name, got, err)
		}
		prov.mu.Lock()
		torn := len(prov.teardowns)
		prov.mu.Unlock()
		if torn != i+1 {
			t.Fatalf("%s: %d teardowns, want the possibly-built pod removed", c.name, torn)
		}
	}
}

func TestWarmIfIdle_Definition(t *testing.T) {
	ctx := context.Background()
	s, prov, reg, store, pool := newDefinitionsServerPool(t)
	sess, err := createDef(s, "dev-dream-analyst", nil)
	if err != nil {
		t.Fatal(err)
	}

	// The profile is built from the pinned version, with the provisioner's
	// default image whatever repos.image says.
	repo, err := s.repos.Get(ctx, "dream-analyst")
	if err != nil {
		t.Fatal(err)
	}
	repo.Image = "registry.example/old-worker:1"
	setRepo(t, pool, repo)
	// The create lands on an older provisioner (or an older replica in a
	// rollout overlap): it built the ordinary pod, so core revokes and removes
	// it. If the removal fails, the phase stays live so the startup-stall
	// sweep retries it: CRASHED would hide a pod that is still running.
	prov.set(func(p *definitionsProvisioner) { p.dropsProfile, p.teardownErr = true, errors.New("k8s down") })
	if _, err := s.WarmIfIdle(ctx, sess.GetId()); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "teardown failed") {
		t.Fatalf("an unprofiled pod whose teardown failed: got %v", err)
	}
	if got, err := store.Get(ctx, sess.GetId()); err != nil || !sessions.IsPodPhaseLive(got.PodPhase) || leaseValid(t, store, prov, 0) {
		t.Fatalf("want no lease and a live phase for the sweep to retry: %+v %v", got, err)
	}
	if err := store.SetPodPhase(ctx, sess.GetId(), agentfleetv1.PodPhase_POD_PHASE_TERMINATED.String(), ""); err != nil {
		t.Fatal(err) // the sweep's teardown
	}
	prov.set(func(p *definitionsProvisioner) { p.teardownErr = nil })
	if _, err := s.WarmIfIdle(ctx, sess.GetId()); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "did not apply the definition pod profile") {
		t.Fatalf("an unconfirmed profile: got %v", err)
	}
	if got, err := store.Get(ctx, sess.GetId()); err != nil || sessions.IsPodPhaseLive(got.PodPhase) || leaseValid(t, store, prov, 1) {
		t.Fatalf("an unprofiled pod kept its lease or a live phase: %+v %v", got, err)
	}
	prov.mu.Lock()
	torn := append([]string(nil), prov.teardowns...)
	prov.mu.Unlock()
	if len(torn) != 2 || torn[1] != sess.GetId() {
		t.Fatalf("teardowns = %v, want the unprofiled pod removed", torn)
	}

	prov.set(func(p *definitionsProvisioner) { p.dropsProfile = false })
	if _, err := s.WarmIfIdle(ctx, sess.GetId()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	prov.mu.Lock()
	got := prov.creates[2]
	prov.mu.Unlock()
	if d := got.GetDefinition(); d.GetName() != "dev-dream-analyst" || d.GetGitAuthorName() != "fleet-dev-agent" || d.GetGitAuthorEmail() != "fleet-dev-agent@bnei.dev" {
		t.Errorf("profile = %v", d)
	}
	// docs/adr/0065: the vault spec (key names only) and limits.timeout.
	if d := got.GetDefinition(); d.GetTimeoutSeconds() != 1800 || d.GetVault().GetName() != "dev-dream-analyst" ||
		len(d.GetVault().GetServices()) != 1 || d.GetVault().GetServices()[0].GetSecret() != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Errorf("profile vault/timeout = %v", d)
	}
	if got.GetImage() != "" || len(got.GetToolKeys()) != 0 {
		t.Errorf("image %q tool keys %v: a definition pod takes the default image and no keys", got.GetImage(), got.GetToolKeys())
	}
	if !leaseValid(t, store, prov, 2) {
		t.Fatal("the warm reserved no lease")
	}

	// Revocation: a content change makes the old pin stop warming.
	other, err := createDef(s, "dev-dream-analyst", nil)
	if err != nil {
		t.Fatal(err)
	}
	prov.set(func(p *definitionsProvisioner) {
		p.sha = "sha-two"
		p.files["definitions/dev-dream-analyst.yaml"] = []byte(strings.Replace(string(p.files["definitions/dev-dream-analyst.yaml"]),
			"fleet-dev-agent@bnei.dev", "someone-else@bnei.dev", 1))
	})
	if err := reg.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WarmIfIdle(ctx, other.GetId()); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "changed since this session was created") {
		t.Fatalf("got %v", err)
	}
	assertNoLease(t, store, other.GetId())

	prov.set(func(p *definitionsProvisioner) { delete(p.files, "definitions/dev-dream-analyst.yaml") })
	if err := reg.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WarmIfIdle(ctx, other.GetId()); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "no longer defined") {
		t.Fatalf("got %v", err)
	}
	if prov.createCount() != 3 {
		t.Fatal("a revoked pin may not create a pod")
	}
}

// docs/adr/0065: a P1 provisioner (profile applied, no vault token) built a pod
// holding the real model token, so it is refused like an unprofiled one; and a
// vault refusal reaches the console with the provisioner's code and the
// missing key's name.
func TestWarmIfIdle_DefinitionNeedsAVaultToken(t *testing.T) {
	ctx := context.Background()
	s, prov, _, store := newDefinitionsServer(t)
	sess, err := createDef(s, "dev-dream-analyst", nil)
	if err != nil {
		t.Fatal(err)
	}
	prov.set(func(p *definitionsProvisioner) { p.noVaultToken = true })
	if _, err := s.WarmIfIdle(ctx, sess.GetId()); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "vault token") {
		t.Fatalf("a pod without a vault token: got %v", err)
	}
	if got, err := store.Get(ctx, sess.GetId()); err != nil || sessions.IsPodPhaseLive(got.PodPhase) || leaseValid(t, store, prov, 0) {
		t.Fatalf("kept its lease or a live phase: %+v %v", got, err)
	}
	prov.set(func(p *definitionsProvisioner) {
		p.noVaultToken = false
		p.createErr = status.Error(codes.FailedPrecondition, "vault: vault dev-dream-analyst: secret CLAUDE_CODE_OAUTH_TOKEN not set in agent-fleet-nygh /agent-vault")
	})
	if _, err := s.WarmIfIdle(ctx, sess.GetId()); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN not set") {
		t.Fatalf("a vault refusal: got %v", err)
	}
}

// docs/adr/0063: a definition warm carries its attached packs, and a
// provisioner that drops them gets the warm refused like a dropped profile.
func TestWarmIfIdle_DefinitionContextPacks(t *testing.T) {
	ctx := context.Background()
	s, prov, _, store, pool := newDefinitionsServerPool(t)
	ps := packs.NewStore(pool)
	s.SetPacks(ps)
	if err := ps.Create(ctx, "notes-style", "platform-admins"); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.SaveVersion(ctx, packs.Version{Pack: "notes-style", Text: "short notes"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := ps.Attach(ctx, packs.KindDefinition, "dev-dream-analyst", "notes-style", "admin"); err != nil {
		t.Fatal(err)
	}
	sess, err := createDef(s, "dev-dream-analyst", nil)
	if err != nil {
		t.Fatal(err)
	}

	prov.set(func(p *definitionsProvisioner) { p.noGenome = true })
	if _, err := s.WarmIfIdle(ctx, sess.GetId()); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "context packs") {
		t.Fatalf("a pod without its packs: got %v", err)
	}
	if got, err := store.Get(ctx, sess.GetId()); err != nil || sessions.IsPodPhaseLive(got.PodPhase) {
		t.Fatalf("kept a live phase: %+v %v", got, err)
	}

	prov.set(func(p *definitionsProvisioner) { p.noGenome = false })
	if _, err := s.WarmIfIdle(ctx, sess.GetId()); err != nil {
		t.Fatal(err)
	}
	prov.mu.Lock()
	g := prov.creates[len(prov.creates)-1].GetGenome()
	prov.mu.Unlock()
	if len(g) != 1 || g[0].GetPath() != ".claude/rules/20-notes-style.md" || g[0].GetBody() != "short notes" {
		t.Fatalf("genome = %v", g)
	}
	// The warm records what it rendered (docs/adr/0063 §7), as a fleet SYSTEM
	// entry the worker never receives.
	var from, text string
	if err := pool.QueryRow(ctx, `SELECT "from", text FROM transcript WHERE session_id = $1 AND text LIKE '%context_rendered%' ORDER BY seq DESC LIMIT 1`, sess.GetId()).Scan(&from, &text); err != nil {
		t.Fatal(err)
	}
	if from != "fleet" || !strings.Contains(text, `"versions":["notes-style@1"]`) || !strings.Contains(text, `"applied":true`) {
		t.Fatalf("context_rendered entry = %s %s", from, text)
	}
}

// docs/adr/0065 §6: a push definition warms only on the one repo URL its
// credentials name, refused before any lease exists.
func TestWarmIfIdle_DefinitionURLBinding(t *testing.T) {
	ctx := context.Background()
	s, prov, reg, store, pool := newDefinitionsServerPool(t)
	prov.set(func(p *definitionsProvisioner) {
		p.files["definitions/dev-pusher.yaml"] = []byte(`apiVersion: fleet.bnei.dev/v1alpha1
kind: AgentDefinition
metadata: { name: dev-pusher }
spec:
  extends: workspace-base
  audience: { group: platform-admins }
  credentials:
    vault: dev-pusher
    services:
      - { host: github.com, paths: ["/MohammadBnei/dream-analyst/*", "/MohammadBnei/dream-analyst.git/*"], secret: GH_DEV_PUSHER }
      - { host: api.anthropic.com, secret: CLAUDE_CODE_OAUTH_TOKEN }
  workspace: { kind: repo, repo: dream-analyst }
  git: { mode: push, branch: main, identity: { name: p, email: p@bnei.dev } }
`)
	})
	if err := reg.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	sess, err := createDef(s, "dev-pusher", nil)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.repos.Get(ctx, "dream-analyst")
	if err != nil {
		t.Fatal(err)
	}
	seeded := repo.URL // https://github.com/MohammadBnei/dream-analyst.git
	for _, url := range []string{"https://github.com/MohammadBnei/dream-analyst-evil.git", "https://github.com/mohammadbnei/dream-analyst.git", "https://github.com/MohammadBnei/dream-analyst/x.git"} {
		repo.URL = url
		setRepo(t, pool, repo)
		if _, err := s.WarmIfIdle(ctx, sess.GetId()); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("%s: got %v", url, err)
		}
		if prov.createCount() != 0 {
			t.Fatalf("%s: a pod was created", url)
		}
		assertNoLease(t, store, sess.GetId())
	}
	repo.URL = seeded
	setRepo(t, pool, repo)
	if _, err := s.WarmIfIdle(ctx, sess.GetId()); err != nil {
		t.Fatalf("the bound URL: %v", err)
	}
}
