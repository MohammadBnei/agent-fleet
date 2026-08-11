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
	"github.com/MohammadBnei/agent-fleet/core/internal/filestore"
	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/lokiclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/promptsnippets"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repoprofiles"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
	"github.com/MohammadBnei/agent-fleet/core/internal/thotevents"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	"github.com/MohammadBnei/agent-fleet/core/internal/webui"
)

// noopNotifier is the relay's target when no Discord bot token is
// configured — the dashboard (DashboardService.CreateTask/AnswerQuestion/
// etc.) never required Discord (a dashboard-origin task already has a nil
// discord_thread_id, which PostToThread already no-ops on), so core's
// gRPC/dashboard surface shouldn't hard-require a bot session at startup
// either. Local dev and dashboard-only deployments can now run core
// without DISCORD_BOT_TOKEN.
type noopNotifier struct{}

func (noopNotifier) PostToThread(context.Context, string, transcript.Entry) error { return nil }

func run(ctx context.Context, cfg config.Config, pool *pgxpool.Pool) error {
	store := transcript.NewPostgresStore(pool)
	taskStore := tasks.NewStore(pool)
	journalStore := journal.NewStore(pool)
	repoStore := repos.NewStore(pool)
	profileStore := repoprofiles.NewStore(pool)
	snippetStore := promptsnippets.NewStore(pool)
	thotEventStore := thotevents.NewStore(pool)
	// Every consumer below except SetNudge (a *PostgresStore-only method,
	// not part of the transcript.Store interface) goes through this
	// activity-tracking wrapper instead of `store` directly — see its own
	// comment for why this is the one choke point for the idle-timeout
	// backstop's activity signal.
	activityStore := newActivityTrackingStore(store, taskStore)

	provisioner, err := provisionerclient.New(cfg.ProvisionerGRPCAddr)
	if err != nil {
		return err
	}
	defer func() { _ = provisioner.Close() }()

	files, err := filestore.New(ctx, filestore.Config{
		Endpoint:  cfg.GarageS3Endpoint,
		Bucket:    cfg.GarageFilesBucket,
		AccessKey: cfg.GarageFilesAccessKey,
		SecretKey: cfg.GarageFilesSecret,
	})
	if err != nil {
		return err
	}

	// Create Loki client for log querying (docs/adr/0013)
	loki := lokiclient.New(cfg.LokiURL)

	var notifier transcript.Notifier = noopNotifier{}
	if cfg.DiscordBotToken != "" {
		dc, err := discord.New(cfg, taskStore, activityStore, repoStore, provisioner)
		if err != nil {
			return err
		}
		if err := dc.Open(); err != nil {
			return err
		}
		defer func() { _ = dc.Close() }()
		notifier = dc
		// Live-refreshes /task's repo dropdown after a dashboard repos
		// mutation (docs/adr/0028) — no redeploy/restart needed.
		repoStore.SetOnChange(func() { dc.RefreshCommands(ctx) })
	} else {
		slog.Warn("DISCORD_BOT_TOKEN not set — running without Discord (dashboard/gRPC only)")
	}

	relay := transcript.NewRelay(pool, notifier)
	go relay.Run(ctx, 2*time.Second)
	// reliability-findings.md #5: nudge the relay right after a write
	// instead of waiting up to pollInterval for the next tick. The ticker
	// stays as the fallback.
	store.SetNudge(relay.Nudge)

	hub := dashboard.NewHub()
	go hub.PollLoop(ctx, store, 2*time.Second)

	// docs/adr/0020 point 2: core claims, then commands the provisioner —
	// the provisioner never claims tasks or decides to spawn on its own.
	dispatchLoop := dispatch.New(taskStore, activityStore, repoStore, profileStore, provisioner, cfg.MaxInFlight, cfg.MaxTaskRetries, cfg.StopGrace, cfg.IdleTimeout)
	// CreateTask/SetStatus/MarkCrashed all nudge (below) for the responsive
	// path, so this interval is now purely a fallback: recovery for a
	// dropped nudge, plus the passive path for a worker that vanished
	// without ever calling MarkCrashed (e.g. OOM-killed) — that case has no
	// write to nudge on and is only caught by the 10-minute heartbeat-
	// staleness scan in ClaimNextTask. 30s is comfortably tight against a
	// 10-minute window while cutting ~150x the wasted ticks 2s produced.
	go dispatchLoop.Run(ctx, 30*time.Second)
	// Same nudge pattern as the relay above — CreateTask/SetStatus/
	// MarkCrashed all fire it so dispatch/reclaim don't wait up to
	// pollInterval for the common, event-driven cases.
	taskStore.SetNudge(dispatchLoop.Nudge)

	// core's first gRPC server (docs/adr/0020's Context) — the provisioner
	// pushes pod-lifecycle events here, and every worker pod's sidecar
	// reaches everything else (the old /mcp HTTP surface, and the direct-SQL
	// calls worker/src/db.ts used to make) through this same service.
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(coreserver.AccessLogInterceptor))
	agentfleetv1.RegisterCoreServiceServer(grpcServer, coreserver.New(activityStore, taskStore, journalStore, profileStore, provisioner, files, loki, thotEventStore))
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
	dashboardSvc := dashboard.NewServer(taskStore, activityStore, journalStore, repoStore, profileStore, snippetStore, provisioner, files, hub, cfg.MaxInFlight, loki, thotEventStore)
	dashboardPath, dashboardHandler := agentfleetv1connect.NewDashboardServiceHandler(
		dashboardSvc,
		connect.WithInterceptors(dashboard.NewCSRFInterceptor(), dashboard.NewAccessLogInterceptor()),
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
