package e2eclient

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type fakeE2eServer struct {
	agentfleetv1.UnimplementedE2EProvisionerServiceServer
	killed bool
}

func (f *fakeE2eServer) KillE2ESession(ctx context.Context, req *agentfleetv1.KillE2ESessionRequest) (*agentfleetv1.KillE2ESessionResponse, error) {
	return &agentfleetv1.KillE2ESessionResponse{Killed: f.killed}, nil
}

func TestClient_KillSession(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() { _ = lis.Close() })

	srv := grpc.NewServer()
	agentfleetv1.RegisterE2EProvisionerServiceServer(srv, &fakeE2eServer{killed: true})
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

	c := &Client{conn: conn, rpc: agentfleetv1.NewE2EProvisionerServiceClient(conn)}
	killed, err := c.KillSession(context.Background(), "task-1", "idem-1")
	if err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if !killed {
		t.Errorf("expected killed=true")
	}
}
