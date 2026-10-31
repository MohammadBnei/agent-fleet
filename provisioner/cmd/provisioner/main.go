package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/config"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/coreclient"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/git"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/github"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/grpcserver"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/reconcile"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/vault"
	"github.com/MohammadBnei/ukubi-go/telemetry"
)

// version is the release this binary runs as: FLEET_VERSION, which deploy
// sets on the Deployment (.github/workflows/docker.yml), or "dev" without it.
// Not compiled in any more: an image is tagged by a hash of its inputs and
// reused across releases that don't touch them, so the binary cannot know
// which release it is part of.
var version = "dev"

func main() {
	// The environment is read here and nowhere else (internal/config). Its
	// refusals (a malformed FLEET_GIT_URL_REWRITE) are held until after the
	// token refusal below; its warnings, for an optional key that was set but
	// invalid and took its default, are logged as soon as the handler exists.
	cfg, cfgErr := config.FromProcess()
	if cfg.Version != "" {
		version = cfg.Version
	}

	// JSON, not slog's default TextHandler — Loki/LogQL queries against
	// this fleet expect structured logs (same convention the TS services'
	// log.ts already used). Level is LOG_LEVEL-configurable (debug/info/
	// warn/error).
	level, warning := resolveLogLevel(cfg.LogLevel)
	slog.SetDefault(telemetry.NewLoggerAt(os.Stdout, level))
	if warning != "" {
		slog.Warn(warning, "value", cfg.LogLevel)
	}
	// Whatever LOG_LEVEL is: LOG_LEVEL=error must not hide that a value was
	// ignored. Same stdout, same JSON, a handler of its own at warn.
	warnLog := telemetry.NewLoggerAt(os.Stdout, slog.LevelWarn)
	for _, w := range cfg.Warnings {
		warnLog.Warn(w)
	}
	slog.Info("provisioner starting", "version", version)

	// First, before any side effect: an unauthenticated ProvisionerService
	// can create and expose pods for anyone on the pod network
	// (docs/adr/0069), so no token means no provisioner.
	//
	// Opt-in OTLP export (docs/adr/0081): inert without the variable. Built
	// first because the server takes its option, refused last: a nil Tracing
	// gives no option.
	tr, trErr := telemetry.New(cfg.TracesEndpoint, "agent-fleet-provisioner")
	grpcSrv, err := grpcserver.NewGRPCServer(cfg.CoreProvisionerToken, tr.ServerOption())
	if err != nil {
		slog.Error("refusing to start", "error", err)
		os.Exit(1)
	}
	// What Load refuses: a malformed FLEET_GIT_URL_REWRITE (docs/adr/0080), a
	// setting that contains the local stack and so fails closed. (The one
	// required key, the core token, is refused above.)
	if cfgErr != nil {
		slog.Error("refusing to start", "error", cfgErr)
		os.Exit(1)
	}
	if trErr != nil {
		slog.Error("refusing to start", "error", trErr)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Every file/dir this process creates on the shared workspace PVC
	// (git clone, worktree add — internal/git) must stay writable by the
	// worker container too, which runs as a non-root user. Provisioner and
	// worker are separate containers with no
	// coordinated UID/GID, so world-writable is the simplest fix that
	// doesn't need fsGroup/GID plumbing across two services on a
	// single-tenant, internal-only PVC.
	syscall.Umask(0)

	// No Postgres connection anywhere in this process — the provisioner
	// holds no DB credentials at all (docs/adr/0020 point 1). Kubernetes
	// itself is the durable source of truth for pod/session state.
	k8sc, err := k8s.New(cfg.Namespace, images(cfg, version))
	if err != nil {
		slog.Error("k8s client init failed", "error", err)
		os.Exit(1)
	}

	// The shared cluster-ip-whitelist Middleware used to be created here, and
	// its failure was fatal. It existed so worker pods could reach each
	// other's preview URLs without a basic-auth prompt — which stopped being
	// a thing when the sandbox merged into the session pod (docs/adr/0048 §6):
	// an agent testing its own app reaches it on localhost, in its own pod.
	//
	// Nothing referenced the Middleware any more; expose()'s route carries
	// basic auth alone. Removing it also removes a startup dependency on a
	// Traefik CRD, which is one less thing the local harness has to fake.

	gitMgr := git.NewManager(cfg.WorkspaceRoot)
	// docs/adr/0075 §9: the provisioner's own clone/fetch uses the App's
	// read-only installation tokens, and nothing else: the bot PAT is gone.
	app := github.NewApp(cfg.GitHubAppDir, parseIDs(cfg.GitHubAccounts))
	gitMgr.GitEnv = app.GitEnv

	// Fill the shared PVC's Playwright browser cache, which every worker pod
	// mounts read-only (pod.go's browsersDir). Best-effort and NOT fatal: a
	// fleet whose browser cache is still filling cannot browse yet, which is
	// strictly better than a provisioner that refuses to start over it. The
	// Job itself is idempotent, so this is a no-op once the cache is warm.
	if err := k8sc.EnsureBrowserCache(ctx); err != nil {
		slog.Warn("browser cache job not ensured — sessions will have no browser until this succeeds",
			"error", err)
	}

	// An unset token is not a silent degradation: CoreService returns
	// PERMISSION_DENIED, which coreclient's retry policy does NOT retry, so
	// pod-lifecycle events would stop reaching core with nothing in the logs
	// looking wrong. Say so at startup.
	if cfg.CoreToken == "" {
		slog.Warn("FLEET_PROVISIONER_TOKEN is unset: core will reject every pod event this provisioner reports")
	}
	core, err := coreclient.New(cfg.CoreGRPCAddr, cfg.CoreToken)
	if err != nil {
		slog.Error("core client init failed", "error", err)
		os.Exit(1)
	}
	defer func() { _ = core.Close() }()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("/metrics", promhttp.Handler())
	httpServer := &http.Server{Addr: ":" + cfg.Port, Handler: mux, ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)}

	// docs/adr/0065: the fleet's only Agent Vault client. Nothing is checked at
	// boot: before infra-bootstrap's agent-vault-bootstrap.sh has run, every
	// definition warm and reconcile fails closed with that script's name.
	vc := vault.New(cfg.AgentVaultAPI, cfg.AgentVaultProxy, cfg.AgentVaultSecretsDir)
	// docs/adr/0075: with the App mounted, each vault's GitHub key is a token
	// minted for its one repo; without it, the mounted file as before.
	// Always wired: Token re-checks the mount per call, so a Secret synced
	// after boot is used without a restart; unmounted, the vault falls back
	// to the Infisical files.
	vc.Forge = app
	if !app.Configured() {
		slog.Warn("github app not mounted yet: vault GitHub keys come from Infisical files", "dir", cfg.GitHubAppDir)
	} else if len(app.Accounts) == 0 {
		slog.Error("github app mounted but FLEET_GITHUB_ACCOUNTS is empty: every definition GitHub token will fail closed")
	}
	provSvc := grpcserver.New(k8sc, gitMgr, core, vc, cfg.E2eHost, cfg.FleetSharedRepoURL, cfg.FleetSharedBranch, version)
	provSvc.SetDefinitionsRef(cfg.FleetDefinitionsRef)
	provSvc.SetGitHub(app)
	provSvc.SetGitIdentity(cfg.GitAuthorName, cfg.GitAuthorEmail)
	agentfleetv1.RegisterProvisionerServiceServer(grpcSrv, provSvc)

	startReconcile(ctx, cfg, k8sc, core, vc)

	// docs/adr/0075 §5: push re-minted tokens on the provisioner's own clock,
	// not core's. Tokens are re-minted with <30m left, so 5m leaves margin.
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if app.Configured() { // nothing re-mints without the App
					provSvc.RefreshVaults(ctx)
				}
			}
		}
	}()

	// The branch/worktree sweep loop is gone (docs/adr/0048 §5). It deleted
	// worktrees on its own ticker, without telling core — which is why any
	// claim that a session knew whether its disk still existed was false, and
	// why swept_at could not be trusted. core is the single writer of that
	// fact now, and the only thing that decides a session's disk should go.

	go func() {
		lis, err := net.Listen("tcp", ":"+cfg.GRPCPort)
		if err != nil {
			slog.Error("grpc listen failed", "error", err)
			return
		}
		slog.Info("provisioner grpc listening", "port", cfg.GRPCPort)
		if err := grpcSrv.Serve(lis); err != nil {
			slog.Error("grpc serve failed", "error", err)
		}
	}()

	go func() {
		slog.Info("provisioner http listening", "port", cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http serve failed", "error", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	grpcSrv.GracefulStop()
	// After the server: its last calls' spans are in the batch.
	if err := tr.Shutdown(); err != nil {
		slog.Warn("traces", "err", err)
	}
}

// startReconcile runs the reconcile loop on the configured tick, with the
// configured idle timeout for shared instances. Both durations are positive:
// config.Load replaced anything else with its default before main got here.
func startReconcile(ctx context.Context, cfg config.Config, k8sc reconcile.JobLister, core reconcile.EventReporter, vc reconcile.Revoker) {
	loop := reconcile.New(k8sc, core, vc, cfg.SharedInstanceIdleTimeout)
	go loop.Run(ctx, cfg.ReconcileInterval)
}

// images is what the k8s client is built from: every part of the configuration
// that reaches a worker pod. A field added to k8s.Images and not set here
// compiles and reaches no pod, so TestImages fills the Config and checks that
// every field of the result is set.
func images(cfg config.Config, version string) k8s.Images {
	return k8s.Images{
		WorkerImage:           cfg.WorkerImage,
		SidecarImage:          cfg.SidecarImage,
		Version:               version,
		ThotAuthToken:         cfg.ThotAuthToken,
		ExecutorAddr:          cfg.ExecutorAddr,
		WorkspacePVC:          cfg.WorkspacePVC,
		SessionStorageClass:   cfg.SessionStorageClass,
		SessionNodeSelector:   cfg.NodeSelectorMap(),
		LogLevel:              cfg.LogLevel,
		CoreGRPCAddr:          cfg.CoreGRPCAddr,
		PostgresImage:         cfg.PostgresImage,
		RedisImage:            cfg.RedisImage,
		SharedInstancePVCSize: cfg.SharedInstancePVCSize,
		GitURLRewrite:         cfg.GitURLRewrite,
		ClaudeModel:           cfg.ClaudeModel,
		MaxTurns:              cfg.MaxTurns,
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

// parseIDs reads FLEET_GITHUB_ACCOUNTS. A malformed id is logged and skipped:
// it can only shrink the allowlist.
func parseIDs(raw string) []int64 {
	var out []int64
	for _, f := range strings.Split(raw, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			slog.Error("FLEET_GITHUB_ACCOUNTS: not an account id, skipped", "value", f)
			continue
		}
		out = append(out, id)
	}
	return out
}
