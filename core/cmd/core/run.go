package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	"github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1/agentfleetv1connect"

	"github.com/MohammadBnei/agent-fleet/core/internal/agentsvc"
	"github.com/MohammadBnei/agent-fleet/core/internal/alertwebhook"
	"github.com/MohammadBnei/agent-fleet/core/internal/announce"
	"github.com/MohammadBnei/agent-fleet/core/internal/auth"
	"github.com/MohammadBnei/agent-fleet/core/internal/bridgeclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/config"
	"github.com/MohammadBnei/agent-fleet/core/internal/coreserver"
	"github.com/MohammadBnei/agent-fleet/core/internal/dashboard"
	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/discord"
	"github.com/MohammadBnei/agent-fleet/core/internal/dispatch"
	"github.com/MohammadBnei/agent-fleet/core/internal/errkeys"
	"github.com/MohammadBnei/agent-fleet/core/internal/filediff"
	"github.com/MohammadBnei/agent-fleet/core/internal/filestore"
	"github.com/MohammadBnei/agent-fleet/core/internal/grants"
	"github.com/MohammadBnei/agent-fleet/core/internal/islands"
	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/lokiclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/metrics"
	"github.com/MohammadBnei/agent-fleet/core/internal/packs"
	"github.com/MohammadBnei/agent-fleet/core/internal/promclient"
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
	// The pools' rule (MAX_DEFINITION_SESSIONS < MAX_IN_FLIGHT_TASKS, ...) is config.Load's.
	store := transcript.NewPostgresStore(pool)
	sessionStore := sessions.NewStore(pool)
	journalStore := journal.NewStore(pool)
	repoStore := repos.NewStore(pool)
	proposalStore := proposals.NewStore(pool)
	scheduleStore := schedules.NewStore(pool)
	// Every consumer below except SetNudge (a *PostgresStore-only method,
	// not part of the transcript.Store interface) goes through this
	// activity-tracking wrapper instead of `store` directly — see its own
	// comment for why this is the one choke point for the idle-timeout
	// backstop's activity signal.
	// Session announcements (docs/adr/0078 §2-3): published to the event
	// bridge for the sessions of clients with announceSessions. Without a
	// bridge (no BRIDGE_URL or no app password file) the announcer only
	// waits; core starts either way.
	var announceClients func() []string
	bridge := bridgeclient.New(cfg.Bridge)
	announcer := announce.New(pool, bridge, func() []string {
		if announceClients == nil {
			return nil
		}
		return announceClients()
	})
	activityStore := newActivityTrackingStore(store, sessionStore, announcer.Nudge)

	if cfg.CoreProvisionerToken == "" {
		slog.Warn("FLEET_CORE_PROVISIONER_TOKEN is unset: the provisioner will reject every call core makes (docs/adr/0069)")
	}
	provisioner, err := provisionerclient.New(cfg.ProvisionerGRPCAddr, cfg.CoreProvisionerToken)
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
	// Defined agents (docs/adr/0062) sync on the same pass, first run
	// immediate. Its own timeout keeps a slow fetch inside the 60s interval.
	defs := definitions.NewRegistry(pool, provisioner)
	// After each GOOD sync, the full set of valid definitions' vaults goes to
	// the provisioner, which makes the vaults match git (docs/adr/0065 §2).
	// Core commands, the provisioner executes; core sends key names, never
	// values. Off the sessions loop: a hung vault (30s client budget) must not
	// delay the ordinary-session sweeps, and a tick still running skips the next.
	var vaultErrs string
	var vaultBusy atomic.Bool
	repoOverrides := &repos.Overrides{Path: cfg.RepoOverridesPath}
	var repoSyncBusy atomic.Bool
	sessionLoop.SetOnTick(func(ctx context.Context) {
		if cfg.RepoOverridesPath != "" && repoSyncBusy.CompareAndSwap(false, true) {
			go func() {
				defer repoSyncBusy.Store(false)
				syncRepos(ctx, provisioner, repoStore, repoOverrides)
			}()
		}
		if err := defs.Sync(ctx); err != nil {
			slog.Error("definitions: sync failed", "error", err)
			return
		}
		if !vaultBusy.CompareAndSwap(false, true) {
			return
		}
		vaults := defs.Vaults()
		go func() {
			defer vaultBusy.Store(false)
			vaultErrs = reconcileVaults(ctx, provisioner, vaults, vaultErrs)
		}()
	})
	go sessionLoop.Run(ctx, 60*time.Second)

	announceClients = func() []string {
		var names []string
		for _, c := range defs.Clients() {
			if c.Spec.AnnounceSessions {
				names = append(names, c.Name)
			}
		}
		return names
	}
	if cfg.Bridge.URL == "" {
		slog.Info("BRIDGE_URL is unset: no session announcements (docs/adr/0078)")
	}
	go announcer.Run(ctx, 30*time.Second)

	// A schedule files a proposal, which a human opens — there is no separate
	// service to reach, so the scheduler always runs.
	scheduleLoop := schedules.NewLoop(scheduleStore, proposalStore)
	scheduleLoop.SetInactive(repoStore.Inactive)
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
		// errkeys first: every error a CoreService handler or interceptor returns leaves keyed (mountain plan/15 §4).
		grpc.ChainUnaryInterceptor(errkeys.UnaryServerInterceptor(), coreserver.AccessLogInterceptor, metrics.UnaryInterceptor, grpcAuth.UnaryInterceptor),
		grpc.ChainStreamInterceptor(errkeys.StreamServerInterceptor(), grpcAuth.StreamInterceptor),
	)
	coreSvc := coreserver.New(activityStore, sessionStore, journalStore, repoStore, provisioner, files, loki)
	// docs/adr/0078 §8: a definition session hands work on through the
	// bridge, with core setting actor, causation, hops and trust.
	coreSvc.SetPublishFunc(func(ctx context.Context, sessionID, definition, typ, subject, text, key string) (string, bool, int32, error) {
		r, err := dispatch.Publish(ctx, pool, bridge, sessionID, definition, typ, subject, text, key)
		return r.ID, r.Trusted, r.Hops, err
	})
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
	metricsServer := &http.Server{Addr: ":" + cfg.MetricsPort, Handler: metricsMux, ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)}
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
	dashboardSvc := dashboard.NewServer(sessionStore, proposalStore, activityStore, journalStore, repoStore, provisioner, files, hub, sessions.Caps{MaxLive: cfg.MaxInFlight, MaxDefinition: cfg.MaxDefinitionSessions, MaxExternal: cfg.MaxExternalSessions, MaxPerMember: cfg.MaxSessionsPerMember}, loki, prom, scheduleStore, version, cfg.ClaudeModel)
	dashboardSvc.SetDefinitions(defs)
	packStore := packs.NewStore(pool)
	defs.SetOnFiles(packStore.OnDefinitionFiles)
	dashboardSvc.SetPacks(packStore)
	grantStore := grants.NewStore(pool)
	dashboardSvc.SetGrants(grantStore)
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
	coreSvc.SetGrants(grantStore)
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
	var authInterceptors, memberInterceptors []connect.Interceptor
	gate := func(h http.Handler) http.Handler { return h }
	// The constant, not NewAgentServiceHandler(nil): the generated constructor
	// reads method values off its argument and panics on nil.
	agentPath := "/" + agentfleetv1connect.AgentServiceName + "/"
	if cfg.AuthDisabled {
		slog.Warn("FLEET_AUTH_DISABLED=1: the console is UNAUTHENTICATED. Local stacks only.")
	} else {
		oidcAuth, err := auth.New(ctx, auth.Config{
			IssuerURL:    cfg.OIDCIssuerURL,
			ClientID:     cfg.OIDCClientID,
			ClientSecret: cfg.OIDCClientSecret,
			PublicURL:    cfg.DashboardPublicURL,
			SessionKeys:  cfg.SessionKeys,
			AdmitGroups:  definitions.KnownGroups(),
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
		// Where the island's sign-in popup lands and closes (docs/adr/0068).
		mux.HandleFunc("/auth/done", auth.Done)
		// One value for both layers today. A service with a wider audience
		// widens the Gate's policy to the union and passes its own here.
		policy := auth.AdminOnly
		authInterceptors = append(authInterceptors, oidcAuth.ConnectInterceptor(policy))
		// Members reach AgentService and nothing else (docs/adr/0066 §2); the
		// service's authz table decides each method from there.
		members := auth.AnyGroup(definitions.KnownGroups())
		memberInterceptors = append(memberInterceptors, oidcAuth.ConnectInterceptor(members))
		gate = oidcAuth.Gate(policy, auth.Route{Prefix: agentPath, Allow: members})
	}
	// CSRF stays alongside the session check and is not redundant: a cookie is
	// attached by the browser to same-origin requests regardless of which page
	// triggered them, and the fleet publishes agent-authored dev servers on
	// same-site siblings of this host. The header plus no CORS on this service
	// is what stops one of those forging a call; the session check is what
	// stops an unauthenticated one. Only AgentService answers CORS, and only
	// for the embed origins (auth.CORS, docs/adr/0068).
	// errkeys.WithInterceptors on every Connect handler: what a handler or an inner interceptor returns leaves keyed.
	interceptors := []connect.Interceptor{dashboard.NewCSRFInterceptor(), dashboard.NewAccessLogInterceptor(), metrics.NewConnectInterceptor()}
	interceptors = append(interceptors, authInterceptors...)
	dashboardPath, dashboardHandler := agentfleetv1connect.NewDashboardServiceHandler(
		dashboardSvc,
		errkeys.WithInterceptors(interceptors...),
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
	alertHandler := alertwebhook.New(proposalStore, alertNotifier, alertwebhook.Config{
		Token:     cfg.AlertWebhookToken,
		Repo:      cfg.ThotRepo,
		ChannelID: cfg.ThotDiscordChannel,
	})
	alertHandler.SetInactive(repoStore.Inactive)
	mux.Handle("/webhook/alertmanager", alertHandler)
	mux.Handle(dashboardPath, dashboardHandler)
	// With FLEET_AUTH_DISABLED=1 no identity is set, so every AgentService
	// call is Unauthenticated: the service has no unauthenticated mode.
	memberInterceptors = append([]connect.Interceptor{dashboard.NewCSRFInterceptor(), dashboard.NewAccessLogInterceptor(), metrics.NewConnectInterceptor()},
		append(memberInterceptors, agentsvc.MemberInterceptor(originOf(cfg.DashboardPublicURL), embedOrigins(defs)))...)
	agentSvc := agentsvc.NewServer(defs, dashboardSvc, sessionStore, activityStore, hub)
	// The bridge's dispatcher (docs/adr/0071, 0078 §5): events reach
	// definition threads through agentSvc as client bridge, and a firing
	// alert reaches the same handler as /webhook/alertmanager.
	if defs != nil {
		var alerts dispatch.Alerts
		if cfg.ThotRepo != "" {
			alerts = alertHandler
		}
		dispatcher := dispatch.New(pool, bridge, agentSvc, defs, proposalStore, alerts)
		dashboardSvc.SetEventOpener(func(ctx context.Context, p *proposals.Proposal, sub string) (string, error) {
			return dispatch.OpenProposal(ctx, agentSvc, p, sub)
		})
		if cfg.Bridge.URL != "" {
			go dispatcher.Run(ctx)
		}
	}
	_, agentHandler := agentfleetv1connect.NewAgentServiceHandler(agentSvc, errkeys.WithInterceptors(memberInterceptors...))
	mux.Handle(agentPath, agentHandler)

	// :9091, clients (docs/adr/0066 §3): the same handlers, a bearer for the
	// principal, AgentService and nothing else on the socket. No CSRF check:
	// no browser and no cookie reach this port.
	var clientList func() []definitions.Client
	if defs != nil {
		clientList = defs.Clients
	} else {
		clientList = func() []definitions.Client { return nil }
	}
	clientMux := http.NewServeMux()
	_, clientHandler := agentfleetv1connect.NewAgentServiceHandler(agentSvc, connect.WithReadMaxBytes(1<<20), errkeys.WithInterceptors(
		dashboard.NewAccessLogInterceptor(), metrics.NewConnectInterceptor(), agentsvc.ClientInterceptor(clientList, cfg.TokenEnv)))
	clientMux.Handle(agentPath, clientHandler)
	clientServer := &http.Server{Addr: ":" + cfg.ClientPort, Handler: clientMux, ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		slog.Info("core client API listening", "port", cfg.ClientPort)
		if err := clientServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	defer func() { _ = clientServer.Close() }()
	mux.Handle("/islands/", islands.Handler("/islands/"))
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
	cors := auth.CORS(agentPath, originOf(cfg.DashboardPublicURL), embedOrigins(defs))
	httpServer := &http.Server{Addr: ":" + cfg.Port, Handler: httpHandler(mux, gate, cors), ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)}

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

// originOf is a URL's scheme://host, the form a browser's Origin header takes.
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// embedOrigins is nil without a registry: no island is accepted.
func embedOrigins(defs *definitions.Registry) func() []string {
	if defs == nil {
		return nil
	}
	return defs.EmbedOrigins
}

// httpHandler is the outermost composition core serves. FrameDeny sits outside
// the gate so the gate's own 302/401/403 carry it, and applies with
// FLEET_AUTH_DISABLED=1 (identity gate) too (docs/adr/0069). CORS sits
// between them for the same reason: the gate's 401/403 to an island must carry
// its headers, and a preflight is answered before the gate (docs/adr/0068).
func httpHandler(mux http.Handler, gate, cors func(http.Handler) http.Handler) http.Handler {
	return auth.FrameDeny(cors(gate(mux)))
}

// syncRepos projects the bot's GitHub repos plus infra-bootstrap's overrides
// into repos (docs/adr/0074). No good overrides read yet in this process, or
// a failed listing: nothing changes. Off the sessions loop like the vault
// reconcile, so a slow GitHub cannot delay the sweeps.
func syncRepos(ctx context.Context, p *provisionerclient.Client, store *repos.Store, ov *repos.Overrides) {
	overrides, ok := ov.Load()
	if !ok {
		return
	}
	list, err := p.ListGitHubRepos(ctx)
	if status.Code(err) == codes.Unimplemented {
		slog.Debug("repos: provisioner predates ListGitHubRepos (docs/adr/0074)")
		return
	}
	if err != nil {
		slog.Error("repos: list GitHub repos failed, nothing changed", "error", err)
		return
	}
	gh := make([]repos.GitHubRepo, len(list))
	for i, r := range list {
		gh[i] = repos.GitHubRepo{ID: r.GetId(), FullName: r.GetFullName(), Name: r.GetName(), CloneURL: r.GetCloneUrl(), DefaultBranch: r.GetDefaultBranch()}
	}
	if _, err := store.Sync(ctx, gh, overrides); err != nil {
		slog.Error("repos: sync", "error", err)
	}
}

// vaultReconciler is provisionerclient.Client's ReconcileVaults.
type vaultReconciler interface {
	ReconcileVaults(ctx context.Context, vaults []*agentfleetv1.VaultSpec) (*agentfleetv1.ReconcileVaultsResponse, error)
}

// reconcileVaults pushes the vault set and returns the joined error state,
// logged only when it differs from prev, like the registry's file errors: a
// missing secret is a standing state, not news every minute. An older
// provisioner (Unimplemented) is a debug line and leaves the state as it was.
func reconcileVaults(ctx context.Context, p vaultReconciler, vaults []*agentfleetv1.VaultSpec, prev string) string {
	resp, err := p.ReconcileVaults(ctx, vaults)
	if status.Code(err) == codes.Unimplemented {
		slog.Debug("definitions: provisioner predates ReconcileVaults (docs/adr/0065)")
		return prev
	}
	var msgs []string
	if err != nil {
		msgs = append(msgs, err.Error())
	}
	for _, r := range resp.GetResults() {
		if r.GetError() != "" {
			msgs = append(msgs, r.GetName()+": "+r.GetError())
		}
	}
	if e := resp.GetPruneError(); e != "" {
		msgs = append(msgs, "prune: "+e)
	}
	joined := strings.Join(msgs, "; ")
	switch {
	case joined == prev:
	case joined != "":
		slog.Error("definitions: vault reconcile", "errors", joined)
	default:
		slog.Info("definitions: vaults match their definitions")
	}
	return joined
}
