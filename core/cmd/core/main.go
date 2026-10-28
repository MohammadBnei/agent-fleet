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
	cfg, err := config.FromProcess()
	if err != nil {
		// A refusal to start (mountain plan/15 §5): every problem in the one error, before anything is opened. The
		// logger is the JSON one at its default level, because LOG_LEVEL is one of the values that may be the problem.
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	if cfg.Version != "" {
		version = cfg.Version
	}

	// JSON, not slog's default TextHandler — Loki/LogQL queries against
	// this fleet expect structured logs (same convention the TS services'
	// log.ts already used). Level is LOG_LEVEL-configurable (debug/info/
	// warn/error).
	level, warning := resolveLogLevel(cfg.LogLevel)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
	if warning != "" {
		slog.Warn(warning, "value", cfg.LogLevel)
	}
	// Optional values that were set and invalid, already replaced by their defaults (config.Config.Warnings); a value
	// with userinfo in it was redacted when the problem was written.
	for _, w := range cfg.Warnings {
		slog.Warn("invalid configuration value, using the default", "problem", w)
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
// From main the fallback never fires: config.Load replaces an invalid LOG_LEVEL
// (with a warning) and an empty one by "info" (the branches stay for other callers).
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
