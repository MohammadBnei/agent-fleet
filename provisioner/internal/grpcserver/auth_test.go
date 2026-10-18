package grpcserver

import (
	"context"
	"net"
	"testing"

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
)

type versionOnly struct {
	agentfleetv1.UnimplementedProvisionerServiceServer
}

func (versionOnly) GetVersion(context.Context, *agentfleetv1.GetVersionRequest) (*agentfleetv1.GetVersionResponse, error) {
	return &agentfleetv1.GetVersionResponse{Version: "test"}, nil
}

// authConn serves ProvisionerService (unary) and grpc.health (Watch is a
// server stream, which ProvisionerService has none of yet) on the server main
// builds (NewServer), over bufconn.
func authConn(t *testing.T, token string) *grpc.ClientConn {
	t.Helper()
	srv, err := NewServer(token, AccessLogInterceptor)
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

func TestAuth_EmptyTokenRefusesToStart(t *testing.T) {
	if _, err := NewServer(""); err == nil {
		t.Fatal("NewServer(\"\") succeeded; an empty token must refuse to start, not serve everyone")
	}
}

func TestAuth_Matrix(t *testing.T) {
	conn := authConn(t, "right-token")
	cases := []struct {
		name string
		md   []string // nil = no metadata at all
		want codes.Code
	}{
		{"missing", nil, codes.Unauthenticated},
		{"empty bearer", []string{"authorization", "Bearer "}, codes.Unauthenticated},
		{"wrong token", []string{"authorization", "Bearer wrong-token"}, codes.Unauthenticated},
		{"prefix of the token", []string{"authorization", "Bearer right"}, codes.Unauthenticated},
		{"no scheme", []string{"authorization", "right-token"}, codes.Unauthenticated},
		{"other header", []string{"x-fleet-provisioner-token", "right-token"}, codes.Unauthenticated},
		{"right token", []string{"authorization", "Bearer right-token"}, codes.OK},
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
	conn := authConn(t, "right-token")
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

// An unregistered method never reaches a handler. grpc-go answers it
// Unimplemented before any interceptor runs (no UnknownServiceHandler is set),
// which is closed: there is nothing behind it to reach.
func TestAuth_UnknownMethodFailsClosed(t *testing.T) {
	conn := authConn(t, "right-token")
	for _, md := range [][]string{nil, {"authorization", "Bearer right-token"}} {
		ctx := context.Background()
		if md != nil {
			ctx = metadata.AppendToOutgoingContext(ctx, md...)
		}
		err := conn.Invoke(ctx, "/agentfleet.v1.ProvisionerService/NoSuchMethod", &emptypb.Empty{}, &emptypb.Empty{})
		if status.Code(err) == codes.OK {
			t.Errorf("unknown method with metadata %v answered OK", md)
		}
	}
}
