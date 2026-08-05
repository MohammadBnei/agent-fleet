package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/config"
	"github.com/MohammadBnei/agent-fleet/core/internal/coreserver"
	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/discord"
	"github.com/MohammadBnei/agent-fleet/core/internal/dispatch"
	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	"github.com/MohammadBnei/agent-fleet/core/internal/webui"
)

func run(ctx context.Context, cfg config.Config, pool *pgxpool.Pool) error {
	store := transcript.NewPostgresStore(pool)
	taskStore := tasks.NewStore(pool)
	journalStore := journal.NewStore(pool)

	provisioner, err := provisionerclient.New(cfg.ProvisionerGRPCAddr)
	if err != nil {
		return err
	}
	defer func() { _ = provisioner.Close() }()

	dc, err := discord.New(cfg, taskStore, store, provisioner)
	if err != nil {
		return err
	}
	if err := dc.Open(); err != nil {
		return err
	}
	defer func() { _ = dc.Close() }()

	relay := transcript.NewRelay(pool, dc)
	go relay.Run(ctx, 2*time.Second)
	// reliability-findings.md #5: nudge the relay right after a write
	// instead of waiting up to pollInterval for the next tick. The ticker
	// stays as the fallback.
	store.SetNudge(relay.Nudge)

	hub := dashboard.NewHub()
	go hub.PollLoop(ctx, store, 2*time.Second)

	// docs/adr/0020 point 2: core claims, then commands the provisioner —
	// the provisioner never claims tasks or decides to spawn on its own.
	dispatchLoop := dispatch.New(taskStore, provisioner, cfg.MaxInFlight)
	go dispatchLoop.Run(ctx, 2*time.Second)
	// Same nudge pattern as the relay above — CreateTask fires it so a new
	// task doesn't wait up to pollInterval for its first dispatch attempt.
	taskStore.SetNudge(dispatchLoop.Nudge)

	// core's first gRPC server (docs/adr/0020's Context) — the provisioner
	// pushes pod-lifecycle events here, and every worker pod's sidecar
	// reaches everything else (the old /mcp HTTP surface, and the direct-SQL
	// calls worker/src/db.ts used to make) through this same service.
	grpcServer := grpc.NewServer()
	agentfleetv1.RegisterCoreServiceServer(grpcServer, coreserver.New(store, taskStore, journalStore, provisioner))
	grpcLis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
	if err != nil {
		return err
	}
	go func() {
		slog.Info("core gRPC listening", "port", cfg.GRPCPort)
		if err := grpcServer.Serve(grpcLis); err != nil {
			slog.Error("core gRPC server exited", "error", err)
		}
	}()
	defer grpcServer.GracefulStop()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	dashboardSvc := dashboard.NewServer(taskStore, store, provisioner, hub)
	dashboardPath, dashboardHandler := agentfleetv1connect.NewDashboardServiceHandler(
		dashboardSvc,
		connect.WithInterceptors(dashboard.NewCSRFInterceptor()),
	)
	mux.Handle(dashboardPath, dashboardHandler)
	mux.Handle("/", webui.Handler())
	httpServer := &http.Server{Addr: ":" + cfg.Port, Handler: mux}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("core listening", "port", cfg.Port)
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
