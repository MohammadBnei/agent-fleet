package grpcserver

import (
	"context"
	"crypto/subtle"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Auth requires `authorization: Bearer <FLEET_CORE_PROVISIONER_TOKEN>` on
// every ProvisionerService call (docs/adr/0069). Before it, this port
// authenticated nobody: any pod that could reach the Service could create
// pods, expose them to the internet or be handed a DSN.
//
// Method-agnostic on purpose: there is no exempt list, so a method added later
// needs the token without anyone remembering to say so. The stream half guards
// the first streaming RPC before it exists, the hole ADR-0057 found in core.
type Auth struct {
	want []byte
}

// NewAuth refuses an empty token rather than serving everyone. The Infisical
// operator rendering an empty Secret is a known failure, and "off when unset"
// would come up wide open and look healthy (the ADR-0056 reasoning).
func NewAuth(token string) (*Auth, error) {
	if token == "" {
		return nil, errors.New("FLEET_CORE_PROVISIONER_TOKEN is unset: refusing to serve ProvisionerService unauthenticated")
	}
	return &Auth{want: []byte("Bearer " + token)}, nil
}

func (a *Auth) Unary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := a.check(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (a *Auth) Stream(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := a.check(ss.Context()); err != nil {
		return err
	}
	return handler(srv, ss)
}

func (a *Auth) check(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("authorization")
	if len(vals) != 1 || subtle.ConstantTimeCompare([]byte(vals[0]), a.want) != 1 {
		return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
	}
	return nil
}
