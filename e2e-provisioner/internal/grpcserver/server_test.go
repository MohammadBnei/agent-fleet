package grpcserver

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/MohammadBnei/agent-fleet/e2e-provisioner/internal/db"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type fakeStore struct {
	killResult bool
	session    *db.Session
}

func (f *fakeStore) RequestKill(ctx context.Context, taskID string) (bool, error) {
	return f.killResult, nil
}
func (f *fakeStore) GetActiveSessionForTask(ctx context.Context, taskID string) (*db.Session, error) {
	return f.session, nil
}

func dialServer(t *testing.T, store SessionStore) agentfleetv1.E2EProvisionerServiceClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() { _ = lis.Close() })

	srv := grpc.NewServer()
	agentfleetv1.RegisterE2EProvisionerServiceServer(srv, New(store))
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
	return agentfleetv1.NewE2EProvisionerServiceClient(conn)
}

func TestKillE2ESession(t *testing.T) {
	client := dialServer(t, &fakeStore{killResult: true})
	resp, err := client.KillE2ESession(context.Background(), &agentfleetv1.KillE2ESessionRequest{TaskId: "t1"})
	if err != nil {
		t.Fatalf("KillE2ESession: %v", err)
	}
	if !resp.GetKilled() {
		t.Errorf("expected killed=true")
	}
}

func TestGetE2ESessionStatus_NoActiveSession(t *testing.T) {
	client := dialServer(t, &fakeStore{session: nil})
	resp, err := client.GetE2ESessionStatus(context.Background(), &agentfleetv1.GetE2ESessionStatusRequest{TaskId: "t1"})
	if err != nil {
		t.Fatalf("GetE2ESessionStatus: %v", err)
	}
	if resp.GetStatus() != "" {
		t.Errorf("expected empty status for no session, got %q", resp.GetStatus())
	}
}

func TestGetE2ESessionStatus_ActiveSession(t *testing.T) {
	path := "/t1/app"
	client := dialServer(t, &fakeStore{session: &db.Session{Status: "running", IngressPath: &path}})
	resp, err := client.GetE2ESessionStatus(context.Background(), &agentfleetv1.GetE2ESessionStatusRequest{TaskId: "t1"})
	if err != nil {
		t.Fatalf("GetE2ESessionStatus: %v", err)
	}
	if resp.GetStatus() != "running" || resp.GetPreviewUrl() != "/t1/app" {
		t.Errorf("unexpected response: %+v", resp)
	}
}
