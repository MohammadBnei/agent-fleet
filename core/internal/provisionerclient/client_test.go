package provisionerclient

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

type headerRecorder struct {
	agentfleetv1.UnimplementedProvisionerServiceServer
	got chan []string
}

func (h *headerRecorder) GetVersion(ctx context.Context, _ *agentfleetv1.GetVersionRequest) (*agentfleetv1.GetVersionResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	h.got <- md.Get("authorization")
	return &agentfleetv1.GetVersionResponse{}, nil
}

// The provisioner rejects a call without this header (docs/adr/0069), so a
// client that forgets it takes the whole fleet's pod lifecycle down.
//
// ponytail: unary only. ProvisionerService has no stream RPC to call, and the
// header comes from grpc.WithPerRPCCredentials, which grpc-go applies to
// streams the same way.
func TestClientSendsBearerToken(t *testing.T) {
	for _, tc := range []struct {
		token string
		want  []string
	}{
		{"s3cret", []string{"Bearer s3cret"}},
		{"", nil}, // no header at all, not a bare "Bearer "
	} {
		if got := recordHeader(t, tc.token); len(got) != len(tc.want) || (len(got) == 1 && got[0] != tc.want[0]) {
			t.Errorf("token %q: authorization = %q, want %q", tc.token, got, tc.want)
		}
	}
}

func recordHeader(t *testing.T, token string) []string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rec := &headerRecorder{got: make(chan []string, 1)}
	srv := grpc.NewServer()
	agentfleetv1.RegisterProvisionerServiceServer(srv, rec)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	c, err := New(lis.Addr().String(), token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	if _, _, _, err := c.GetVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
	return <-rec.got
}
