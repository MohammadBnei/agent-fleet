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

	"google.golang.org/grpc"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/config"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/coreclient"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/git"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/grpcserver"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/mcpproxy"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/reconcile"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/sweep"
)

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No Postgres connection anywhere in this process — the provisioner
	// holds no DB credentials at all (docs/adr/0020 point 1). Kubernetes
	// itself is the durable source of truth for pod/session state.
	k8sc, err := k8s.New(cfg.Namespace, k8s.Images{
		RunnerImage:           cfg.E2eRunnerImage,
		WorkerImage:           cfg.WorkerImage,
		SidecarImage:          cfg.SidecarImage,
		WorkspacePVC:          cfg.WorkspacePVC,
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
	proxy := mcpproxy.New(
		func(taskID string) string { return k8s.PlaywrightURLFor(cfg.Namespace, taskID) },
		func(taskID string) string { return k8s.ExecURLFor(cfg.Namespace, taskID) },
	)
	gitMgr := git.NewManager(cfg.WorktreesRoot)
	if err := gitMgr.ConfigureAuth(ctx); err != nil {
		slog.Error("git auth configuration failed", "error", err)
		os.Exit(1)
	}

	core, err := coreclient.New(cfg.CoreGRPCAddr)
	if err != nil {
		slog.Error("core client init failed", "error", err)
		os.Exit(1)
	}
	defer func() { _ = core.Close() }()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	httpServer := &http.Server{Addr: ":" + cfg.Port, Handler: mux}

	grpcSrv := grpc.NewServer(grpc.UnaryInterceptor(grpcserver.AccessLogInterceptor))
	agentfleetv1.RegisterProvisionerServiceServer(grpcSrv, grpcserver.New(k8sc, gitMgr, proxy, core, cfg.E2eHost, cfg.FleetSharedRepoURL, cfg.FleetSharedBranch, cfg.ClaudeHomeDir))

	reconcileInterval, _ := strconv.Atoi(cfg.ReconcileInterval)
	loop := reconcile.New(k8sc, core)
	go loop.Run(ctx, time.Duration(reconcileInterval)*time.Millisecond)

	sweepInterval, _ := strconv.Atoi(cfg.SweepInterval)
	sweepLoop := sweep.New(gitMgr)
	go sweepLoop.Run(ctx, time.Duration(sweepInterval)*time.Millisecond)

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
