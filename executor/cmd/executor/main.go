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
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/executor/internal/kubectl"
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

// newAuthInterceptor rejects anything without the shared bearer token. This
// process is the most privileged in the fleet, so it never relies on network
// reachability alone (ADR-0035 flagged that precedent as not scaling to
// cluster-mutation power).
//
// An empty token is an error, not an open mode (docs/adr/0069): it used to
// pass every call with a warning, so an Infisical operator rendering an empty
// Secret would have brought cluster RBAC up for anyone, looking healthy.
func newAuthInterceptor(token string) (grpc.UnaryServerInterceptor, error) {
	if token == "" {
		return nil, errors.New("THOT_AUTH_TOKEN is unset: refusing to serve ExecutorService unauthenticated")
	}
	want := []byte("Bearer " + token)
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		vals := md.Get("authorization")
		if len(vals) != 1 || subtle.ConstantTimeCompare([]byte(vals[0]), want) != 1 {
			return nil, status.Error(codes.Unauthenticated, "missing or invalid bearer token")
		}
		return handler(ctx, req)
	}, nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	grpcPort := env("GRPC_PORT", "9090")
	httpPort := env("HTTP_PORT", "8080")
	auth, err := newAuthInterceptor(os.Getenv("THOT_AUTH_TOKEN"))
	if err != nil {
		slog.Error("refusing to start", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(auth))
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
	defer grpcServer.GracefulStop()

	// Liveness only — deliberately not gated on anything downstream, the
	// lesson from thot's own crashloop (a probe that waits on a slow
	// dependency kills the pod before that dependency can ever finish).
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	httpServer := &http.Server{Addr: ":" + httpPort, Handler: mux}
	go func() {
		slog.Info("executor health listening", "port", httpPort)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server exited", "error", err)
		}
	}()

	<-ctx.Done()
	_ = httpServer.Close()
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
