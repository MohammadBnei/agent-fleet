// Command executor is the fleet's only process holding cluster RBAC on
// behalf of agents (ADR-0037, superseding ADR-0035).
//
// It exists so a thot session can be an ordinary worker pod with zero
// Kubernetes credentials — the ADR-0012 pattern, where the thing holding
// write-in-git trust never also holds infra-mutation trust. Its identity
// comes from the ServiceAccount on its own Deployment, defined and
// reviewed in infra-bootstrap's gitops, never created or named by the
// provisioner.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/executor/internal/config"
	"github.com/MohammadBnei/agent-fleet/executor/internal/kubectl"
	"github.com/MohammadBnei/ukubi-go/telemetry"
)

type server struct {
	agentfleetv1.UnimplementedExecutorServiceServer
}

func (s *server) Exec(ctx context.Context, req *agentfleetv1.ExecRequest) (*agentfleetv1.ExecResponse, error) {
	args := req.GetArgs()

	if req.GetReadOnly() {
		// Unattended path: kubectl_read bypasses canUseTool by design, so
		// nothing else is checking this one.
		if refusal := kubectl.ValidateReadOnly(args); refusal != "" {
			slog.Warn("executor refused read-only request", "args", args, "reason", refusal)
			return &agentfleetv1.ExecResponse{Refused: refusal, ExitCode: -1}, nil
		}
	} else {
		// Human-gated path: a person approved this exact argv through
		// canUseTool. Deliberately a dumb pipe — see executor.proto.
		slog.Info("executor running human-approved command", "args", args)
	}

	res := kubectl.Run(ctx, args)
	return &agentfleetv1.ExecResponse{
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: int32(res.ExitCode),
	}, nil
}

// minTokenLen is the shortest bearer accepted at boot. The live value is 64
// characters.
const minTokenLen = 32

// newGRPCServer rejects anything without the shared bearer token, on unary
// and stream calls alike. This process is the most privileged in the fleet, so
// it never relies on network reachability alone (ADR-0035 flagged that
// precedent as not scaling to cluster-mutation power). ExecutorService has no
// stream RPC today; the stream half means the first one is not silently open,
// the hole ADR-0057 found in core. main and the tests both build the server
// here, so the tested wiring is the shipped wiring.
//
// An empty, blank or short token is an error, not an open mode
// (docs/adr/0069): it used to pass every call with a warning, so an Infisical
// operator rendering an empty Secret would have brought cluster RBAC up for
// anyone, looking healthy.
//
// ponytail: the bearer check is twinned in
// provisioner/internal/grpcserver/auth.go (NewGRPCServer). They are separate
// go.work modules with no shared package, so a fix to one must be made in the
// other; factor it out if a third gRPC server needs it (ADR-0072).
//
// opts are added after the interceptors: main passes its tracing's
// ServerOption.
func newGRPCServer(token string, opts ...grpc.ServerOption) (*grpc.Server, error) {
	if len(strings.TrimSpace(token)) < minTokenLen {
		return nil, fmt.Errorf("THOT_AUTH_TOKEN is unset or shorter than %d bytes: refusing to serve ExecutorService", minTokenLen)
	}
	want := []byte("Bearer " + token)
	check := func(ctx context.Context) error {
		md, _ := metadata.FromIncomingContext(ctx)
		vals := md.Get("authorization")
		if len(vals) != 1 || subtle.ConstantTimeCompare([]byte(vals[0]), want) != 1 {
			return status.Error(codes.Unauthenticated, "missing or invalid bearer token")
		}
		return nil
	}
	return grpc.NewServer(append([]grpc.ServerOption{
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			if err := check(ctx); err != nil {
				return nil, err
			}
			return handler(ctx, req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			if err := check(ss.Context()); err != nil {
				return err
			}
			return handler(srv, ss)
		}),
	}, opts...)...), nil
}

func main() {
	slog.SetDefault(telemetry.NewLogger(os.Stdout))

	// The environment is read here and nowhere else (internal/config).
	cfg := config.ExecutorFromProcess()
	grpcPort := cfg.GRPCPort
	httpPort := cfg.HTTPPort
	// Opt-in OTLP export (docs/adr/0081): inert without the variable. Built
	// first because the server takes its option, refused after the token: a
	// nil Tracing gives no option.
	tr, trErr := telemetry.New(cfg.TracesEndpoint, "thot-executor")
	grpcServer, err := newGRPCServer(cfg.AuthToken, tr.ServerOption())
	if err != nil {
		slog.Error("refusing to start", "error", err)
		os.Exit(1)
	}
	if trErr != nil {
		slog.Error("refusing to start", "error", trErr)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	agentfleetv1.RegisterExecutorServiceServer(grpcServer, &server{})
	lis, err := net.Listen("tcp", ":"+grpcPort)
	if err != nil {
		slog.Error("listen", "error", err)
		os.Exit(1)
	}
	go func() {
		slog.Info("executor gRPC listening", "port", grpcPort)
		if err := grpcServer.Serve(lis); err != nil {
			slog.Error("grpc server exited", "error", err)
		}
	}()
	// Deferred before GracefulStop, so it runs after: the last calls' spans
	// are in the batch.
	defer func() {
		if err := tr.Shutdown(); err != nil {
			slog.Warn("traces", "err", err)
		}
	}()
	defer grpcServer.GracefulStop()

	// Liveness only — deliberately not gated on anything downstream, the
	// lesson from thot's own crashloop (a probe that waits on a slow
	// dependency kills the pod before that dependency can ever finish).
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	httpServer := &http.Server{Addr: ":" + httpPort, Handler: mux, ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)}
	go func() {
		slog.Info("executor health listening", "port", httpPort)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server exited", "error", err)
		}
	}()

	<-ctx.Done()
	_ = httpServer.Close()
}
