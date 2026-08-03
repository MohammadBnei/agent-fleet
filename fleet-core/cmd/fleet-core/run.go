package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/config"
	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/discord"
	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/e2eclient"
	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/mcpserver"
	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/tasks"
	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/transcript"
)

func run(ctx context.Context, cfg config.Config, pool *pgxpool.Pool) error {
	store := transcript.NewPostgresStore(pool)
	taskStore := tasks.NewStore(pool)

	e2eClient, err := e2eclient.New(cfg.E2eProvisionerGRPCAddr)
	if err != nil {
		return err
	}
	defer func() { _ = e2eClient.Close() }()

	dc, err := discord.New(cfg, taskStore, store, e2eClient)
	if err != nil {
		return err
	}
	if err := dc.Open(); err != nil {
		return err
	}
	defer func() { _ = dc.Close() }()

	go transcript.RelayLoop(ctx, pool, dc, 2*time.Second)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.New(store))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	httpServer := &http.Server{Addr: ":" + cfg.Port, Handler: mux}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("fleet-core listening", "port", cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
