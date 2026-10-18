package main

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// An empty THOT_AUTH_TOKEN used to mean "serve everyone" — on the one process
// holding cluster RBAC (docs/adr/0069).
func TestAuthInterceptor_EmptyTokenRefusesToStart(t *testing.T) {
	if _, err := newAuthInterceptor(""); err == nil {
		t.Fatal("newAuthInterceptor(\"\") succeeded; an empty token must refuse to start")
	}
}

func TestAuthInterceptor_Matrix(t *testing.T) {
	intercept, err := newAuthInterceptor("right-token")
	if err != nil {
		t.Fatal(err)
	}
	handler := func(context.Context, any) (any, error) { return "ran", nil }
	cases := []struct {
		name string
		md   metadata.MD // nil = no metadata at all
		want codes.Code
	}{
		{"no metadata", nil, codes.Unauthenticated},
		{"no header", metadata.Pairs("x-other", "right-token"), codes.Unauthenticated},
		{"empty bearer", metadata.Pairs("authorization", "Bearer "), codes.Unauthenticated},
		{"wrong token", metadata.Pairs("authorization", "Bearer wrong-token"), codes.Unauthenticated},
		{"prefix of the token", metadata.Pairs("authorization", "Bearer right"), codes.Unauthenticated},
		{"no scheme", metadata.Pairs("authorization", "right-token"), codes.Unauthenticated},
		{"right token", metadata.Pairs("authorization", "Bearer right-token"), codes.OK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.md != nil {
				ctx = metadata.NewIncomingContext(ctx, tc.md)
			}
			resp, err := intercept(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/agentfleet.v1.ExecutorService/Exec"}, handler)
			if got := status.Code(err); got != tc.want {
				t.Fatalf("code = %v, want %v (err %v)", got, tc.want, err)
			}
			if (resp == "ran") != (tc.want == codes.OK) {
				t.Fatalf("handler ran = %v, want %v", resp == "ran", tc.want == codes.OK)
			}
		})
	}
}
