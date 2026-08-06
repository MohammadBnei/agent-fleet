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
	rawLogLevel := env("LOG_LEVEL", "info")
	level, warning := resolveLogLevel(rawLogLevel)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
	if warning != "" {
		slog.Warn(warning, "value", rawLogLevel)
	}

	taskID := os.Getenv("TASK_ID")
	if taskID == "" {
		slog.Error("TASK_ID is required")
		os.Exit(1)
	}
	coreAddr := env("CORE_GRPC_ADDR", "agent-fleet-core.agent-fleet.svc.cluster.local:9090")
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

	// Backs /readyz: the provisioner's StartupProbe waits on this before
	// unblocking the worker container, so a core outage/rollout blocks the
	// worker from starting instead of it crashing on its first sidecar call
	// (observed live: worker died in <1s on an unguarded setStatus call
	// while core's Service briefly had zero endpoints).
	go func() {
		if err := core.WaitReady(ctx); err != nil {
			slog.Warn("core.WaitReady did not complete", "error", err)
		}
	}()

	go telemetry.Run(ctx, core, worktreePath, 5*time.Second)

	mcpServer := &http.Server{Addr: ":" + mcpPort, Handler: withAccessLog("sidecar mcp", mcpserver.New(core))}
	localAPIServer := &http.Server{Addr: ":" + localAPIPort, Handler: withAccessLog("sidecar local api", localapi.New(core))}

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

// withAccessLog logs one line per request (method, path, status, duration)
// — both of sidecar's HTTP muxes had zero request-level logging before.
func withAccessLog(name string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)
		slog.Info(name, "method", r.Method, "path", r.URL.Path, "status", sw.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Flush forwards to the underlying ResponseWriter's Flusher. Without this,
// embedding http.ResponseWriter as an interface field doesn't promote
// Flush() — humanMessagesHandler's `w.(http.Flusher)` assertion always
// failed through this wrapper, so /human-messages always returned 500
// (confirmed live: every worker session's approve/abort signal silently
// never arrived, forcing the agent to discover approval by polling the
// transcript instead).
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
