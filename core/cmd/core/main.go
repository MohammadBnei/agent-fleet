package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/MohammadBnei/agent-fleet/core/internal/config"
	"github.com/MohammadBnei/agent-fleet/core/internal/db"
)

// version is the release this binary runs as: FLEET_VERSION, which deploy
// sets on the Deployment (.github/workflows/docker.yml), or "dev" without it.
// Not compiled in any more: an image is tagged by a hash of its inputs and
// reused across releases that don't touch them, so the binary cannot know
// which release it is part of.
//
// It covers the dashboard SPA too: that is compiled into this binary
// (core/Dockerfile's spa stage → go:embed), so it has no version of its own.
var version = "dev"

func main() {
	if v := os.Getenv("FLEET_VERSION"); v != "" {
		version = v
	}
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
	slog.Info("core starting", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg)
	if err != nil {
		slog.Error("db connect failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := run(ctx, cfg, pool, version); err != nil {
		slog.Error("core exited with error", "error", err)
		os.Exit(1)
	}
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
