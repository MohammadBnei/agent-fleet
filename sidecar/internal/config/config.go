// Package config is the sidecar's whole configuration: one struct, read from the
// environment once (FromProcess) or from a test's map (Load). Nothing else in
// this module reads the environment (TestEnvBan; mountain plan/15 §5), apart
// from the two child-process pass-throughs that test names.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var setRe = regexp.MustCompile(`^(A|AB|AC|ABC|B|BC|C)$`)

// ValidSet reports whether set is a Rule of Two set: a non-empty, uppercase
// subset of ABC, in that order.
func ValidSet(set string) bool { return setRe.MatchString(set) }

// ErrNoSessionID and ErrNoLeaseID are the two refusals, with the words the
// sidecar has always logged for them.
var (
	// ErrNoSessionID: without SESSION_ID the sidecar cannot say whose it is.
	ErrNoSessionID = errors.New("SESSION_ID is required")
	// ErrNoLeaseID: LEASE_ID authenticates every call to core. It is minted per
	// warm by sessions.ReserveSlot and cleared at teardown, so it identifies this
	// pod rather than just this session — CoreService rejects a stale one.
	ErrNoLeaseID = errors.New("LEASE_ID is required: without it every CoreService call is rejected")
)

// Config is what the sidecar runs with. Each field names its variable; a blank
// variable is an unset one.
type Config struct {
	// LogLevel is one of debug/info/warn/error, case-insensitive (LOG_LEVEL,
	// default "info"). Kept raw: cmd/sidecar resolves it, and logs the raw value
	// when it falls back.
	LogLevel string
	// SessionID is the session this pod serves (SESSION_ID, required),
	// stamped on every log line.
	SessionID string
	// LeaseID is this pod's lease on that session (LEASE_ID, required), sent
	// with every call to core.
	LeaseID string
	// CoreGRPCAddr is where the one outbound connection goes (CORE_GRPC_ADDR).
	CoreGRPCAddr string
	// WorktreePath is the session's checkout, whose diff telemetry pushes
	// (WORKTREE_PATH).
	WorktreePath string
	// MCPPort, LocalAPIPort and HealthPort are the three listeners' ports
	// (MCP_PORT, LOCAL_API_PORT, HEALTH_PORT). Strings, as the listeners want
	// them: a value that is no port fails at Listen.
	MCPPort      string
	LocalAPIPort string
	HealthPort   string
	// VaultProxy is the pod's Agent Vault proxy URL with its session token as
	// userinfo (FLEET_VAULT_PROXY, docs/adr/0075 §7). Set on every pod
	// (repo and definition); empty starts no egress router. Read once at start
	// from FLEET_VAULT_PROXY_FILE, the pod's credential Secret. Never logged.
	VaultProxy string
	// VaultHosts is the comma-separated host list the router sends to the vault
	// (FLEET_VAULT_HOSTS), parsed by egress.New.
	VaultHosts string
	// EgressPort is the loopback port of the router (EGRESS_PORT), used only
	// when VaultProxy is set.
	EgressPort string
	// TracesEndpoint is the OTLP/HTTP traces URL, /v1/traces included
	// (OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, read raw). Empty exports nothing;
	// a malformed one refuses the start, in cmd/sidecar where telemetry.New
	// checks it.
	TracesEndpoint string
	// TrustSet is the session's Rule of Two set, e.g. "AB" or "BC"
	// (FLEET_TRUST_SET, ADR-0086 §2). Required, and valid per ValidSet, when
	// FLEET_AGENT_NAME or FLEET_TRUST_SET is set: the sidecar refuses to start
	// otherwise, so a malformed set never turns quarantine off.
	TrustSet string
	// AgentName is the long-lived agent whose memory this pod writes
	// (FLEET_AGENT_NAME). Empty registers no memory tool.
	AgentName string
	// MemoryRoot is the directory holding agent-<name>-memory and
	// agent-<name>-quarantine (FLEET_MEMORY_ROOT, default /memory).
	MemoryRoot string
	// ClusterRead is the definition's cluster.read grant (FLEET_CLUSTER_READ=1): the read-only kube token is
	// mounted here, so kubectl_read may be served (ADR-0084 §6). ClusterLogNamespaces
	// (FLEET_CLUSTER_LOG_NAMESPACES, comma-separated) are the only namespaces it may read pod logs in.
	ClusterRead          bool
	ClusterLogNamespaces string
	// KubeAPIHost and KubeAPIPort are the kubelet-injected KUBERNETES_SERVICE_HOST/PORT: the API server's ClusterIP,
	// which kubectl_read dials because the egress lock's DNS allowlist refuses kubernetes.default.svc (#434).
	KubeAPIHost, KubeAPIPort string
	// GatewayKeyFile is the credential file holding the llm-gateway key
	// (FLEET_GATEWAY_KEY_FILE, docs/adr/0088). Read per request, so a rotation
	// needs no restart. Missing or empty answers 503 on the gateway route.
	GatewayKeyFile string
	// GatewayURL is the llm-gateway the loopback route proxies to
	// (FLEET_GATEWAY_URL) and GatewayPort the route's loopback port.
	GatewayURL, GatewayPort string
}

// FromProcess is the sidecar's configuration from its environment: the one call
// that reads it.
func FromProcess() (Config, error) { return Load(os.Getenv) }

// Load is the sidecar's configuration from getenv (FromProcess passes
// os.Getenv, a test a map). The error joins every refusal that applies, in the
// order the sidecar has always checked them: ErrNoSessionID, then ErrNoLeaseID.
// A problem added to Load later must also be acted on in cmd/sidecar: main
// refuses on any error, after those two keep their old places and words.
// The Config is returned with it, so a caller can log with what was read.
func Load(getenv func(string) string) (Config, error) {
	env := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	var fileErrs []error
	// docs/adr/0086 §6: a credential is the file KEY_FILE names; the bare key
	// is the fallback for a local run. An unreadable file is a refusal.
	secret := func(key string) string {
		path := getenv(key + "_FILE")
		if path == "" {
			return getenv(key)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			fileErrs = append(fileErrs, fmt.Errorf("%s_FILE: %w", key, err))
		}
		return strings.TrimSpace(string(b))
	}
	c := Config{
		LogLevel:             env("LOG_LEVEL", "info"),
		SessionID:            getenv("SESSION_ID"),
		LeaseID:              secret("LEASE_ID"),
		CoreGRPCAddr:         env("CORE_GRPC_ADDR", "agent-fleet-core.agent-fleet.svc.cluster.local:9090"),
		WorktreePath:         env("WORKTREE_PATH", "/workspace"),
		MCPPort:              env("MCP_PORT", "9090"),
		LocalAPIPort:         env("LOCAL_API_PORT", "9091"),
		HealthPort:           env("HEALTH_PORT", "9092"),
		VaultProxy:           secret("FLEET_VAULT_PROXY"),
		VaultHosts:           getenv("FLEET_VAULT_HOSTS"),
		EgressPort:           env("EGRESS_PORT", "9093"),
		TracesEndpoint:       getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		TrustSet:             getenv("FLEET_TRUST_SET"),
		AgentName:            getenv("FLEET_AGENT_NAME"),
		MemoryRoot:           env("FLEET_MEMORY_ROOT", "/memory"),
		ClusterRead:          getenv("FLEET_CLUSTER_READ") == "1",
		ClusterLogNamespaces: getenv("FLEET_CLUSTER_LOG_NAMESPACES"),
		KubeAPIHost:          getenv("KUBERNETES_SERVICE_HOST"),
		KubeAPIPort:          getenv("KUBERNETES_SERVICE_PORT"),
		GatewayKeyFile:       getenv("FLEET_GATEWAY_KEY_FILE"),
		GatewayURL:           env("FLEET_GATEWAY_URL", "http://llm-gateway.llm-gateway.svc.cluster.local:8080"),
		GatewayPort:          env("GATEWAY_PORT", "9094"),
	}

	var errs []error
	if c.SessionID == "" {
		errs = append(errs, ErrNoSessionID)
	}
	if c.LeaseID == "" {
		errs = append(errs, ErrNoLeaseID)
	}
	if (c.AgentName != "" || c.TrustSet != "") && !ValidSet(c.TrustSet) {
		errs = append(errs, fmt.Errorf("FLEET_TRUST_SET %q is not a set like AB or BC", c.TrustSet))
	}
	return c, errors.Join(append(errs, fileErrs...)...)
}
