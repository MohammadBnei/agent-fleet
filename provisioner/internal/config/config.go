package config

import "os"

type Config struct {
	Namespace    string
	WorkerImage  string
	SidecarImage string
	// WorkspacePVC is the one shared PVC name (docs/adr/0019) — replaces
	// the old per-repo WorkspacePvcFor(repo) naming, since there's only
	// one workspace PVC in the fleet now, not one per repo.
	WorkspacePVC string
	// SessionStorageClass is the StorageClass for per-session working volumes.
	SessionStorageClass string
	// WorkspaceRoot is where the shared PVC is mounted inside THIS pod. The
	// provisioner needs its own read-write mount to maintain the clone cache
	// and to seed each session's claude-home before its pod exists.
	//
	// It does NOT hold session working trees any more (docs/adr/0048 §4):
	// those are per-session volumes this process never mounts, cloned into by
	// an init container in the session's own pod.
	WorkspaceRoot string
	E2eHost       string
	Port          string
	GRPCPort      string
	// CoreGRPCAddr is where ReportPodEvents streams to (docs/adr/0020
	// point 3) — the provisioner is a gRPC client of core for this one
	// call, on top of being core's gRPC server for everything else.
	CoreGRPCAddr string
	// Passed through to each worker pod's sidecar so ask_thot registers
	// (docs/adr/0035). Empty leaves the tool unregistered — which is how
	// the feature silently did nothing in production until this was wired.
	ThotAuthToken     string
	ExecutorAddr      string
	ReconcileInterval string
	// LogLevel is one of debug/info/warn/error (case-insensitive), parsed
	// via slog.Level.UnmarshalText in cmd/provisioner/main.go.
	LogLevel string
	// FleetSharedRepoURL/FleetSharedBranch are the git.Manager.SyncFleetShared
	// source (docs/adr/0032) — the git-tracked fleet-shared/ dir mirrored
	// into every worker pod's CLAUDE_CONFIG_DIR.
	FleetSharedRepoURL string
	FleetSharedBranch  string
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
		WorkerImage:                 env("WORKER_IMAGE", "mohammaddocker/agent-fleet-worker:latest"),
		SidecarImage:                env("SIDECAR_IMAGE", "mohammaddocker/agent-fleet-sidecar:latest"),
		WorkspacePVC:                env("WORKSPACE_PVC", "agent-fleet-workspace"),
		SessionStorageClass:         env("SESSION_STORAGE_CLASS", ""),
		WorkspaceRoot:               env("WORKSPACE_ROOT", "/workspace"),
		E2eHost:                     env("E2E_HOST", "e2e.bnei.dev"),
		Port:                        env("PORT", "8080"),
		GRPCPort:                    env("GRPC_PORT", "9090"),
		CoreGRPCAddr:                env("CORE_GRPC_ADDR", "agent-fleet-core.agent-fleet.svc.cluster.local:9090"),
		ThotAuthToken:               env("THOT_AUTH_TOKEN", ""),
		ExecutorAddr:                env("EXECUTOR_ADDR", "thot-executor.thot.svc.cluster.local:9090"),
		ReconcileInterval:           env("RECONCILE_INTERVAL_MS", "10000"),
		LogLevel:                    env("LOG_LEVEL", "info"),
		FleetSharedRepoURL:          env("FLEET_SHARED_REPO_URL", "https://github.com/MohammadBnei/agent-fleet.git"),
		FleetSharedBranch:           env("FLEET_SHARED_BRANCH", "main"),
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
