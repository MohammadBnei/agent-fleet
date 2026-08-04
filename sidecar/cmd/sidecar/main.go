package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/localapi"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/mcpserver"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/telemetry"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	taskID := os.Getenv("TASK_ID")
	if taskID == "" {
		slog.Error("TASK_ID is required")
		os.Exit(1)
	}
	coreAddr := env("CORE_GRPC_ADDR", "core.agent-fleet.svc.cluster.local:9090")
	worktreePath := env("WORKTREE_PATH", "/workspace")
	mcpPort := env("MCP_PORT", "9090")
	localAPIPort := env("LOCAL_API_PORT", "9091")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	core, err := coreclient.New(coreAddr, taskID)
	if err != nil {
		slog.Error("core client init failed", "error", err)
		os.Exit(1)
	}
	defer func() { _ = core.Close() }()

	go telemetry.Run(ctx, core, worktreePath, 5*time.Second)

	mcpServer := &http.Server{Addr: ":" + mcpPort, Handler: mcpserver.New(core)}
	localAPIServer := &http.Server{Addr: ":" + localAPIPort, Handler: localapi.New(core)}

	go func() {
		slog.Info("sidecar mcp listening", "port", mcpPort)
		if err := mcpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("mcp server exited", "error", err)
		}
	}()
	go func() {
		slog.Info("sidecar local api listening", "port", localAPIPort)
		if err := localAPIServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("local api server exited", "error", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = mcpServer.Shutdown(shutdownCtx)
	_ = localAPIServer.Shutdown(shutdownCtx)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
