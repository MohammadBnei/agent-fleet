package config

import (
	"os"
	"strings"
)

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
	// SessionNodeSelector constrains where session pods may be scheduled,
	// as a single "key=value" label. Empty means no constraint.
	//
	// Sessions belong on nodes big enough to build on: the control-plane
	// nodes are 2 vCPU / 4 GB with ~33 GiB allocatable, and filling one is a
	// cluster incident rather than a session failure. Empty is what
	// /kind-local relies on — its single node carries no label.
	SessionNodeSelector string
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
		// registry.bnei.lan:5000 is ukubi-cluster's in-cluster Zot registry
		// (infra-bootstrap ADR-0034), which containerd is configured to trust
		// over plain HTTP — `.lan` is a name Let's Encrypt cannot issue for,
		// so there is no ingress and no certificate. Anonymous pull, so no
		// imagePullSecrets. k8s/provisioner/deployment.yaml overrides both
		// with the release-pinned tags; these :latest defaults only apply to
		// a local/kind run.
		WorkerImage:                 env("WORKER_IMAGE", "registry.bnei.lan:5000/agent-fleet-worker:latest"),
		SidecarImage:                env("SIDECAR_IMAGE", "registry.bnei.lan:5000/agent-fleet-sidecar:latest"),
		WorkspacePVC:                env("WORKSPACE_PVC", "agent-fleet-workspace"),
		SessionStorageClass:         env("SESSION_STORAGE_CLASS", ""),
		SessionNodeSelector:         env("SESSION_NODE_SELECTOR", ""),
		WorkspaceRoot:               env("WORKSPACE_ROOT", "/workspace"),
		E2eHost:                     env("E2E_HOST", "bnei.dev"),
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

// NodeSelectorMap parses SessionNodeSelector into the shape a PodSpec wants.
//
// Returns nil — not an empty map — for anything it cannot parse, including a
// value with no "=". nil is the "schedule anywhere" case, so a typo degrades to
// the previous behaviour rather than to a selector that matches no node and
// leaves every session Pending with no explanation.
func (c Config) NodeSelectorMap() map[string]string {
	key, value, found := strings.Cut(c.SessionNodeSelector, "=")
	key, value = strings.TrimSpace(key), strings.TrimSpace(value)
	if !found || key == "" {
		return nil
	}
	return map[string]string{key: value}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
