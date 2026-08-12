//go:build integration

package dispatch

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/jackc/pgx/v5/pgxpool"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repoprofiles"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

func newTestPool(t *testing.T) *pgxpool.Pool {
	return dbtest.NewPool(t)
}

// fakeProvisionerServer records CreateWorkerPod requests instead of doing
// any real git/k8s work — same pattern as dashboard's warm_integration_test.go
// (package-local, not shared: neither package exports a testing helper the
// other could import without creating a test-only cross-package dependency).
type fakeProvisionerServer struct {
	agentfleetv1.UnimplementedProvisionerServiceServer
	calls []*agentfleetv1.CreateWorkerPodRequest
}

func (f *fakeProvisionerServer) CreateWorkerPod(ctx context.Context, req *agentfleetv1.CreateWorkerPodRequest) (*agentfleetv1.CreateWorkerPodResponse, error) {
	f.calls = append(f.calls, req)
	return &agentfleetv1.CreateWorkerPodResponse{PodName: "worker-" + req.GetTaskId()}, nil
}

func newFakeProvisioner(t *testing.T) (*fakeProvisionerServer, *provisionerclient.Client) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	fake := &fakeProvisionerServer{}
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

// TestTick_ResolvesWorkerProfileIngredients covers docs/adr/0034's dispatch
// integration: a claimed task's CreateWorkerPod call carries whatever
// tool_keys/service_ingredients the repo's "worker"-named profile declares.
func TestTick_ResolvesWorkerProfileIngredients(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)
	repoStore := repos.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool)

	// "dream-analyst" is seeded by db/migrations/ (docs/adr/0030).
	if _, err := profileStore.Create(ctx, repoprofiles.Profile{
		RepoName: "dream-analyst",
		Name:     "worker",
		Tools:    []string{"bun-toolchain"},
		Services: []repoprofiles.ServiceIngredient{{Key: "postgres", ScopeMode: "task-scoped"}},
	}); err != nil {
		t.Fatalf("seed worker profile: %v", err)
	}

	taskID, err := taskStore.CreateTask(ctx, "dream-analyst", "do the thing", "", "claude-opus-4-8", nil, nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	fake, provisioner := newFakeProvisioner(t)
	l := New(taskStore, transcr, repoStore, profileStore, provisioner, 5, 3, 30*time.Second, 30*time.Minute, 3*time.Minute)

	l.tick(ctx)

	if len(fake.calls) != 1 {
		t.Fatalf("expected exactly 1 CreateWorkerPod call, got %d", len(fake.calls))
	}
	req := fake.calls[0]
	if req.GetTaskId() != taskID {
		t.Errorf("CreateWorkerPod taskId = %q, want %q", req.GetTaskId(), taskID)
	}
	if len(req.GetToolKeys()) != 1 || req.GetToolKeys()[0] != "bun-toolchain" {
		t.Errorf("CreateWorkerPod tool_keys = %v, want [bun-toolchain]", req.GetToolKeys())
	}
	if len(req.GetServiceIngredients()) != 1 || req.GetServiceIngredients()[0].GetKey() != "postgres" ||
		req.GetServiceIngredients()[0].GetScopeMode() != agentfleetv1.ScopeMode_SCOPE_MODE_TASK_SCOPED {
		t.Errorf("CreateWorkerPod service_ingredients = %+v", req.GetServiceIngredients())
	}
}

// TestTick_NoWorkerProfile_EmptyIngredients confirms a repo with no
// "worker"-named profile row gets exactly today's pre-recipe pod shape —
// backward compatible by construction, not by an explicit special case.
func TestTick_NoWorkerProfile_EmptyIngredients(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)
	repoStore := repos.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)
	transcr := transcript.NewPostgresStore(pool)

	// "vos-monolith" is seeded by db/migrations/ but has no "worker" profile.
	taskID, err := taskStore.CreateTask(ctx, "vos-monolith", "do the other thing", "", "claude-opus-4-8", nil, nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	fake, provisioner := newFakeProvisioner(t)
	l := New(taskStore, transcr, repoStore, profileStore, provisioner, 5, 3, 30*time.Second, 30*time.Minute, 3*time.Minute)

	l.tick(ctx)

	if len(fake.calls) != 1 {
		t.Fatalf("expected exactly 1 CreateWorkerPod call, got %d", len(fake.calls))
	}
	req := fake.calls[0]
	if req.GetTaskId() != taskID {
		t.Errorf("CreateWorkerPod taskId = %q, want %q", req.GetTaskId(), taskID)
	}
	if len(req.GetToolKeys()) != 0 || len(req.GetServiceIngredients()) != 0 {
		t.Errorf("expected empty ingredients, got tool_keys=%v service_ingredients=%+v", req.GetToolKeys(), req.GetServiceIngredients())
	}
}
