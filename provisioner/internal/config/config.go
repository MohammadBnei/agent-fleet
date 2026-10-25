package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Defaults the egress lock (k8s/provisioner/definition-egress.yaml) pins:
// definition pods may dial exactly these two hosts (docs/adr/0065 §5).
const (
	DefaultCoreGRPCAddr    = "agent-fleet-core.agent-fleet.svc.cluster.local:9090"
	DefaultAgentVaultProxy = "agent-vault.agent-vault.svc.cluster.local:14322"
	DefaultAgentVaultAPI   = "http://agent-vault.agent-vault.svc.cluster.local:14321"
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
	// CoreToken authenticates this provisioner to CoreService, which rejects
	// unauthenticated callers. Arrives from the same whole-scope Infisical sync
	// the other secrets here use (k8s/provisioner/infisicalsecret.yaml declares
	// no key list and the Deployment consumes it with envFrom), so adding the
	// key needs no manifest change.
	CoreToken string
	// CoreProvisionerToken is what core must present to THIS process's
	// ProvisionerService (docs/adr/0069), the reverse of CoreToken and a
	// separate key. Empty refuses to start: see grpcserver.NewServer.
	CoreProvisionerToken string
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
	// FleetDefinitionsRef is the ref definitions/ is read at from
	// FleetSharedRepoURL (docs/adr/0062). Empty = this build's release tag;
	// kind and dev set a branch.
	FleetDefinitionsRef string
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
	// AgentVault* locate infra-bootstrap's Agent Vault (docs/adr/0065): its API
	// (provisioner only), its MITM proxy (definition pods), and the mounted
	// Secret holding the agent token and the credential values.
	AgentVaultAPI        string
	AgentVaultProxy      string
	AgentVaultSecretsDir string
	// GitHubAppDir holds the App ukubi-fleet's GITHUB_APP_ID and
	// GITHUB_APP_PRIVATE_KEY as files (docs/adr/0075). GitHubAccounts is
	// FLEET_GITHUB_ACCOUNTS, comma-separated account ids whose installations
	// count: the App is public.
	GitHubAppDir   string
	GitHubAccounts string
	// GitAuthorName/GitAuthorEmail author repo sessions' commits
	// (docs/adr/0075 §8); definitions keep their git.identity.
	GitAuthorName  string
	GitAuthorEmail string
	// GitURLRewrite and OwnerTokenFile are the offline local stack's
	// (docs/adr/0080 §1-2), unset in production. GitURLRewrite is the forge
	// every worker's `https://github.com/` rewrites to; OwnerTokenFile is the
	// owner vaults' GitHub token when no App is mounted.
	GitURLRewrite  string
	OwnerTokenFile string
}

func Load() Config {
	return Config{
		Namespace: env("NAMESPACE", "agent-fleet"),
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
		CoreGRPCAddr:                env("CORE_GRPC_ADDR", DefaultCoreGRPCAddr),
		CoreToken:                   os.Getenv("FLEET_PROVISIONER_TOKEN"),
		CoreProvisionerToken:        os.Getenv("FLEET_CORE_PROVISIONER_TOKEN"),
		ThotAuthToken:               env("THOT_AUTH_TOKEN", ""),
		ExecutorAddr:                env("EXECUTOR_ADDR", "thot-executor.thot.svc.cluster.local:9090"),
		ReconcileInterval:           env("RECONCILE_INTERVAL_MS", "10000"),
		LogLevel:                    env("LOG_LEVEL", "info"),
		FleetSharedRepoURL:          env("FLEET_SHARED_REPO_URL", "https://github.com/MohammadBnei/agent-fleet.git"),
		FleetSharedBranch:           env("FLEET_SHARED_BRANCH", "main"),
		FleetDefinitionsRef:         env("FLEET_DEFINITIONS_REF", ""),
		PostgresImage:               env("POSTGRES_IMAGE", "postgres:16-alpine"),
		RedisImage:                  env("REDIS_IMAGE", "redis:7-alpine"),
		SharedInstancePVCSize:       env("SHARED_INSTANCE_PVC_SIZE", "2Gi"),
		SharedInstanceIdleTimeoutMs: env("SHARED_INSTANCE_IDLE_TIMEOUT_MS", "43200000"), // 12h
		AgentVaultAPI:               env("AGENT_VAULT_API_ADDR", DefaultAgentVaultAPI),
		AgentVaultProxy:             env("AGENT_VAULT_PROXY_ADDR", DefaultAgentVaultProxy),
		AgentVaultSecretsDir:        env("AGENT_VAULT_SECRETS_DIR", "/var/run/agent-vault"),
		GitHubAppDir:                env("GITHUB_APP_DIR", "/var/run/github-app"),
		GitHubAccounts:              env("FLEET_GITHUB_ACCOUNTS", ""),
		GitAuthorName:               env("FLEET_GIT_AUTHOR_NAME", "ukubi"),
		GitAuthorEmail:              env("FLEET_GIT_AUTHOR_EMAIL", "ukubi@bnei.dev"),
		GitURLRewrite:               env("FLEET_GIT_URL_REWRITE", ""),
		OwnerTokenFile:              env("FLEET_OWNER_TOKEN_FILE", ""),
	}
}

// Validate refuses the local stack's settings where they could do harm
// (docs/adr/0080). The rewrite sends every worker's GitHub traffic elsewhere,
// so it is refused against the production vault, matched by host name: the
// production deployment sets no AGENT_VAULT_* and takes its Infisical project
// whole, so a stray key must not be enough. The owner token file must be one of
// the vault's own secret files, never any file the provisioner can read.
func (c Config) Validate() error {
	var errs []error
	if c.GitURLRewrite != "" {
		u, err := url.Parse(c.GitURLRewrite)
		switch {
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || !strings.HasSuffix(u.Path, "/"):
			errs = append(errs, fmt.Errorf("FLEET_GIT_URL_REWRITE %q must be an http(s) URL with a host, no user info and a trailing /", c.GitURLRewrite))
		case productionVault(c.AgentVaultAPI) || productionVault(c.AgentVaultProxy):
			errs = append(errs, errors.New("FLEET_GIT_URL_REWRITE is set while the Agent Vault address is production's: it is for the local stack only (docs/adr/0080)"))
		}
	}
	if c.OwnerTokenFile != "" {
		if rel, err := filepath.Rel(c.AgentVaultSecretsDir, filepath.Clean(c.OwnerTokenFile)); err != nil || rel == "." || strings.HasPrefix(rel, "..") || strings.Contains(rel, "/") {
			errs = append(errs, fmt.Errorf("FLEET_OWNER_TOKEN_FILE %q must be a file in AGENT_VAULT_SECRETS_DIR %q", c.OwnerTokenFile, c.AgentVaultSecretsDir))
		}
	}
	return errors.Join(errs...)
}

// productionVault reports whether addr (a URL or host:port) names the
// production vault Service, in any of its cluster DNS forms.
func productionVault(addr string) bool {
	host := addr
	if u, err := url.Parse(addr); err == nil && u.Host != "" {
		host = u.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, h := range []string{"agent-vault.agent-vault", "agent-vault.agent-vault.svc", "agent-vault.agent-vault.svc.cluster.local"} {
		if host == h {
			return true
		}
	}
	return false
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
