package main

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/config"
	"github.com/MohammadBnei/agent-fleet/core/internal/coreserver"
	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/discord"
	"github.com/MohammadBnei/agent-fleet/core/internal/filediff"
	"github.com/MohammadBnei/agent-fleet/core/internal/filestore"
	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/lokiclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/metrics"
	"github.com/MohammadBnei/agent-fleet/core/internal/promclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/promptsnippets"
	"github.com/MohammadBnei/agent-fleet/core/internal/proposals"
	"github.com/MohammadBnei/agent-fleet/core/internal/provisionerclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/repos"
	"github.com/MohammadBnei/agent-fleet/core/internal/schedules"
	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
	"github.com/MohammadBnei/agent-fleet/core/internal/sttclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	"github.com/MohammadBnei/agent-fleet/core/internal/webui"
)

func run(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, version string) error {
	store := transcript.NewPostgresStore(pool)
	sessionStore := sessions.NewStore(pool)
	journalStore := journal.NewStore(pool)
	repoStore := repos.NewStore(pool)
	proposalStore := proposals.NewStore(pool)
	snippetStore := promptsnippets.NewStore(pool)
	scheduleStore := schedules.NewStore(pool)
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

	// A schedule files a proposal, which a human opens — there is no separate
	// service to reach, so the scheduler always runs.
	scheduleLoop := schedules.NewLoop(scheduleStore, proposalStore)
	go scheduleLoop.Run(ctx, 60*time.Second)
	// Same live-refresh wiring repos uses: a dashboard edit takes effect now,
	// not on the next tick.
	scheduleStore.SetOnChange(scheduleLoop.Nudge)

	// core's first gRPC server (docs/adr/0020's Context) — the provisioner
	// pushes pod-lifecycle events here, and every worker pod's sidecar
	// reaches everything else (the old /mcp HTTP surface, and the direct-SQL
	// calls worker/src/db.ts used to make) through this same service.
	// Every call carries the calling session's lease_id, and core checks it
	// against the sessions table (docs/adr/0020 point 1 keeps that check here,
	// where the DB credentials already are). Before this, 9090 authenticated
	// nobody: any pod in the namespace could call it, and every method that
	// names a session took that name from the request body on trust.
	//
	// The STREAM chain is not decoration. This server had only a unary chain,
	// so a unary-only check would leave StreamHumanMessages — the live feed of
	// everything a human types to a session — and ReportPodEvents wide open,
	// while every test still passed.
	if cfg.ProvisionerToken == "" {
		slog.Warn("FLEET_PROVISIONER_TOKEN is unset: the provisioner cannot authenticate to CoreService, so pod events will be rejected")
	}
	// grpcAuth, not `auth`: internal/auth is a PACKAGE imported by this file
	// for the console's OIDC gate below, and a local variable of that name
	// shadows it for the rest of the function. The two arrived in separate PRs
	// that were each green on their own branch and had no textual conflict, so
	// nothing failed until they were on main together — `auth.New` then
	// resolved against this variable and the build broke at the release, not at
	// either PR. Renaming the variable is the fix; do not rename the import.
	grpcAuth := coreserver.NewAuthenticator(sessionStore, cfg.ProvisionerToken)
	grpcServer := grpc.NewServer(
		grpc.ChainUnaryInterceptor(coreserver.AccessLogInterceptor, metrics.UnaryInterceptor, grpcAuth.UnaryInterceptor),
		grpc.ChainStreamInterceptor(grpcAuth.StreamInterceptor),
	)
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

	// /metrics gets its OWN listener, and is deliberately absent from the mux
	// below (docs/adr/0059).
	//
	// It used to live on cfg.Port, whose mux the OIDC gate wraps, and the gate's
	// exempt list did not include it — so every ServiceMonitor scrape got a 401
	// and core's targets went down (#230). The fix is NOT to exempt it: this
	// host's IngressRoute matches on HOST with no path constraint, so an exempt
	// path is a public one, and /metrics carries every target repo name
	// (agentfleet_tasks_current{repo}) and every RPC method served. A port with
	// no IngressRoute in front of it needs no exemption and no credential —
	// which is what the old comment claimed was already true, and wasn't.
	//
	// A failed bind here is FATAL, not a warning. #230's actual damage was that
	// core looked perfectly healthy — console up, gRPC up, no restarts — while
	// producing no metrics at all, so the only signal was an alert about the
	// alerting. A process that cannot serve its own telemetry should say so by
	// dying, not by logging one line and carrying on.
	errCh := make(chan error, 1)
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsServer := &http.Server{Addr: ":" + cfg.MetricsPort, Handler: metricsMux}
	go func() {
		slog.Info("core metrics listening", "port", cfg.MetricsPort)
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("metrics server: %w", err)
		}
	}()
	// Close, not Shutdown: a half-delivered scrape is worth nothing, and the
	// drain that matters is httpServer.Shutdown's below.
	defer func() { _ = metricsServer.Close() }()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	dashboardSvc := dashboard.NewServer(sessionStore, proposalStore, activityStore, journalStore, repoStore, snippetStore, provisioner, files, hub, cfg.MaxInFlight, loki, prom, scheduleStore, version)
	// PromptSession warms an idle target through the dashboard server's own
	// warmIfIdle rather than a second copy of it (docs/adr/0041) — that
	// function carries the capacity cap and the proposed/pending gates that
	// stop an unapproved task ever getting a pod, and two implementations
	// of pod dispatch is exactly the drift docs/adr/0020 point 2 exists to
	// prevent.
	coreSvc.SetWarmFunc(dashboardSvc.WarmIfIdle)
	// One parking lot, both servers. The dashboard drops "diff this path" in;
	// the sidecar's telemetry call — which lands on coreSvc, not here — is the
	// only channel core has for asking a pod anything, so it is what carries
	// the request out and the answer back.
	fileDiffs := filediff.New()
	// Dictation. Deliberately non-fatal: ukubi-stt is single-replica on a node
	// that reboots for gaming (infra-bootstrap ADR-0044), so a console that
	// refuses to start without it would trade a working feature for a broken
	// deployment. No token configured is the same story — the RPC answers
	// Unavailable and everything else works.
	if cfg.STTToken == "" {
		slog.Info("speech-to-text disabled: STT_TOKEN_FLEET is not set")
	} else if stt, err := sttclient.New(cfg.STTAddr, cfg.STTToken); err != nil {
		slog.Warn("speech-to-text disabled", "addr", cfg.STTAddr, "err", err)
	} else {
		defer func() { _ = stt.Close() }()
		dashboardSvc.SetSTTClient(stt)
		slog.Info("speech-to-text enabled", "addr", cfg.STTAddr)
	}

	dashboardSvc.SetFileDiffStore(fileDiffs)
	coreSvc.SetFileDiffStore(fileDiffs)
	// The fleet's only outbound "a session needs you" signal. Without this
	// wire, a session blocked on a permission decision notifies nobody and
	// is findable only by someone already watching the dashboard.
	if dc != nil {
		coreSvc.SetNotifyBlockedFunc(dc.NotifyBlocked)
	}
	// The console federates to authentik (infra-bootstrap ADR-0041).
	//
	// FAILS CLOSED, unlike every other optional secret core reads. An unset
	// ALERT_WEBHOOK_TOKEN disables a webhook; an unset OIDC config must not
	// disable the gate, because the Infisical operator renders a stale or empty
	// Secret often enough that core would then come up completely open and look
	// perfectly healthy. Refusing to start is loud; serving unauthenticated is
	// not. FLEET_AUTH_DISABLED=1 is the explicit local-stack opt-out, so "no
	// auth" is always something someone wrote down.
	var authInterceptors []connect.Interceptor
	gate := func(h http.Handler) http.Handler { return h }
	if cfg.AuthDisabled {
		slog.Warn("FLEET_AUTH_DISABLED=1: the console is UNAUTHENTICATED. Local stacks only.")
	} else {
		oidcAuth, err := auth.New(ctx, auth.Config{
			IssuerURL:    cfg.OIDCIssuerURL,
			ClientID:     cfg.OIDCClientID,
			ClientSecret: cfg.OIDCClientSecret,
			PublicURL:    cfg.DashboardPublicURL,
			SessionKeys:  cfg.SessionKeys,
		})
		if err != nil {
			return fmt.Errorf("oidc setup (set FLEET_AUTH_DISABLED=1 for a local stack): %w", err)
		}
		mux.HandleFunc("/auth/login", oidcAuth.Login)
		mux.HandleFunc("/auth/callback", oidcAuth.Callback)
		mux.HandleFunc("/auth/logout", oidcAuth.Logout)
		// Backs the console's own "signed in as" chrome. Registered only when
		// the gate is on, so with FLEET_AUTH_DISABLED=1 it 404s and the SPA
		// simply shows nothing — which is honest: there is no identity to show.
		mux.HandleFunc("/auth/me", oidcAuth.Me)
		authInterceptors = append(authInterceptors, oidcAuth.ConnectInterceptor())
		gate = oidcAuth.Gate
	}
	// CSRF stays alongside the session check and is not redundant: a cookie is
	// attached by the browser to same-origin requests regardless of which page
	// triggered them, and the fleet publishes agent-authored dev servers on
	// same-site siblings of this host. The header plus never allowing CORS is
	// what stops one of those forging a call; the session check is what stops
	// an unauthenticated one.
	interceptors := []connect.Interceptor{dashboard.NewCSRFInterceptor(), dashboard.NewAccessLogInterceptor(), metrics.NewConnectInterceptor()}
	interceptors = append(interceptors, authInterceptors...)
	dashboardPath, dashboardHandler := agentfleetv1connect.NewDashboardServiceHandler(
		dashboardSvc,
		connect.WithInterceptors(interceptors...),
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
	// The gate wraps the WHOLE mux, with an explicit exempt list inside it,
	// rather than wrapping webui.Handler() alone.
	//
	// Gating one handler makes every route added later public by default until
	// someone remembers — the same shape as this repo's "wire it into EVERY CI
	// path" trap, with a worse failure mode. This way a forgotten route fails
	// closed. It also has to sit outside the SPA handler specifically, because
	// that one serves index.html with a 200 for any unknown path, so a gate
	// underneath it would return the app shell to an unauthenticated request.
	httpServer := &http.Server{Addr: ":" + cfg.Port, Handler: gate(mux)}

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
