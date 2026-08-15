package coreclient

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// grpc.NewClient only validates retryServiceConfig's JSON at call time —
// go build/vet/lint never catch a bad status-code string in it (shipped
// once: "CANCELED" instead of grpc's expected "CANCELLED", which crash-
// looped every pod calling New at startup). This is the cheapest possible
// guard against that recurring silently.
func TestNew_ValidServiceConfig(t *testing.T) {
	c, err := New("localhost:0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = c.Close()
}

type fakeCoreServer struct {
	agentfleetv1.UnimplementedCoreServiceServer
	sawDeadline   bool
	deadlineHadDL bool
}

func (f *fakeCoreServer) ReportPodEvents(stream agentfleetv1.CoreService_ReportPodEventsServer) error {
	_, f.deadlineHadDL = stream.Context().Deadline()
	f.sawDeadline = true
	if _, err := stream.Recv(); err != nil {
		return stream.SendAndClose(&agentfleetv1.ReportPodEventsResponse{})
	}
	return stream.SendAndClose(&agentfleetv1.ReportPodEventsResponse{})
}

// A hung ReportPodEvents stream previously blocked ReportEvent's caller
// forever — since ReportEvent is called synchronously from
// grpcserver.CreateWorkerPod's own handler at each provisioning step, that
// meant a hung *progress report* could block real provisioning work
// indefinitely, the same class of bug as the core->provisioner direction
// (confirmed live in prod), just the opposite way round. Proves the
// server-side stream context always carries a deadline even when the
// caller passes context.Background().
func TestClient_ReportEvent_AlwaysBoundDeadline(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() { _ = lis.Close() })

	fake := &fakeCoreServer{}
	srv := grpc.NewServer()
	agentfleetv1.RegisterCoreServiceServer(srv, fake)
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

	c := &Client{conn: conn, rpc: agentfleetv1.NewCoreServiceClient(conn)}
	c.ReportEvent(context.Background(), &agentfleetv1.PodEvent{SessionId: "task-1", Phase: agentfleetv1.PodPhase_POD_PHASE_CREATED})

	if !fake.sawDeadline || !fake.deadlineHadDL {
		t.Errorf("ReportEvent: server-side stream context had no deadline — a hung stream would block the caller forever")
	}
}
