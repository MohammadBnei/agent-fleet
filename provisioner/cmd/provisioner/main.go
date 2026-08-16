package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/config"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/coreclient"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/git"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/grpcserver"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/metrics"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/reconcile"
)

// version is stamped at image build time via
// `-ldflags "-X main.version=$VERSION"` (see provisioner/Dockerfile and
// .github/workflows/docker.yml, which passes the same string it tags the image
// with). "dev" for a plain local `go build` — there is no other source: the
// binary is built from a partial context with no .git, so debug.ReadBuildInfo
// has no VCS stamp to read.
var version = "dev"

func main() {
	cfg := config.Load()

	// JSON, not slog's default TextHandler — Loki/LogQL queries against
	// this fleet expect structured logs (same convention the TS services'
	// log.ts already used). Level is LOG_LEVEL-configurable (debug/info/
	// warn/error).
	level, warning := resolveLogLevel(cfg.LogLevel)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
	if warning != "" {
		slog.Warn(warning, "value", cfg.LogLevel)
	}
	slog.Info("provisioner starting", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Every file/dir this process creates on the shared workspace PVC
	// (git clone, worktree add — internal/git) must stay writable by the
	// worker container too, which runs as a non-root user (required for
	// Claude Code's own bypassPermissions mode — it refuses to run as
	// root/sudo). Provisioner and worker are separate containers with no
	// coordinated UID/GID, so world-writable is the simplest fix that
	// doesn't need fsGroup/GID plumbing across two services on a
	// single-tenant, internal-only PVC.
	syscall.Umask(0)

	// No Postgres connection anywhere in this process — the provisioner
	// holds no DB credentials at all (docs/adr/0020 point 1). Kubernetes
	// itself is the durable source of truth for pod/session state.
	k8sc, err := k8s.New(cfg.Namespace, k8s.Images{
		WorkerImage:           cfg.WorkerImage,
		SidecarImage:          cfg.SidecarImage,
		ThotAuthToken:         cfg.ThotAuthToken,
		ExecutorAddr:          cfg.ExecutorAddr,
		WorkspacePVC:          cfg.WorkspacePVC,
		SessionStorageClass:   cfg.SessionStorageClass,
		SessionNodeSelector:   cfg.NodeSelectorMap(),
		LogLevel:              cfg.LogLevel,
		CoreGRPCAddr:          cfg.CoreGRPCAddr,
		PostgresImage:         cfg.PostgresImage,
		RedisImage:            cfg.RedisImage,
		SharedInstancePVCSize: cfg.SharedInstancePVCSize,
	})
	if err != nil {
		slog.Error("k8s client init failed", "error", err)
		os.Exit(1)
	}

	// The shared cluster-ip-whitelist Middleware used to be created here, and
	// its failure was fatal. It existed so worker pods could reach each
	// other's preview URLs without a basic-auth prompt — which stopped being
	// a thing when the sandbox merged into the session pod (docs/adr/0048 §6):
	// an agent testing its own app reaches it on localhost, in its own pod.
	//
	// Nothing referenced the Middleware any more; expose()'s route carries
	// basic auth alone. Removing it also removes a startup dependency on a
	// Traefik CRD, which is one less thing the local harness has to fake.

	gitMgr := git.NewManager(cfg.WorkspaceRoot)
	if err := gitMgr.ConfigureAuth(ctx); err != nil {
		slog.Error("git auth configuration failed", "error", err)
		os.Exit(1)
	}

	// Fill the shared PVC's Playwright browser cache, which every worker pod
	// mounts read-only (pod.go's browsersDir). Best-effort and NOT fatal: a
	// fleet whose browser cache is still filling cannot browse yet, which is
	// strictly better than a provisioner that refuses to start over it. The
	// Job itself is idempotent, so this is a no-op once the cache is warm.
	if err := k8sc.EnsureBrowserCache(ctx); err != nil {
		slog.Warn("browser cache job not ensured — sessions will have no browser until this succeeds",
			"error", err)
	}

	core, err := coreclient.New(cfg.CoreGRPCAddr)
	if err != nil {
		slog.Error("core client init failed", "error", err)
		os.Exit(1)
	}
	defer func() { _ = core.Close() }()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("/metrics", promhttp.Handler())
	httpServer := &http.Server{Addr: ":" + cfg.Port, Handler: mux}

	grpcSrv := grpc.NewServer(grpc.ChainUnaryInterceptor(grpcserver.AccessLogInterceptor, metrics.UnaryInterceptor))
	agentfleetv1.RegisterProvisionerServiceServer(grpcSrv, grpcserver.New(k8sc, gitMgr, core, cfg.E2eHost, cfg.FleetSharedRepoURL, cfg.FleetSharedBranch, version))

	reconcileInterval, _ := strconv.Atoi(cfg.ReconcileInterval)
	sharedInstanceIdleTimeout, _ := strconv.Atoi(cfg.SharedInstanceIdleTimeoutMs)
	loop := reconcile.New(k8sc, core, time.Duration(sharedInstanceIdleTimeout)*time.Millisecond)
	go loop.Run(ctx, time.Duration(reconcileInterval)*time.Millisecond)

	// The branch/worktree sweep loop is gone (docs/adr/0048 §5). It deleted
	// worktrees on its own ticker, without telling core — which is why any
	// claim that a session knew whether its disk still existed was false, and
	// why swept_at could not be trusted. core is the single writer of that
	// fact now, and the only thing that decides a session's disk should go.

	go func() {
		lis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
		if err != nil {
			slog.Error("grpc listen failed", "error", err)
			return
		}
		slog.Info("provisioner grpc listening", "port", cfg.GRPCPort)
		if err := grpcSrv.Serve(lis); err != nil {
			slog.Error("grpc serve failed", "error", err)
		}
	}()

	go func() {
		slog.Info("provisioner http listening", "port", cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http serve failed", "error", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	grpcSrv.GracefulStop()
}

// resolveLogLevel parses raw (LOG_LEVEL) via slog.Level.UnmarshalText
// (DEBUG/INFO/WARN/ERROR, case-insensitive) and falls back to Info on
// anything invalid or empty. warning is non-empty when the fallback fired,
// so the caller can log it once the level-appropriate handler exists.
func resolveLogLevel(raw string) (level slog.Level, warning string) {
	if raw == "" {
		return slog.LevelInfo, ""
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(raw)); err != nil {
		return slog.LevelInfo, "invalid LOG_LEVEL, defaulting to info"
	}
	return l, ""
}
