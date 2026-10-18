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
	"google.golang.org/grpc"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// definitionsProvisioner serves definitions/ and a configurable
// definition_pod_profile answer, and records pod creates.
type definitionsProvisioner struct {
	agentfleetv1.UnimplementedProvisionerServiceServer
	mu      sync.Mutex
	files   map[string][]byte
	sha     string
	profile bool
	creates []*agentfleetv1.CreateWorkerPodRequest
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

func (p *definitionsProvisioner) GetVersion(context.Context, *agentfleetv1.GetVersionRequest) (*agentfleetv1.GetVersionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return &agentfleetv1.GetVersionResponse{Version: "test", DefinitionPodProfile: p.profile}, nil
}

func (p *definitionsProvisioner) CreateWorkerPod(_ context.Context, req *agentfleetv1.CreateWorkerPodRequest) (*agentfleetv1.CreateWorkerPodResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates = append(p.creates, req)
	return &agentfleetv1.CreateWorkerPodResponse{PodName: "worker-test"}, nil
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
	return s, prov, reg, store
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

	// A definition only narrows, for admins too.
	_, err = s.SetPermissionMode(context.Background(), connect.NewRequest(&agentfleetv1.SetPermissionModeRequest{SessionId: sess.GetId(), Mode: "auto"}))
	if code(err) != connect.CodeInvalidArgument {
		t.Fatalf("auto on a definition session: %v", err)
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

func TestWarmIfIdle_Definition(t *testing.T) {
	ctx := context.Background()
	s, prov, reg, store := newDefinitionsServer(t)
	sess, err := createDef(s, "dev-dream-analyst", nil)
	if err != nil {
		t.Fatal(err)
	}

	// S1 alone: the provisioner does not apply the profile, so nothing warms
	// with the ordinary one (GH_TOKEN and LEASE_ID in the worker).
	if _, err := s.WarmIfIdle(ctx, sess.GetId()); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "definition pod profile") {
		t.Fatalf("got %v", err)
	}
	assertNoLease(t, store, sess.GetId())
	if prov.createCount() != 0 {
		t.Fatal("no pod may be created")
	}

	prov.set(func(p *definitionsProvisioner) { p.profile = true })
	if _, err := s.WarmIfIdle(ctx, sess.GetId()); err != nil {
		t.Fatalf("pinned == current and the profile is applied: %v", err)
	}
	if prov.createCount() != 1 || len(prov.creates[0].GetToolKeys()) != 0 {
		t.Fatalf("creates %d, tool keys %v", prov.createCount(), prov.creates)
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
}
