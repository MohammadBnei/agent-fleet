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
	ReconcileInterval string
	// SweepInterval is how often the [gone]-branch sweep runs
	// (reliability-findings.md #2) — minutes, not seconds: it does a real
	// `git fetch` per repo, unlike the k8s-only reconcile loop.
	SweepInterval string
	// LogLevel is one of debug/info/warn/error (case-insensitive), parsed
	// via slog.Level.UnmarshalText in cmd/provisioner/main.go.
	LogLevel string
}

func Load() Config {
	return Config{
		Namespace:         env("NAMESPACE", "agent-fleet"),
		E2eRunnerImage:    env("E2E_RUNNER_IMAGE", "mohammaddocker/agent-fleet-e2e-runner:latest"),
		WorkerImage:       env("WORKER_IMAGE", "mohammaddocker/agent-fleet-worker:latest"),
		SidecarImage:      env("SIDECAR_IMAGE", "mohammaddocker/agent-fleet-sidecar:latest"),
		WorkspacePVC:      env("WORKSPACE_PVC", "agent-fleet-workspace"),
		WorktreesRoot:     env("WORKTREES_ROOT", "/workspace"),
		E2eHost:           env("E2E_HOST", "e2e.bnei.dev"),
		Port:              env("PORT", "8080"),
		GRPCPort:          env("GRPC_PORT", "9090"),
		CoreGRPCAddr:      env("CORE_GRPC_ADDR", "core.agent-fleet.svc.cluster.local:9090"),
		ReconcileInterval: env("RECONCILE_INTERVAL_MS", "10000"),
		SweepInterval:     env("SWEEP_INTERVAL_MS", "300000"),
		LogLevel:          env("LOG_LEVEL", "info"),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
