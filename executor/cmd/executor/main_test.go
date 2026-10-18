package main

import (
	"context"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// rightToken is minTokenLen long, the shortest newGRPCServer accepts.
const rightToken = "right-token-0123456789abcdef0123"

// An empty THOT_AUTH_TOKEN used to mean "serve everyone" — on the one process
// holding cluster RBAC (docs/adr/0069). A blank or short one is no better.
func TestGRPCServer_WeakTokenRefusesToStart(t *testing.T) {
	for _, tok := range []string{"", " ", "\n", "x", rightToken[:minTokenLen-1], strings.Repeat(" ", minTokenLen)} {
		if _, err := newGRPCServer(tok); err == nil {
			t.Errorf("newGRPCServer(%q) succeeded; an empty, blank or short token must refuse to start", tok)
		}
	}
	if _, err := newGRPCServer(rightToken); err != nil {
		t.Errorf("newGRPCServer(%d-byte token): %v", len(rightToken), err)
	}
}

// fakeExec stands in for server so the OK case runs no kubectl.
type fakeExec struct {
	agentfleetv1.UnimplementedExecutorServiceServer
}

func (fakeExec) Exec(context.Context, *agentfleetv1.ExecRequest) (*agentfleetv1.ExecResponse, error) {
	return &agentfleetv1.ExecResponse{}, nil
}

// Runs the matrix against the server main builds, over bufconn: Exec (unary)
// and grpc.health Watch (a server stream, which ExecutorService has none of
// yet) on the same server.
func TestGRPCServer_Matrix(t *testing.T) {
	srv, err := newGRPCServer(rightToken)
	if err != nil {
		t.Fatal(err)
	}
	agentfleetv1.RegisterExecutorServiceServer(srv, fakeExec{})
	healthpb.RegisterHealthServer(srv, health.NewServer())
	lis := bufconn.Listen(1024 * 1024)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	cases := []struct {
		name string
		md   []string // nil = no metadata at all
		want codes.Code
	}{
		{"no metadata", nil, codes.Unauthenticated},
		{"no header", []string{"x-other", rightToken}, codes.Unauthenticated},
		{"empty bearer", []string{"authorization", "Bearer "}, codes.Unauthenticated},
		{"wrong token", []string{"authorization", "Bearer wrong-token"}, codes.Unauthenticated},
		{"prefix of the token", []string{"authorization", "Bearer right"}, codes.Unauthenticated},
		{"no scheme", []string{"authorization", rightToken}, codes.Unauthenticated},
		{"right token twice", []string{"authorization", "Bearer " + rightToken, "authorization", "Bearer " + rightToken}, codes.Unauthenticated},
		{"right token", []string{"authorization", "Bearer " + rightToken}, codes.OK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.md != nil {
				ctx = metadata.AppendToOutgoingContext(ctx, tc.md...)
			}

			_, err := agentfleetv1.NewExecutorServiceClient(conn).Exec(ctx, &agentfleetv1.ExecRequest{})
			if got := status.Code(err); got != tc.want {
				t.Errorf("unary: code = %v, want %v (err %v)", got, tc.want, err)
			}

			stream, err := healthpb.NewHealthClient(conn).Watch(ctx, &healthpb.HealthCheckRequest{})
			if err == nil {
				_, err = stream.Recv()
			}
			if got := status.Code(err); got != tc.want {
				t.Errorf("stream: code = %v, want %v (err %v)", got, tc.want, err)
			}
		})
	}
}
