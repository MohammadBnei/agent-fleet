package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Defaults the egress lock (k8s/provisioner/definition-egress.yaml) pins:
// definition pods may dial exactly these two hosts (docs/adr/0065 §5).
const (
	DefaultCoreGRPCAddr    = "agent-fleet-core.agent-fleet.svc.cluster.local:9090"
	DefaultAgentVaultProxy = "agent-vault.agent-vault.svc.cluster.local:14322"
	DefaultAgentVaultAPI   = "http://agent-vault.agent-vault.svc.cluster.local:14321"
)

// Config is the provisioner's whole configuration: one struct, read from the
// environment once (FromProcess) or from a test's map (Load), and nothing else
// in this module reads the environment (TestEnvBan; mountain plan/15 §5). Each
// field names the variable it comes from. A blank variable is an unset one,
// except where a field says it is read raw.
//
// Optional keys have a default, or unset is a supported state: one that is set
// but invalid is warned about (Warnings) and takes its default, and the process
// starts. Only a required key, one the app cannot run without, refuses the
// start. The provisioner has one: FLEET_CORE_PROVISIONER_TOKEN, which
// cmd/provisioner hands to grpcserver.NewGRPCServer rather than Load checking it.
// A security setting fails closed whatever its optionality: a malformed
// FLEET_GIT_URL_REWRITE refuses the start too (see GitURLRewrite).
//
// A value that is printed in a warning or a refusal is printed by shown: one
// containing "@" may hold credentials and is not echoed.
type Config struct {
	// Warnings is one line per optional variable that was set but invalid and
	// was replaced by its default (or, for FLEET_GIT_URL_REWRITE against the
	// production vault, dropped), each naming the variable and the rule. Load
	// fills it; cmd/provisioner logs each, whatever LOG_LEVEL is. Not read from
	// the environment.
	Warnings []string

	// Namespace is the namespace session pods and shared instances live in
	// (NAMESPACE).
	Namespace string
	// WorkerImage and SidecarImage are the two containers of every session
	// pod (WORKER_IMAGE, SIDECAR_IMAGE).
	WorkerImage  string
	SidecarImage string
	// WorkspacePVC is the one shared PVC name (docs/adr/0019, WORKSPACE_PVC) —
	// replaces the old per-repo WorkspacePvcFor(repo) naming, since there's
	// only one workspace PVC in the fleet now, not one per repo.
	WorkspacePVC string
	// SessionStorageClass is the StorageClass for per-session working volumes
	// (SESSION_STORAGE_CLASS).
	SessionStorageClass string
	// SessionNodeSelector constrains where session pods may be scheduled,
	// as a single "key=value" label. Empty means no constraint
	// (SESSION_NODE_SELECTOR).
	//
	// Sessions belong on nodes big enough to build on: the control-plane
	// nodes are 2 vCPU / 4 GB with ~33 GiB allocatable, and filling one is a
	// cluster incident rather than a session failure. Empty is what
	// /kind-local relies on — its single node carries no label.
	SessionNodeSelector string
	// WorkspaceRoot is where the shared PVC is mounted inside THIS pod
	// (WORKSPACE_ROOT). The provisioner needs its own read-write mount to
	// maintain the clone cache and to seed each session's claude-home before
	// its pod exists.
	//
	// It does NOT hold session working trees any more (docs/adr/0048 §4):
	// those are per-session volumes this process never mounts, cloned into by
	// an init container in the session's own pod.
	WorkspaceRoot string
	// E2eHost is the base domain preview hostnames are built under (E2E_HOST).
	E2eHost string
	// Port is the HTTP port of /healthz and /metrics (PORT) and GRPCPort the
	// one ProvisionerService listens on (GRPC_PORT). Strings, as the
	// listeners want them: a value that is no port fails at Listen.
	Port     string
	GRPCPort string
	// CoreGRPCAddr is where ReportPodEvents streams to (docs/adr/0020
	// point 3, CORE_GRPC_ADDR) — the provisioner is a gRPC client of core for
	// this one call, on top of being core's gRPC server for everything else.
	CoreGRPCAddr string
	// CoreToken authenticates this provisioner to CoreService, which rejects
	// unauthenticated callers (FLEET_PROVISIONER_TOKEN, read raw). Arrives
	// from the same whole-scope Infisical sync the other secrets here use
	// (k8s/provisioner/infisicalsecret.yaml declares no key list and the
	// Deployment consumes it with envFrom), so adding the key needs no
	// manifest change.
	CoreToken string
	// CoreProvisionerToken is what core must present to THIS process's
	// ProvisionerService (docs/adr/0069), the reverse of CoreToken and a
	// separate key (FLEET_CORE_PROVISIONER_TOKEN, read raw). Empty, blank or
	// shorter than grpcserver.MinTokenLen refuses to start: see
	// grpcserver.NewGRPCServer.
	CoreProvisionerToken string
	// ThotAuthToken is passed through to each worker pod's sidecar so
	// ask_thot registers (docs/adr/0035, THOT_AUTH_TOKEN). Empty leaves the
	// tool unregistered — which is how the feature silently did nothing in
	// production until this was wired.
	ThotAuthToken string
	// ExecutorAddr is where the cluster-access ingredient's kubectl shim
	// sends argv (docs/adr/0037, EXECUTOR_ADDR).
	ExecutorAddr string
	// ReconcileInterval is the reconcile loop's tick (RECONCILE_INTERVAL_MS,
	// a positive integer of milliseconds; 10s unset). Any other value warns
	// and takes the 10s, so a Config Load returns always holds a positive
	// duration: time.NewTicker panics on anything else.
	ReconcileInterval time.Duration
	// LogLevel is one of debug/info/warn/error (case-insensitive), parsed
	// via slog.Level.UnmarshalText in cmd/provisioner/main.go (LOG_LEVEL).
	LogLevel string
	// FleetSharedRepoURL/FleetSharedBranch are the git.Manager.SyncFleetShared
	// source (docs/adr/0032) — the git-tracked fleet-shared/ dir mirrored
	// into every worker pod's CLAUDE_CONFIG_DIR (FLEET_SHARED_REPO_URL,
	// FLEET_SHARED_BRANCH).
	FleetSharedRepoURL string
	FleetSharedBranch  string
	// FleetDefinitionsRef is the ref definitions/ is read at from
	// FleetSharedRepoURL (docs/adr/0062, FLEET_DEFINITIONS_REF). Empty = this
	// build's release tag; kind and dev set a branch.
	FleetDefinitionsRef string
	// PostgresImage/RedisImage back the "postgres"/"redis" service
	// ingredients' shared instances (docs/adr/0034; POSTGRES_IMAGE,
	// REDIS_IMAGE) — operationally configurable (version/security-patch
	// bumps) unlike a tool ingredient's CopyImage, which is a code-level fact
	// tied to specific in-image binary paths (see catalog.Tools).
	PostgresImage string
	RedisImage    string
	// SharedInstancePVCSize is the postgres shared instance's data-volume
	// size — redis needs none (NeedsPVC is false, pure cache)
	// (SHARED_INSTANCE_PVC_SIZE, a positive Kubernetes quantity; 2Gi unset).
	// Anything else warns and takes the 2Gi, so a Config Load returns always
	// holds a size resource.ParseQuantity accepts.
	SharedInstancePVCSize string
	// SharedInstanceIdleTimeout mirrors core's own warm-pod idle-timeout
	// config shape (docs/adr/0029), translated to Kubernetes since the
	// provisioner has no Postgres column to put a last-active timestamp in
	// — reconcile.Loop compares this against a last-used-at annotation
	// instead (docs/adr/0034). SHARED_INSTANCE_IDLE_TIMEOUT_MS, a positive
	// integer of milliseconds; 12h unset. Any other value warns and takes the
	// 12h: a timeout of zero or less would make every annotated shared
	// instance (a Postgres with its PVC included) idle on every reconcile tick.
	SharedInstanceIdleTimeout time.Duration
	// AgentVaultAPI, AgentVaultProxy and AgentVaultSecretsDir locate
	// infra-bootstrap's Agent Vault (docs/adr/0065): its API (provisioner
	// only), its MITM proxy (definition pods), and the mounted Secret holding
	// the agent token and the credential values (AGENT_VAULT_API_ADDR,
	// AGENT_VAULT_PROXY_ADDR, AGENT_VAULT_SECRETS_DIR).
	AgentVaultAPI        string
	AgentVaultProxy      string
	AgentVaultSecretsDir string
	// GitHubAppDir holds the App ukubi-fleet's GITHUB_APP_ID and
	// GITHUB_APP_PRIVATE_KEY as files (docs/adr/0075, GITHUB_APP_DIR).
	// GitHubAccounts is FLEET_GITHUB_ACCOUNTS, comma-separated account ids
	// whose installations count: the App is public. Kept as the raw string:
	// cmd/provisioner parses it, so a malformed id is logged where it always
	// was.
	GitHubAppDir   string
	GitHubAccounts string
	// GitAuthorName/GitAuthorEmail author repo sessions' commits
	// (docs/adr/0075 §8; FLEET_GIT_AUTHOR_NAME, FLEET_GIT_AUTHOR_EMAIL);
	// definitions keep their git.identity.
	GitAuthorName  string
	GitAuthorEmail string
	// GitURLRewrite is the forge every worker's `https://github.com/`
	// rewrites to, on the offline local stack (docs/adr/0080 §2,
	// FLEET_GIT_URL_REWRITE). Unset in production. It contains the local
	// stack (without it a worker pushes to the real github.com through the
	// vault), so a MALFORMED value refuses the start: Load returns the error.
	// One set while the Agent Vault address is production's warns and is
	// dropped (left ""), which is exactly production's behaviour.
	GitURLRewrite string
	// Version is the release this binary runs as (FLEET_VERSION, set by
	// deploy on the Deployment, read raw). "" means a build deploy did not
	// stamp: cmd/provisioner then reports "dev".
	Version string
	// ClaudeModel and MaxTurns are the operator's model override and turn cap
	// for every worker pod (CLAUDE_MODEL, MAX_TURNS, read raw), forwarded as
	// the worker container's env of the same names. "" leaves Claude Code's
	// own default.
	ClaudeModel string
	MaxTurns    string
	// TracesEndpoint is the OTLP/HTTP traces URL, /v1/traces included
	// (OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, read raw). Empty exports nothing;
	// a malformed one refuses the start, in cmd/provisioner where telemetry.New
	// checks it.
	TracesEndpoint string
}

// FromProcess is the provisioner's configuration from its environment: the
// one call that reads it, and the only one in this module that does.
func FromProcess() (Config, error) { return Load(os.Getenv) }

// Load is the provisioner's configuration from getenv (FromProcess passes
// os.Getenv, a test a map). An optional variable that is set but invalid does
// not fail the load: a line naming the variable and the rule goes to
// Config.Warnings and the variable takes its default (a FLEET_GIT_URL_REWRITE
// set against the production vault is dropped), in field order, then the
// rewrite's. The error is for what must stop the start: a malformed
// FLEET_GIT_URL_REWRITE, a setting that contains the local stack and so fails
// closed. FLEET_CORE_PROVISIONER_TOKEN, the one required key, is refused by
// cmd/provisioner, not here.
func Load(getenv func(string) string) (Config, error) {
	env := func(key, fallback string) string { return envOr(getenv, key, fallback) }
	var warnings []string
	millis := func(key string, fallback time.Duration) time.Duration {
		d, err := envMillis(getenv, key, fallback)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%v; using the default %s", err, durationText(fallback)))
		}
		return d
	}
	pvcSize := func() string {
		v := env("SHARED_INSTANCE_PVC_SIZE", defaultSharedInstancePVCSize)
		if err := checkQuantity(v); err != nil {
			warnings = append(warnings, fmt.Sprintf("SHARED_INSTANCE_PVC_SIZE must be a positive Kubernetes quantity (e.g. 2Gi), got %q: %v; using the default %s", shown(v), err, defaultSharedInstancePVCSize))
			return defaultSharedInstancePVCSize
		}
		return v
	}
	c := Config{
		Namespace: env("NAMESPACE", "agent-fleet"),
		// registry.bnei.lan:5000 is ukubi-cluster's in-cluster Zot registry
		// (infra-bootstrap ADR-0034), which containerd is configured to trust
		// over plain HTTP — `.lan` is a name Let's Encrypt cannot issue for,
		// so there is no ingress and no certificate. Anonymous pull, so no
		// imagePullSecrets. k8s/provisioner/deployment.yaml overrides both
		// with the release-pinned tags; these :latest defaults only apply to
		// a local/kind run.
		WorkerImage:               env("WORKER_IMAGE", "registry.bnei.lan:5000/agent-fleet-worker:latest"),
		SidecarImage:              env("SIDECAR_IMAGE", "registry.bnei.lan:5000/agent-fleet-sidecar:latest"),
		WorkspacePVC:              env("WORKSPACE_PVC", "agent-fleet-workspace"),
		SessionStorageClass:       env("SESSION_STORAGE_CLASS", ""),
		SessionNodeSelector:       env("SESSION_NODE_SELECTOR", ""),
		WorkspaceRoot:             env("WORKSPACE_ROOT", "/workspace"),
		E2eHost:                   env("E2E_HOST", "bnei.dev"),
		Port:                      env("PORT", "8080"),
		GRPCPort:                  env("GRPC_PORT", "9090"),
		CoreGRPCAddr:              env("CORE_GRPC_ADDR", DefaultCoreGRPCAddr),
		CoreToken:                 getenv("FLEET_PROVISIONER_TOKEN"),
		CoreProvisionerToken:      getenv("FLEET_CORE_PROVISIONER_TOKEN"),
		ThotAuthToken:             env("THOT_AUTH_TOKEN", ""),
		ExecutorAddr:              env("EXECUTOR_ADDR", "thot-executor.thot.svc.cluster.local:9090"),
		ReconcileInterval:         millis("RECONCILE_INTERVAL_MS", 10*time.Second),
		LogLevel:                  env("LOG_LEVEL", "info"),
		FleetSharedRepoURL:        env("FLEET_SHARED_REPO_URL", "https://github.com/MohammadBnei/agent-fleet.git"),
		FleetSharedBranch:         env("FLEET_SHARED_BRANCH", "main"),
		FleetDefinitionsRef:       env("FLEET_DEFINITIONS_REF", ""),
		PostgresImage:             env("POSTGRES_IMAGE", "postgres:16-alpine"),
		RedisImage:                env("REDIS_IMAGE", "redis:7-alpine"),
		SharedInstancePVCSize:     pvcSize(),
		SharedInstanceIdleTimeout: millis("SHARED_INSTANCE_IDLE_TIMEOUT_MS", 12*time.Hour),
		AgentVaultAPI:             env("AGENT_VAULT_API_ADDR", DefaultAgentVaultAPI),
		AgentVaultProxy:           env("AGENT_VAULT_PROXY_ADDR", DefaultAgentVaultProxy),
		AgentVaultSecretsDir:      env("AGENT_VAULT_SECRETS_DIR", "/var/run/agent-vault"),
		GitHubAppDir:              env("GITHUB_APP_DIR", "/var/run/github-app"),
		GitHubAccounts:            env("FLEET_GITHUB_ACCOUNTS", ""),
		GitAuthorName:             env("FLEET_GIT_AUTHOR_NAME", "ukubi"),
		GitAuthorEmail:            env("FLEET_GIT_AUTHOR_EMAIL", "ukubi@bnei.dev"),
		GitURLRewrite:             env("FLEET_GIT_URL_REWRITE", ""),
		Version:                   getenv("FLEET_VERSION"),
		ClaudeModel:               getenv("CLAUDE_MODEL"),
		MaxTurns:                  getenv("MAX_TURNS"),
		TracesEndpoint:            getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
	}
	var errs []error
	if err := c.rewriteMalformed(); err != nil {
		errs = append(errs, err)
	} else if err := c.rewriteOnProductionVault(); err != nil {
		warnings = append(warnings, err.Error()+"; ignored")
		c.GitURLRewrite = ""
	}
	c.Warnings = warnings
	return c, errors.Join(errs...)
}

// durationText is d as an operator writes it: 12h, not 12h0m0s.
func durationText(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// defaultSharedInstancePVCSize is SHARED_INSTANCE_PVC_SIZE unset, and what a
// value that is no usable quantity falls back to.
const defaultSharedInstancePVCSize = "2Gi"

// checkQuantity is nil when s is a positive Kubernetes quantity: the form
// resource.ParseQuantity accepts (resource.MustParse, which the shared
// instance's PVC used to be built with, panics on exactly the rest), and above
// zero, since a PVC asking for nothing is refused by the API server at the
// first Postgres shared instance rather than here.
func checkQuantity(s string) error {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return err
	}
	if q.Sign() <= 0 {
		return errors.New("not above zero")
	}
	return nil
}

// rewriteMalformed is the refusal of a FLEET_GIT_URL_REWRITE that is no
// http(s) URL with a host, no user info and a trailing /, or nil (docs/adr/0080).
// The rewrite contains the local stack: dropped, a worker would push to the
// real github.com through the vault while the provisioner's own insteadOf
// (local/kind/40-provisioner.yaml) still reads the raw value. So it fails closed.
func (c Config) rewriteMalformed() error {
	if c.GitURLRewrite == "" {
		return nil
	}
	u, err := url.Parse(c.GitURLRewrite)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || !strings.HasSuffix(u.Path, "/") {
		return fmt.Errorf("FLEET_GIT_URL_REWRITE %q must be an http(s) URL with a host, no user info and a trailing /", shown(c.GitURLRewrite))
	}
	return nil
}

// rewriteOnProductionVault says why a well-formed rewrite cannot be used: it
// sends every worker's GitHub traffic elsewhere, so it is not used against the
// production vault, matched by host name. The production deployment sets no
// AGENT_VAULT_* and takes its Infisical project whole, so a stray key must not
// be enough. Load drops the rewrite (production's behaviour), with a warning.
// Defence in depth: the vault injects GitHub credentials for github.com only,
// never for the rewritten host.
func (c Config) rewriteOnProductionVault() error {
	if c.GitURLRewrite != "" && (productionVault(c.AgentVaultAPI) || productionVault(c.AgentVaultProxy)) {
		return errors.New("FLEET_GIT_URL_REWRITE is set while the Agent Vault address is production's: it is for the local stack only (docs/adr/0080)")
	}
	return nil
}

// shown is raw as a warning or a refusal may print it: a value containing "@"
// might hold credentials (user info, whatever its form) and is not echoed.
func shown(raw string) string {
	if strings.Contains(raw, "@") {
		return "<redacted: contains @>"
	}
	return raw
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

// envMillis is the duration in variable key, a positive integer of
// milliseconds, or fallback when it is unset or empty (getenv cannot tell the
// two apart; a whitespace-only value is not a number). Anything else comes back
// as fallback with an error naming the variable and the rule, for the caller to
// warn with: no partial parse (1.5, 12s), nothing zero or negative, nothing that
// overflows a time.Duration. A leading "+" is allowed, as strconv.Atoi allowed
// it before this check.
func envMillis(getenv func(string) string, key string, fallback time.Duration) (time.Duration, error) {
	raw := getenv(key)
	if raw == "" {
		return fallback, nil
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	switch {
	case err != nil && errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(raw, "-"):
		return fallback, fmt.Errorf("%s must be a positive integer (milliseconds) no larger than %d, got %q", key, maxMillis, shown(raw))
	case err != nil || ms <= 0:
		return fallback, fmt.Errorf("%s must be a positive integer (milliseconds), got %q", key, shown(raw))
	case ms > maxMillis:
		return fallback, fmt.Errorf("%s must be a positive integer (milliseconds) no larger than %d, got %q", key, maxMillis, shown(raw))
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// maxMillis is the most milliseconds a time.Duration (nanoseconds, int64) holds.
const maxMillis = math.MaxInt64 / int64(time.Millisecond)

// envOr is getenv(key), or fallback when it is empty.
func envOr(getenv func(string) string, key, fallback string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}
