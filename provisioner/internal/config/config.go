package config

import "os"

type Config struct {
	Namespace      string
	E2eRunnerImage string
	WorkerImage    string
	SidecarImage   string
	// WorkspacePVC is the one shared PVC name (docs/adr/0019) — replaces
	// the old per-repo WorkspacePvcFor(repo) naming, since there's only
	// one workspace PVC in the fleet now, not one per repo.
	WorkspacePVC string
	// WorktreesRoot is where that PVC is mounted inside THIS pod — the
	// provisioner needs its own read-write mount to run git clone/fetch/
	// worktree add before a worker pod exists (docs/adr/0019 point 2:
	// the provisioner owns the entire git lifecycle on the shared PVC).
	WorktreesRoot string
	E2eHost       string
	Port          string
	GRPCPort      string
	// CoreGRPCAddr is where ReportPodEvents streams to (docs/adr/0020
	// point 3) — the provisioner is a gRPC client of core for this one
	// call, on top of being core's gRPC server for everything else.
	CoreGRPCAddr      string
	// Passed through to each worker pod's sidecar so ask_thot registers
	// (docs/adr/0035). Empty leaves the tool unregistered — which is how
	// the feature silently did nothing in production until this was wired.
	ThotGRPCAddr  string
	ThotAuthToken string
	ExecutorAddr  string
	ReconcileInterval string
	// SweepInterval is how often the [gone]-branch sweep runs
	// (reliability-findings.md #2) — minutes, not seconds: it does a real
	// `git fetch` per repo, unlike the k8s-only reconcile loop.
	SweepInterval string
	// LogLevel is one of debug/info/warn/error (case-insensitive), parsed
	// via slog.Level.UnmarshalText in cmd/provisioner/main.go.
	LogLevel string
	// FleetSharedRepoURL/FleetSharedBranch are the git.Manager.SyncFleetShared
	// source (docs/adr/0032) — the git-tracked fleet-shared/ dir mirrored
	// into every worker pod's CLAUDE_CONFIG_DIR.
	FleetSharedRepoURL string
	FleetSharedBranch  string
	// ClaudeHomeDir must equal the claudeConfigDir constant in
	// provisioner/internal/k8s/pod.go (the value forwarded to worker pods as
	// CLAUDE_CONFIG_DIR) — kept as a separate literal rather than threaded
	// through k8s.Client for a smaller diff; if you change one, change both.
	ClaudeHomeDir string
	// PostgresImage/RedisImage back the "postgres"/"redis" service
	// ingredients' shared instances (docs/adr/0034) — operationally
	// configurable (version/security-patch bumps) unlike a tool
	// ingredient's CopyImage, which is a code-level fact tied to specific
	// in-image binary paths (see catalog.Tools).
	PostgresImage string
	RedisImage    string
	// SharedInstancePVCSize is the postgres shared instance's data-volume
	// size — redis needs none (NeedsPVC is false, pure cache).
	SharedInstancePVCSize string
	// SharedInstanceIdleTimeoutMs mirrors core's own warm-pod idle-timeout
	// config shape (docs/adr/0029), translated to Kubernetes since the
	// provisioner has no Postgres column to put a last-active timestamp in
	// — reconcile.Loop compares this against a last-used-at annotation
	// instead (docs/adr/0034).
	SharedInstanceIdleTimeoutMs string
}

func Load() Config {
	return Config{
		Namespace:                   env("NAMESPACE", "agent-fleet"),
		E2eRunnerImage:              env("E2E_RUNNER_IMAGE", "mohammaddocker/agent-fleet-e2e-runner:latest"),
		WorkerImage:                 env("WORKER_IMAGE", "mohammaddocker/agent-fleet-worker:latest"),
		SidecarImage:                env("SIDECAR_IMAGE", "mohammaddocker/agent-fleet-sidecar:latest"),
		WorkspacePVC:                env("WORKSPACE_PVC", "agent-fleet-workspace"),
		WorktreesRoot:               env("WORKTREES_ROOT", "/workspace"),
		E2eHost:                     env("E2E_HOST", "e2e.bnei.dev"),
		Port:                        env("PORT", "8080"),
		GRPCPort:                    env("GRPC_PORT", "9090"),
		CoreGRPCAddr:                env("CORE_GRPC_ADDR", "agent-fleet-core.agent-fleet.svc.cluster.local:9090"),
		ThotGRPCAddr:                env("THOT_GRPC_ADDR", "thot.thot.svc.cluster.local:9090"),
		ThotAuthToken:               env("THOT_AUTH_TOKEN", ""),
		ExecutorAddr:                env("EXECUTOR_ADDR", "thot-executor.thot.svc.cluster.local:9090"),
		ReconcileInterval:           env("RECONCILE_INTERVAL_MS", "10000"),
		SweepInterval:               env("SWEEP_INTERVAL_MS", "300000"),
		LogLevel:                    env("LOG_LEVEL", "info"),
		FleetSharedRepoURL:          env("FLEET_SHARED_REPO_URL", "https://github.com/MohammadBnei/agent-fleet.git"),
		FleetSharedBranch:           env("FLEET_SHARED_BRANCH", "main"),
		ClaudeHomeDir:               env("CLAUDE_HOME_DIR", "/workspace/.claude-home"),
		PostgresImage:               env("POSTGRES_IMAGE", "postgres:16-alpine"),
		RedisImage:                  env("REDIS_IMAGE", "redis:7-alpine"),
		SharedInstancePVCSize:       env("SHARED_INSTANCE_PVC_SIZE", "2Gi"),
		SharedInstanceIdleTimeoutMs: env("SHARED_INSTANCE_IDLE_TIMEOUT_MS", "43200000"), // 12h
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
