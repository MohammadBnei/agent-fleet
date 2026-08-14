//go:build integration

package coreserver

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repoprofiles"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
)

// fakeE2eProvisionerServer records CreateE2ESession requests — same
// package-local fake pattern already used in dispatch/loop_integration_test.go
// and dashboard/warm_integration_test.go (no shared test-helper package
// across those three, deliberately: a test-only cross-package dependency
// buys nothing three call sites don't already get from copy-paste).
type fakeE2eProvisionerServer struct {
	agentfleetv1.UnimplementedProvisionerServiceServer
	calls []*agentfleetv1.CreateE2ESessionRequest
}

// fakeEndpoints stands in for what k8s.EndpointsFor would produce. The values
// are deliberately not the real ones — a test that reproduces the production
// address format would still pass if core started rebuilding endpoints itself
// instead of forwarding them, which is precisely the regression to catch.
var fakeEndpoints = []*agentfleetv1.ServiceEndpoint{
	{Name: "exec", Address: "sentinel-exec.invalid.:1", Protocol: "mcp-streamable-http", Path: "/mcp"},
	{Name: "playwright", Address: "sentinel-pw.invalid.:2", Protocol: "mcp-streamable-http", Path: "/mcp"},
}

func (f *fakeE2eProvisionerServer) CreateE2ESession(ctx context.Context, req *agentfleetv1.CreateE2ESessionRequest) (*agentfleetv1.CreateE2ESessionResponse, error) {
	f.calls = append(f.calls, req)
	return &agentfleetv1.CreateE2ESessionResponse{
		Status:     "running",
		PreviewUrl: "https://example.test/preview",
		Endpoints:  fakeEndpoints,
	}, nil
}

func newFakeE2eProvisioner(t *testing.T) (*fakeE2eProvisionerServer, *provisionerclient.Client) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	fake := &fakeE2eProvisionerServer{}
	agentfleetv1.RegisterProvisionerServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	client, err := provisionerclient.New(lis.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return fake, client
}

// TestRequestE2EEnv_UsesResolvedProfileStartCmd is a regression test for a
// bug caught live via /kind-local: RequestE2EEnv resolved a profile's
// Tools/Services but forgot to fall back to the profile's own start_cmd
// when the caller's own request.start_cmd was empty — CreateE2ESession
// silently got an empty start_cmd and the provisioner fell through to its
// StartCmdFor rolling-deploy fallback (the OLD, unfixed dream-analyst
// command) instead of the fixed one stored in the profile.
func TestRequestE2EEnv_UsesResolvedProfileStartCmd(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := sessions.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)

	// "e2e-test", not "e2e" — dream-analyst already has a real seeded "e2e"
	// profile (db/migrations/000003_repo_profiles.up.sql), and
	// (repo_name, name) is unique.
	if _, err := profileStore.Create(ctx, repoprofiles.Profile{
		RepoName: "dream-analyst",
		Name:     "e2e-test",
		StartCmd: "cd front && bun install && bunx vite dev --host 0.0.0.0 --port $PORT",
	}); err != nil {
		t.Fatalf("seed e2e profile: %v", err)
	}

	taskID, err := taskStore.CreateTask(ctx, "dream-analyst", "verify e2e start_cmd resolution", "", "claude-opus-4-8", nil, nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	fake, provisioner := newFakeE2eProvisioner(t)
	srv := New(nil, taskStore, nil, profileStore, repos.NewStore(pool), provisioner, nil, nil)

	if _, err := srv.RequestE2EEnv(ctx, &agentfleetv1.RequestE2EEnvRequest{SessionId: taskID, Profile: "e2e-test"}); err != nil {
		t.Fatalf("RequestE2EEnv: %v", err)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("expected exactly 1 CreateE2ESession call, got %d", len(fake.calls))
	}
	got := fake.calls[0].GetStartCmd()
	want := "cd front && bun install && bunx vite dev --host 0.0.0.0 --port $PORT"
	if got != want {
		t.Errorf("CreateE2ESession start_cmd = %q, want %q (resolved profile's own start_cmd)", got, want)
	}
}

// TestRequestE2EEnv_ForwardsEndpointsUntouched pins docs/adr/0045's central
// claim: core is a *field* passthrough for the roster, not a participant in
// it. It must not build, reorder, filter or normalise these — it does not
// know what MCP is, and the moment it starts caring the relay it just deleted
// is back in a different shape.
//
// The sentinel addresses are unresolvable on purpose, so the only way this
// passes is by forwarding exactly what the provisioner sent.
func TestRequestE2EEnv_ForwardsEndpointsUntouched(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := sessions.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)

	taskID, err := taskStore.CreateTask(ctx, "dream-analyst", "verify endpoint passthrough", "", "claude-opus-4-8", nil, nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	_, provisioner := newFakeE2eProvisioner(t)
	srv := New(nil, taskStore, nil, profileStore, repos.NewStore(pool), provisioner, nil, nil)

	resp, err := srv.RequestE2EEnv(ctx, &agentfleetv1.RequestE2EEnvRequest{SessionId: taskID})
	if err != nil {
		t.Fatalf("RequestE2EEnv: %v", err)
	}

	got := resp.GetEndpoints()
	if len(got) != len(fakeEndpoints) {
		t.Fatalf("got %d endpoints, want %d — core dropped or added some", len(got), len(fakeEndpoints))
	}
	for i, want := range fakeEndpoints {
		if got[i].GetName() != want.GetName() {
			t.Errorf("endpoint %d name = %q, want %q (order must survive too)", i, got[i].GetName(), want.GetName())
		}
		if got[i].GetAddress() != want.GetAddress() {
			t.Errorf("endpoint %d address = %q, want %q — core must not rewrite addresses", i, got[i].GetAddress(), want.GetAddress())
		}
		if got[i].GetProtocol() != want.GetProtocol() || got[i].GetPath() != want.GetPath() {
			t.Errorf("endpoint %d protocol/path = %q %q, want %q %q", i, got[i].GetProtocol(), got[i].GetPath(), want.GetProtocol(), want.GetPath())
		}
	}
}

// TestRequestE2EEnv_CallerOverrideWinsOverProfile confirms the caller's
// own start_cmd still takes priority — the agent knows the worktree's
// actual layout right now, which beats a static profile value.
func TestRequestE2EEnv_CallerOverrideWinsOverProfile(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := sessions.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)

	if _, err := profileStore.Create(ctx, repoprofiles.Profile{
		RepoName: "dream-analyst",
		Name:     "e2e-test",
		StartCmd: "profile default command",
	}); err != nil {
		t.Fatalf("seed e2e profile: %v", err)
	}

	taskID, err := taskStore.CreateTask(ctx, "dream-analyst", "verify override wins", "", "claude-opus-4-8", nil, nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	fake, provisioner := newFakeE2eProvisioner(t)
	srv := New(nil, taskStore, nil, profileStore, repos.NewStore(pool), provisioner, nil, nil)

	if _, err := srv.RequestE2EEnv(ctx, &agentfleetv1.RequestE2EEnvRequest{SessionId: taskID, Profile: "e2e-test", StartCmd: "agent-supplied command"}); err != nil {
		t.Fatalf("RequestE2EEnv: %v", err)
	}

	if got := fake.calls[0].GetStartCmd(); got != "agent-supplied command" {
		t.Errorf("CreateE2ESession start_cmd = %q, want the caller's override", got)
	}
}
