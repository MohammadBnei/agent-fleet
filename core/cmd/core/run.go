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
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/alertwebhook"
	"github.com/MohammadBnei/agent-fleet/core/internal/audits"
	"github.com/MohammadBnei/agent-fleet/core/internal/config"
	"github.com/MohammadBnei/agent-fleet/core/internal/coreserver"
	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/discord"
	"github.com/MohammadBnei/agent-fleet/core/internal/filestore"
	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/lokiclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/metrics"
	"github.com/MohammadBnei/agent-fleet/core/internal/promclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/promptsnippets"
	"github.com/MohammadBnei/agent-fleet/core/internal/proposals"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/scheduledaudits"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	"github.com/MohammadBnei/agent-fleet/core/internal/webui"
)

func run(ctx context.Context, cfg config.Config, pool *pgxpool.Pool) error {
	store := transcript.NewPostgresStore(pool)
	sessionStore := sessions.NewStore(pool)
	journalStore := journal.NewStore(pool)
	repoStore := repos.NewStore(pool)
	proposalStore := proposals.NewStore(pool)
	snippetStore := promptsnippets.NewStore(pool)
	auditStore := scheduledaudits.NewStore(pool)
	// Every consumer below except SetNudge (a *PostgresStore-only method,
	// not part of the transcript.Store interface) goes through this
	// activity-tracking wrapper instead of `store` directly — see its own
	// comment for why this is the one choke point for the idle-timeout
	// backstop's activity signal.
	activityStore := newActivityTrackingStore(store, sessionStore)

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
	prom := promclient.New(cfg.PrometheusURL)

	// Discord is outbound-only now (docs/adr/0048): no slash commands, no
	// threads, no message-content intent, and no relay of transcript entries.
	// It posts that something needs a human and links to the dashboard, where
	// the answering actually happens behind real authentication.
	//
	// The transcript relay and its retry/dead-letter machinery went with the
	// inbound half — there is nothing to deliver at-least-once when the only
	// outbound message is a notification that a durable, queryable row exists.
	var dc *discord.Client
	if cfg.DiscordBotToken != "" {
		dc, err = discord.New(cfg, sessionStore)
		if err != nil {
			return err
		}
		if err := dc.Open(); err != nil {
			return err
		}
		defer func() { _ = dc.Close() }()
	} else {
		slog.Warn("DISCORD_BOT_TOKEN not set — running without Discord notifications (dashboard/gRPC only)")
	}

	hub := dashboard.NewHub()
	go hub.PollLoop(ctx, store, 2*time.Second)

	// The dispatch loop is gone (docs/adr/0048). Nothing claims, nothing is
	// queued, and no nudge is needed: a session's first message provisions
	// its pod synchronously on the request path, so there is no interval to
	// wait out for the common case.
	//
	// What is left runs on a plain ticker because every pass is a check
	// against reality rather than a reaction to an event — reconciling
	// pod_phase against Kubernetes, and four expiry sweeps. 60s: the
	// reconcile pass is the backstop for a dropped pod event, and a minute of
	// a wrongly-held concurrency slot is unnoticeable where "never" was the
	// bug.
	sessionLoop := sessions.NewLoop(sessionStore, provisioner,
		cfg.StopGrace, cfg.StartupStall, cfg.IdleTimeout, cfg.SessionRetention, cfg.TurnStall)
	go sessionLoop.Run(ctx, 60*time.Second)

	// Scheduled audits create thot tasks (docs/adr/0037) — no separate
	// service to reach, so the scheduler always runs.
	auditLoop := audits.New(auditStore, proposalStore)
	go auditLoop.Run(ctx, 60*time.Second)
	// Same live-refresh wiring repos uses for Discord's /task choices: a
	// dashboard edit takes effect now, not on the next tick.
	auditStore.SetOnChange(auditLoop.Nudge)

	// core's first gRPC server (docs/adr/0020's Context) — the provisioner
	// pushes pod-lifecycle events here, and every worker pod's sidecar
	// reaches everything else (the old /mcp HTTP surface, and the direct-SQL
	// calls worker/src/db.ts used to make) through this same service.
	grpcServer := grpc.NewServer(grpc.ChainUnaryInterceptor(coreserver.AccessLogInterceptor, metrics.UnaryInterceptor))
	coreSvc := coreserver.New(activityStore, sessionStore, journalStore, repoStore, provisioner, files, loki)
	agentfleetv1.RegisterCoreServiceServer(grpcServer, coreSvc)
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
	mux.Handle("/metrics", promhttp.Handler())
	dashboardSvc := dashboard.NewServer(sessionStore, proposalStore, activityStore, journalStore, repoStore, snippetStore, provisioner, files, hub, cfg.MaxInFlight, loki, prom, auditStore)
	// PromptSession warms an idle target through the dashboard server's own
	// warmIfIdle rather than a second copy of it (docs/adr/0041) — that
	// function carries the capacity cap and the proposed/pending gates that
	// stop an unapproved task ever getting a pod, and two implementations
	// of pod dispatch is exactly the drift docs/adr/0020 point 2 exists to
	// prevent.
	coreSvc.SetWarmFunc(dashboardSvc.WarmIfIdle)
	dashboardPath, dashboardHandler := agentfleetv1connect.NewDashboardServiceHandler(
		dashboardSvc,
		connect.WithInterceptors(dashboard.NewCSRFInterceptor(), dashboard.NewAccessLogInterceptor(), metrics.NewConnectInterceptor()),
	)
	// docs/adr/0037: an alert becomes a thot session — but only after a human
	// opens the proposal it files (docs/adr/0048). Registered even when the
	// token is unset: the handler then refuses with 503, which is a far
	// better signal than a route that silently doesn't exist.
	//
	// dc is nil when no bot token is configured, and a nil Notifier disables
	// the notification without disabling the webhook.
	var alertNotifier alertwebhook.Notifier
	if dc != nil {
		alertNotifier = dc
	}
	mux.Handle("/webhook/alertmanager", alertwebhook.New(proposalStore, alertNotifier, alertwebhook.Config{
		Token:     cfg.AlertWebhookToken,
		Repo:      cfg.ThotRepo,
		ChannelID: cfg.ThotDiscordChannel,
	}))
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
