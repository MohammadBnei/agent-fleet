package grpcserver

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/metrics"
)

type versionOnly struct {
	agentfleetv1.UnimplementedProvisionerServiceServer
}

func (versionOnly) GetVersion(context.Context, *agentfleetv1.GetVersionRequest) (*agentfleetv1.GetVersionResponse, error) {
	return &agentfleetv1.GetVersionResponse{Version: "test"}, nil
}

// authConn serves ProvisionerService (unary) and grpc.health (Watch is a
// server stream, which ProvisionerService has none of yet) on the server main
// builds (NewGRPCServer), over bufconn.
func authConn(t *testing.T, token string) *grpc.ClientConn {
	t.Helper()
	srv, err := NewGRPCServer(token)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1024 * 1024)
	agentfleetv1.RegisterProvisionerServiceServer(srv, versionOnly{})
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// rightToken is MinTokenLen long, the shortest NewGRPCServer accepts.
const rightToken = "right-token-0123456789abcdef0123"

func TestAuth_WeakTokenRefusesToStart(t *testing.T) {
	for _, tok := range []string{"", " ", "\n", "x", rightToken[:MinTokenLen-1], strings.Repeat(" ", MinTokenLen)} {
		if _, err := NewGRPCServer(tok); err == nil {
			t.Errorf("NewGRPCServer(%q) succeeded; an empty, blank or short token must refuse to start", tok)
		}
	}
	if _, err := NewGRPCServer(rightToken); err != nil {
		t.Errorf("NewGRPCServer(%d-byte token): %v", len(rightToken), err)
	}
}

func TestAuth_Matrix(t *testing.T) {
	conn := authConn(t, rightToken)
	cases := []struct {
		name string
		md   []string // nil = no metadata at all
		want codes.Code
	}{
		{"missing", nil, codes.Unauthenticated},
		{"empty bearer", []string{"authorization", "Bearer "}, codes.Unauthenticated},
		{"wrong token", []string{"authorization", "Bearer wrong-token"}, codes.Unauthenticated},
		{"prefix of the token", []string{"authorization", "Bearer right"}, codes.Unauthenticated},
		{"no scheme", []string{"authorization", rightToken}, codes.Unauthenticated},
		{"other header", []string{"x-fleet-provisioner-token", rightToken}, codes.Unauthenticated},
		{"right token twice", []string{"authorization", "Bearer " + rightToken, "authorization", "Bearer " + rightToken}, codes.Unauthenticated},
		{"right token", []string{"authorization", "Bearer " + rightToken}, codes.OK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.md != nil {
				ctx = metadata.AppendToOutgoingContext(ctx, tc.md...)
			}

			_, err := agentfleetv1.NewProvisionerServiceClient(conn).GetVersion(ctx, &agentfleetv1.GetVersionRequest{})
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

// Every ProvisionerService method, enumerated from the generated descriptor so
// a method added later is covered without editing this test, must answer
// Unauthenticated without the token. versionOnly leaves all but GetVersion
// Unimplemented, so a method that skipped the check would answer that instead.
func TestAuth_EveryMethodNeedsTheToken(t *testing.T) {
	conn := authConn(t, rightToken)
	sd := agentfleetv1.ProvisionerService_ServiceDesc
	for _, m := range sd.Methods {
		err := conn.Invoke(context.Background(), "/"+sd.ServiceName+"/"+m.MethodName, &emptypb.Empty{}, &emptypb.Empty{})
		if got := status.Code(err); got != codes.Unauthenticated {
			t.Errorf("%s: code = %v, want Unauthenticated", m.MethodName, got)
		}
	}
	if len(sd.Streams) != 0 {
		t.Errorf("ProvisionerService grew a stream RPC: extend this test to call it without a token")
	}
}

// A rejected call is still counted and logged with who made it: the chain
// order in NewGRPCServer is what lets a probe of this port show up at all.
func TestAuth_RejectedCallIsCountedAndAttributed(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rejected := metrics.GrpcRequestsTotal.WithLabelValues("/agentfleet.v1.ProvisionerService/GetVersion", codes.Unauthenticated.String())
	before := testutil.ToFloat64(rejected)

	conn := authConn(t, rightToken)
	_, err := agentfleetv1.NewProvisionerServiceClient(conn).GetVersion(context.Background(), &agentfleetv1.GetVersionRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
	}

	if got := testutil.ToFloat64(rejected) - before; got != 1 {
		t.Errorf("rejected call counted %v times, want 1", got)
	}
	logs := buf.String()
	if !strings.Contains(logs, "rejected unauthenticated call") || strings.Contains(logs, "peer=unknown") || !strings.Contains(logs, "peer=") {
		t.Errorf("rejection not logged with a peer address:\n%s", logs)
	}
}
