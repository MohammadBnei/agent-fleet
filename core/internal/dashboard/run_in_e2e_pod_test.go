package dashboard

import (
	"context"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
)

// fakeSandboxProvisioner answers GetE2ESessionStatus with a configurable
// roster. No Postgres needed: runViaSandbox touches only the provisioner
// client, so the Server is built directly rather than through NewServer.
//
// It deliberately implements no tool-call method — there is none left on
// ProvisionerService to implement (docs/adr/0045).
type fakeSandboxProvisioner struct {
	agentfleetv1.UnimplementedProvisionerServiceServer
	endpoints []*agentfleetv1.ServiceEndpoint
}

func (f *fakeSandboxProvisioner) GetE2ESessionStatus(context.Context, *agentfleetv1.GetE2ESessionStatusRequest) (*agentfleetv1.GetE2ESessionStatusResponse, error) {
	return &agentfleetv1.GetE2ESessionStatusResponse{Status: "running", Endpoints: f.endpoints}, nil
}

func newSandboxFake(t *testing.T, endpoints []*agentfleetv1.ServiceEndpoint) *Server {
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
	return &Server{e2e: client}
}

// With a roster, core dials the sandbox itself. The dial fails here — the
// address is unroutable — and the error must come from the DIAL, naming the
// address, rather than from anything provisioner-shaped. A test that needed a
// live MCP server would be testing mcp-go.
func TestRunViaSandbox_WithRosterDialsTheSandbox(t *testing.T) {
	s := newSandboxFake(t, []*agentfleetv1.ServiceEndpoint{
		{Name: "exec", Address: "sandbox.invalid.:8932", Protocol: "mcp-streamable-http", Path: "/mcp"},
	})

	_, err := s.runViaSandbox(context.Background(), "task-1", "echo hi")
	if err == nil {
		t.Fatal("expected the unroutable direct dial to fail")
	}
	if !strings.Contains(err.Error(), "sandbox.invalid.") {
		t.Errorf("error %q should come from the direct dial and name the address", err)
	}
}

// Without a roster there is nothing left to fall back to (docs/adr/0045), so
// this must fail with a message a human can act on — these two handlers are
// the dashboard's only window into a sandbox.
func TestRunViaSandbox_WithoutRosterFailsLoudly(t *testing.T) {
	s := newSandboxFake(t, nil)

	if _, err := s.runViaSandbox(context.Background(), "task-1", "echo hi"); err == nil {
		t.Fatal("an empty roster must fail — the relay it used to fall back to is deleted")
	} else if !strings.Contains(err.Error(), "no exec endpoint") {
		t.Errorf("error %q should say plainly that there is no sandbox to reach", err)
	}
}

// A roster that exists but has no exec entry is the same as none for this
// path: core needs run_command specifically, and dialing the playwright port
// for it would reach a server that has never heard of the tool.
func TestRunViaSandbox_RosterWithoutExecFails(t *testing.T) {
	s := newSandboxFake(t, []*agentfleetv1.ServiceEndpoint{
		{Name: "playwright", Address: "sandbox.invalid.:8931", Path: "/mcp"},
	})

	if _, err := s.runViaSandbox(context.Background(), "task-1", "echo hi"); err == nil {
		t.Fatal("a roster without an exec endpoint must fail rather than dialing playwright")
	}
}
