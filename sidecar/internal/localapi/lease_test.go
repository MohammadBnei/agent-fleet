package localapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
)

type saveRecorder struct {
	agentfleetv1.UnimplementedCoreServiceServer
	mu       sync.Mutex
	reqLease string
	mdLease  []string
}

func (f *saveRecorder) SaveAgentSessionId(ctx context.Context, req *agentfleetv1.SaveAgentSessionIdRequest) (*agentfleetv1.SaveAgentSessionIdResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqLease, f.mdLease = req.GetLeaseId(), md.Get("x-fleet-lease-id")
	return &agentfleetv1.SaveAgentSessionIdResponse{}, nil
}

// docs/adr/0064: the worker no longer holds the lease (a definition pod has no
// LEASE_ID in its env), so the sidecar always uses its own and ignores any
// leaseId in the body, which also means a process in the worker container
// cannot pick the lease this write is checked against.
func TestSessionID_UsesTheSidecarsOwnLease(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &saveRecorder{}
	gs := grpc.NewServer()
	agentfleetv1.RegisterCoreServiceServer(gs, fake)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	core, err := coreclient.New(lis.Addr().String(), "s1", "own-lease")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	srv := httptest.NewServer(New(core))
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/session-id", "application/json", strings.NewReader(`{"sessionId":"sdk-1","model":"m","leaseId":"forged"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.reqLease != "own-lease" || len(fake.mdLease) != 1 || fake.mdLease[0] != "own-lease" {
		t.Fatalf("SaveAgentSessionId lease: body %q, metadata %v; want the sidecar's own", fake.reqLease, fake.mdLease)
	}
}
