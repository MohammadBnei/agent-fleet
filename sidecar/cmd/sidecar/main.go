package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MohammadBnei/agent-fleet/sidecar/internal/config"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/coreclient"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/egress"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/localapi"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/mcpserver"
	"github.com/MohammadBnei/agent-fleet/sidecar/internal/telemetry"
)

func main() {
	// The environment is read here and nowhere else (internal/config). Its
	// refusals are held until their old places below: the log level comes
	// first, so the first refusal is already logged as configured, and
	// SESSION_ID is checked before LEASE_ID.
	cfg, cfgErr := config.FromProcess()
	level, warning := resolveLogLevel(cfg.LogLevel)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
	if warning != "" {
		slog.Warn(warning, "value", cfg.LogLevel)
	}

	taskID := cfg.SessionID
	if errors.Is(cfgErr, config.ErrNoSessionID) {
		slog.Error(config.ErrNoSessionID.Error())
		os.Exit(1)
	}
	// Stamp taskId onto every line from here on. Without it the sidecar's
	// logs carry nothing tying them to a task, so core's log viewer had no
	// way to scope a query except by parsing the pod name — which meant
	// duplicating the provisioner's shortID() truncation rule into core.
	// One attribute here removes that whole coupling: every fleet component
	// is then filterable with the same `| json | taskId="..."`.
	slog.SetDefault(slog.Default().With("sessionId", taskID))
	coreAddr := cfg.CoreGRPCAddr
	worktreePath := cfg.WorktreePath
	mcpPort := cfg.MCPPort
	localAPIPort := cfg.LocalAPIPort
	healthPort := cfg.HealthPort

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The session's lease authenticates every call to core. It is minted per
	// warm by sessions.ReserveSlot and cleared at teardown, so it identifies
	// this pod rather than just this session — CoreService rejects a stale one.
	leaseID := cfg.LeaseID
	if errors.Is(cfgErr, config.ErrNoLeaseID) {
		slog.Error(config.ErrNoLeaseID.Error())
		os.Exit(1)
	}
	// Any other problem Load may one day report is a refusal too: the two
	// above only keep their old places and words. Unreachable today.
	if cfgErr != nil {
		slog.Error("refusing to start", "error", cfgErr)
		os.Exit(1)
	}

	core, err := coreclient.New(coreAddr, taskID, leaseID)
	if err != nil {
		slog.Error("core client init failed", "error", err)
		os.Exit(1)
	}
	defer func() { _ = core.Close() }()

	// Backs /readyz: the provisioner's StartupProbe waits on this before
	// unblocking the worker container, so a core outage/rollout blocks the
	// worker from starting instead of it crashing on its first sidecar call
	// (observed live: worker died in <1s on an unguarded setStatus call
	// while core's Service briefly had zero endpoints).
	go func() {
		if err := core.WaitReady(ctx); err != nil {
			slog.Warn("core.WaitReady did not complete", "error", err)
		}
	}()

	go telemetry.Run(ctx, core, worktreePath, 5*time.Second)

	// docs/adr/0075 §7: a repo pod's HTTPS_PROXY. Set by the provisioner on
	// repo pods only; a definition pod talks to its vault directly.
	if vp := cfg.VaultProxy; vp != "" {
		router, err := egress.New(vp, cfg.VaultHosts)
		if err != nil {
			slog.Error("egress router", "error", err)
			os.Exit(1)
		}
		// Bound before /readyz can pass: the worker starts only after it, and
		// every HTTPS call it makes comes here. A failed bind fails the pod.
		lis, err := net.Listen("tcp", "127.0.0.1:"+cfg.EgressPort)
		if err != nil {
			slog.Error("egress router listen", "error", err)
			os.Exit(1)
		}
		// No access log: one line per tunnel would be most of the log.
		srv := &http.Server{Handler: router, ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			slog.Info("sidecar egress listening", "addr", lis.Addr().String())
			if err := srv.Serve(lis); err != nil {
				slog.Error("egress router exited", "error", err)
			}
		}()
	}

	// The FLEET_ENDPOINTS roster and the e2eclient that consumed it are gone
	// with the sandbox they addressed (docs/adr/0048 §6). docs/adr/0045's
	// direct-dial decision is not reversed — it is moot: there is no second pod
	// to dial. The sidecar's outbound connections are the one to core it always
	// had and, in a repo pod, the egress router's tunnels.
	mcpServer, localAPIServer, healthServer := servers(core, mcpPort, localAPIPort, healthPort)

	go func() {
		slog.Info("sidecar mcp listening", "port", mcpPort)
		if err := mcpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("mcp server exited", "error", err)
		}
	}()
	go func() {
		slog.Info("sidecar local api listening", "port", localAPIPort)
		if err := localAPIServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("local api server exited", "error", err)
		}
	}()
	go func() {
		slog.Info("sidecar health listening", "port", healthPort)
		if err := healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("health server exited", "error", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = mcpServer.Shutdown(shutdownCtx)
	_ = localAPIServer.Shutdown(shutdownCtx)
	_ = healthServer.Shutdown(shutdownCtx)
}

// servers builds the sidecar's three listeners. Split out of main purely so
// the bind addresses are assertable — the ":" that used to be on the first two
// is the entire reason this function exists, and nothing else in the process
// would have failed if it came back.
//
// mcp and local api bind 127.0.0.1. Every route on them acts under THIS
// session's authority: post a message as it, change its permission mode, read
// the live SSE feed of everything a human has typed to it. A ":" bind
// published all of that on the pod IP, reachable by any pod in the namespace
// — and the per-task NetworkPolicies that would have covered it were deleted
// with the sandbox (docs/adr/0048 §6), so nothing else stood in the way. Both
// package comments always said "localhost"; only the bind address disagreed.
//
// health is the one exception, and the reason /readyz lives on its own mux:
// kubelet dials the StartupProbe at the pod IP. See k8s.SidecarHealthPort.
func servers(core *coreclient.Client, mcpPort, localAPIPort, healthPort string) (mcp, localAPI, health *http.Server) {
	return &http.Server{Addr: "127.0.0.1:" + mcpPort, Handler: withAccessLog("sidecar mcp", mcpserver.New(core)), ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)},
		&http.Server{Addr: "127.0.0.1:" + localAPIPort, Handler: withAccessLog("sidecar local api", localapi.New(core)), ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)},
		&http.Server{Addr: ":" + healthPort, Handler: withAccessLog("sidecar health", localapi.NewHealth(core)), ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)}
}

// withAccessLog logs one line per request (method, path, status, duration)
// — both of sidecar's HTTP muxes had zero request-level logging before.
func withAccessLog(name string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)
		slog.Info(name, "method", r.Method, "path", r.URL.Path, "status", sw.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Flush forwards to the underlying ResponseWriter's Flusher. Without this,
// embedding http.ResponseWriter as an interface field doesn't promote
// Flush() — humanMessagesHandler's `w.(http.Flusher)` assertion always
// failed through this wrapper, so /human-messages always returned 500
// (confirmed live: every worker session's approve/abort signal silently
// never arrived, forcing the agent to discover approval by polling the
// transcript instead).
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
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
