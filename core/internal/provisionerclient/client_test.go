package provisionerclient

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type fakeProvisionerServer struct {
	agentfleetv1.UnimplementedProvisionerServiceServer
	killed        bool
	sawDeadline   bool
	deadlineHadDL bool
	// lastKillReq lets tests assert what KillSession actually put on the
	// wire (repo/also_teardown_services) without a second fake type.
	lastKillReq *agentfleetv1.KillE2ESessionRequest
}

func (f *fakeProvisionerServer) KillE2ESession(ctx context.Context, req *agentfleetv1.KillE2ESessionRequest) (*agentfleetv1.KillE2ESessionResponse, error) {
	f.lastKillReq = req
	resp := &agentfleetv1.KillE2ESessionResponse{Killed: f.killed}
	if req.GetAlsoTeardownServices() {
		resp.ServicesTornDown = []string{"postgres", "redis"}
	}
	return resp, nil
}

func (f *fakeProvisionerServer) CreateWorkerPod(ctx context.Context, req *agentfleetv1.CreateWorkerPodRequest) (*agentfleetv1.CreateWorkerPodResponse, error) {
	_, f.deadlineHadDL = ctx.Deadline()
	f.sawDeadline = true
	return &agentfleetv1.CreateWorkerPodResponse{PodName: "worker-test"}, nil
}

func (f *fakeProvisionerServer) TearDownSession(ctx context.Context, req *agentfleetv1.TearDownSessionRequest) (*agentfleetv1.TearDownSessionResponse, error) {
	_, f.deadlineHadDL = ctx.Deadline()
	f.sawDeadline = true
	return &agentfleetv1.TearDownSessionResponse{TornDown: true}, nil
}

func TestClient_KillSession(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() { _ = lis.Close() })

	fake := &fakeProvisionerServer{killed: true}
	srv := grpc.NewServer()
	agentfleetv1.RegisterProvisionerServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	c := &Client{conn: conn, rpc: agentfleetv1.NewProvisionerServiceClient(conn)}
	killed, servicesTornDown, err := c.KillSession(context.Background(), "task-1", "idem-1", "", false)
	if err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if !killed {
		t.Errorf("expected killed=true")
	}
	if len(servicesTornDown) != 0 {
		t.Errorf("expected no services torn down when alsoTeardownServices=false, got %v", servicesTornDown)
	}
	if fake.lastKillReq.GetRepo() != "" || fake.lastKillReq.GetAlsoTeardownServices() {
		t.Errorf("expected repo/alsoTeardownServices not sent when opted out, got %+v", fake.lastKillReq)
	}

	// Opting in to also_teardown_services threads repo onto the wire and
	// surfaces the provisioner's reported services_torn_down back to the
	// caller — the human-confirmed "kill e2e" checkbox path.
	_, servicesTornDown, err = c.KillSession(context.Background(), "task-1", "idem-2", "dream-analyst", true)
	if err != nil {
		t.Fatalf("KillSession (also teardown services): %v", err)
	}
	if fake.lastKillReq.GetRepo() != "dream-analyst" || !fake.lastKillReq.GetAlsoTeardownServices() {
		t.Errorf("expected repo=dream-analyst, alsoTeardownServices=true on the wire, got %+v", fake.lastKillReq)
	}
	if len(servicesTornDown) != 2 {
		t.Errorf("expected servicesTornDown to come back from the fake, got %v", servicesTornDown)
	}
}

// A hung CreateWorkerPod/TearDownSession call previously blocked its
// caller forever — for CreateWorkerPod's one caller, core's single-
// goroutine dispatch loop, that meant the entire fleet's dispatch froze on
// one wedged task (confirmed live in prod). Proves both calls always carry
// a bounded deadline server-side, even when the caller's own ctx has none.
func TestClient_CreateWorkerPodAndTearDownSession_AlwaysBoundDeadline(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() { _ = lis.Close() })

	fake := &fakeProvisionerServer{killed: true}
	srv := grpc.NewServer()
	agentfleetv1.RegisterProvisionerServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	c := &Client{conn: conn, rpc: agentfleetv1.NewProvisionerServiceClient(conn)}

	if _, err := c.CreateWorkerPod(context.Background(), "task-1", "repo", "url", "main", "desc", "", "lease-1", "", 0, nil, nil); err != nil {
		t.Fatalf("CreateWorkerPod: %v", err)
	}
	if !fake.sawDeadline || !fake.deadlineHadDL {
		t.Errorf("CreateWorkerPod: server-side ctx had no deadline — a hung handler would block the caller forever")
	}

	fake.sawDeadline, fake.deadlineHadDL = false, false
	if _, err := c.TearDownSession(context.Background(), "task-1", agentfleetv1.SessionKind_SESSION_KIND_WORKER); err != nil {
		t.Fatalf("TearDownSession: %v", err)
	}
	if !fake.sawDeadline || !fake.deadlineHadDL {
		t.Errorf("TearDownSession: server-side ctx had no deadline — a hung handler would block the caller forever")
	}

	// sessionCallTimeout itself should be finite and sane — catches an
	// accidental 0/negative value that'd make every real call fail instantly.
	if sessionCallTimeout <= 0 || sessionCallTimeout > 10*time.Minute {
		t.Errorf("sessionCallTimeout = %v, expected a small positive bound", sessionCallTimeout)
	}
}
