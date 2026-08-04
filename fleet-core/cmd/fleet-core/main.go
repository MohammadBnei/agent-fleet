package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/config"
	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/db"
)

// ponytail: no Cobra — one subcommand (`migrate`) doesn't earn a CLI
// framework dependency. Add one if a second subcommand shows up.
func main() {
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg)
	if err != nil {
		slog.Error("db connect failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := db.ApplySchema(ctx, pool); err != nil {
			slog.Error("migrate failed", "error", err)
			os.Exit(1)
		}
		slog.Info("migrate: schema applied")
		return
	}

	if err := run(ctx, cfg, pool); err != nil {
		slog.Error("fleet-core exited with error", "error", err)
		os.Exit(1)
	}
}
