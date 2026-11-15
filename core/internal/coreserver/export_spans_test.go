package coreserver

import (
	"context"
	"net"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/workerspans"
)

// TestExportSpans_OverTheRealAuthChain runs ExportSpans through core's gRPC
// auth interceptors: a session's own lease exports, another session's name is
// refused, and the provisioner token (which writes nothing of a session's) is
// refused. A definition session may export too.
func TestExportSpans_OverTheRealAuthChain(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	sink := workerspans.NewWithExporter(exp, workerspans.DefaultLimits)
	srv := &Server{spans: sink}

	auth := NewAuthenticator(fakeLeases{session: "s1", lease: "l1"}, "ptok")
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(auth.UnaryInterceptor), grpc.ChainStreamInterceptor(auth.StreamInterceptor))
	agentfleetv1.RegisterCoreServiceServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := agentfleetv1.NewCoreServiceClient(conn)

	span := &agentfleetv1.WorkerSpan{Name: "invoke_agent", LocalId: "a", TurnKey: "t1", StartUnixNano: 10, EndUnixNano: 20}
	req := func(session string) *agentfleetv1.ExportSpansRequest {
		return &agentfleetv1.ExportSpansRequest{SessionId: session, Spans: []*agentfleetv1.WorkerSpan{span}}
	}
	own := metadata.AppendToOutgoingContext(context.Background(), mdSessionID, "s1", mdLeaseID, "l1")
	prov := metadata.AppendToOutgoingContext(context.Background(), mdProvisionerToken, "ptok")

	// The session's own lease and its own id: accepted.
	resp, err := c.ExportSpans(own, req("s1"))
	if err != nil || resp.GetAccepted() != 1 {
		t.Fatalf("own session: %v, %v", resp, err)
	}
	// A valid lease naming another session: the binder refuses it.
	if _, err := c.ExportSpans(own, req("s2")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("session mismatch: %v, want PermissionDenied", err)
	}
	// The provisioner token never writes spans, whatever session it names.
	if _, err := c.ExportSpans(prov, req("s1")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("provisioner token: %v, want PermissionDenied", err)
	}
	// No credential at all.
	if _, err := c.ExportSpans(context.Background(), req("s1")); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no credential: %v, want Unauthenticated", err)
	}
	t.Cleanup(sink.Close)
}

// TestExportSpans_DefinitionSessionMayExport: ExportSpans is in
// definitionMethods, since a definition pod runs the same worker and sidecar.
func TestExportSpans_DefinitionSessionMayExport(t *testing.T) {
	const m = "/agentfleet.v1.CoreService/ExportSpans"
	if !definitionMethods[m] || !sessionOnlyMethods[m] {
		t.Fatal("ExportSpans must be in definitionMethods and sessionOnlyMethods")
	}
	if _, ok := callerSessionOf[m]; !ok {
		t.Fatal("ExportSpans must be bound to the caller's session")
	}
}

func TestExportSpans_NoSinkAcceptsNothing(t *testing.T) {
	resp, err := (&Server{}).ExportSpans(context.Background(), &agentfleetv1.ExportSpansRequest{SessionId: "s1",
		Spans: []*agentfleetv1.WorkerSpan{{Name: "x", LocalId: "a", TurnKey: "t", StartUnixNano: 1, EndUnixNano: 2}}})
	if err != nil || resp.GetAccepted() != 0 {
		t.Fatalf("%v, %v", resp, err)
	}
}
