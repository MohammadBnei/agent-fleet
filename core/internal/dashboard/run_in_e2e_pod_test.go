package dashboard

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
)

// fakeSandboxProvisioner answers GetE2ESessionStatus with a configurable
// roster and records whether the relay was used. No Postgres needed:
// runViaSandbox touches only the provisioner client, so the Server is built
// directly rather than through NewServer.
type fakeSandboxProvisioner struct {
	agentfleetv1.UnimplementedProvisionerServiceServer
	endpoints  []*agentfleetv1.ServiceEndpoint
	relayCalls int
}

func (f *fakeSandboxProvisioner) GetE2ESessionStatus(context.Context, *agentfleetv1.GetE2ESessionStatusRequest) (*agentfleetv1.GetE2ESessionStatusResponse, error) {
	return &agentfleetv1.GetE2ESessionStatusResponse{Status: "running", Endpoints: f.endpoints}, nil
}

//nolint:staticcheck // SA1019: exercising the deprecated relay is the point — it is the fallback under test
func (f *fakeSandboxProvisioner) CallE2ETool(context.Context, *agentfleetv1.CallE2EToolRequest) (*agentfleetv1.CallE2EToolResponse, error) {
	f.relayCalls++
	return &agentfleetv1.CallE2EToolResponse{ResultJson: `{"content":[{"type":"text","text":"{\"stdout\":\"relayed\",\"exitCode\":0}"}]}`}, nil
}

func newSandboxFake(t *testing.T, endpoints []*agentfleetv1.ServiceEndpoint) (*fakeSandboxProvisioner, *Server) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	fake := &fakeSandboxProvisioner{endpoints: endpoints}
	agentfleetv1.RegisterProvisionerServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	client, err := provisionerclient.New(lis.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return fake, &Server{e2e: client}
}

// With a roster, core dials the sandbox itself and the provisioner relay is
// never touched. The dial fails here — the address is unroutable — and that
// is fine: the assertion is which path was taken, not whether a sandbox
// answered. A test that needed a live MCP server would be testing mcp-go.
func TestRunViaSandbox_WithRosterDoesNotRelay(t *testing.T) {
	fake, s := newSandboxFake(t, []*agentfleetv1.ServiceEndpoint{
		{Name: "exec", Address: "sandbox.invalid.:8932", Protocol: "mcp-streamable-http", Path: "/mcp"},
	})

	_, err := s.runViaSandbox(context.Background(), "task-1", "echo hi")
	if err == nil {
		t.Fatal("expected the unroutable direct dial to fail")
	}
	if fake.relayCalls != 0 {
		t.Errorf("relay calls = %d, want 0 — a roster must take core out of the provisioner's path", fake.relayCalls)
	}
}

// Without a roster — a provisioner too old to send one — core must keep
// working through the relay. This is the same deploy-skew contract the
// sidecar has, and core and the provisioner deploy independently too.
func TestRunViaSandbox_WithoutRosterFallsBackToRelay(t *testing.T) {
	fake, s := newSandboxFake(t, nil)

	out, err := s.runViaSandbox(context.Background(), "task-1", "echo hi")
	if err != nil {
		t.Fatalf("runViaSandbox: %v", err)
	}
	if fake.relayCalls != 1 {
		t.Errorf("relay calls = %d, want 1 — an empty roster must fall back", fake.relayCalls)
	}
	if out == "" {
		t.Error("expected the relay's result envelope")
	}
}

// A roster that exists but has no exec entry is the same as none for this
// path: core needs run_command specifically, and dialing the playwright port
// for it would reach a server that has never heard of the tool.
func TestRunViaSandbox_RosterWithoutExecFallsBack(t *testing.T) {
	fake, s := newSandboxFake(t, []*agentfleetv1.ServiceEndpoint{
		{Name: "playwright", Address: "sandbox.invalid.:8931", Path: "/mcp"},
	})

	if _, err := s.runViaSandbox(context.Background(), "task-1", "echo hi"); err != nil {
		t.Fatalf("runViaSandbox: %v", err)
	}
	if fake.relayCalls != 1 {
		t.Errorf("relay calls = %d, want 1 — no exec endpoint means no direct path for run_command", fake.relayCalls)
	}
}
