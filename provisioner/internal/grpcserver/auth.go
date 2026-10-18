package grpcserver

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/metrics"
)

// MinTokenLen is the shortest bearer accepted at boot. The live value is 32
// random bytes, hex (64 characters).
const MinTokenLen = 32

// NewGRPCServer is the provisioner's gRPC server, the one main serves and the
// tests exercise. It requires `authorization: Bearer
// <FLEET_CORE_PROVISIONER_TOKEN>` on every ProvisionerService call
// (docs/adr/0069). Before it, this port authenticated nobody: any pod that
// could reach the Service could create pods, expose them to the internet or be
// handed a DSN.
//
// The unary chain is access log, metrics, then the token check, so a rejected
// unary call is still logged and counted. The stream chain is the check alone:
// there is no stream RPC to count yet, so a rejected stream call gets only the
// warning line below. The check is method-agnostic: there is no
// exempt list, so a method added later needs the token without anyone
// remembering to say so. The stream half guards the first streaming RPC before
// it exists, the hole ADR-0057 found in core.
//
// A token shorter than MinTokenLen (after trimming whitespace) refuses to
// start rather than serving everyone, or serving whoever guesses " ". The
// Infisical operator rendering an empty Secret is a known failure, and "off
// when unset" would come up wide open and look healthy (the ADR-0056
// reasoning).
//
// ponytail: the bearer check is twinned in executor/cmd/executor/main.go
// (newGRPCServer). They are separate go.work modules with no shared package,
// so a fix to one must be made in the other; factor it out if a third gRPC
// server needs it.
func NewGRPCServer(token string) (*grpc.Server, error) {
	if len(strings.TrimSpace(token)) < MinTokenLen {
		return nil, fmt.Errorf("FLEET_CORE_PROVISIONER_TOKEN is unset or shorter than %d bytes: refusing to serve ProvisionerService", MinTokenLen)
	}
	want := []byte("Bearer " + token)
	check := func(ctx context.Context, method string) error {
		md, _ := metadata.FromIncomingContext(ctx)
		vals := md.Get("authorization")
		if len(vals) == 1 && subtle.ConstantTimeCompare([]byte(vals[0]), want) == 1 {
			return nil
		}
		// The access log has no caller address; this line is what attributes a probe.
		addr := "unknown"
		if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
			addr = p.Addr.String()
		}
		slog.Warn("provisioner grpc: rejected unauthenticated call", "method", method, "peer", addr)
		return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
	}
	return grpc.NewServer(
		grpc.ChainUnaryInterceptor(AccessLogInterceptor, metrics.UnaryInterceptor,
			func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
				if err := check(ctx, info.FullMethod); err != nil {
					return nil, err
				}
				return handler(ctx, req)
			}),
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			if err := check(ss.Context(), info.FullMethod); err != nil {
				return err
			}
			return handler(srv, ss)
		}),
	), nil
}
