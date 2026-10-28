// Package config is the sidecar's whole configuration: one struct, read from the
// environment once (FromProcess) or from a test's map (Load). Nothing else in
// this module reads the environment (TestEnvBan; mountain plan/15 §5), apart
// from the two child-process pass-throughs that test names.
package config

import (
	"errors"
	"os"
)

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
	// userinfo (FLEET_VAULT_PROXY, docs/adr/0075 §7). Set on repo pods only;
	// empty starts no egress router. Never logged.
	VaultProxy string
	// VaultHosts is the comma-separated host list the router sends to the vault
	// (FLEET_VAULT_HOSTS), parsed by egress.New.
	VaultHosts string
	// EgressPort is the loopback port of the router (EGRESS_PORT), used only
	// when VaultProxy is set.
	EgressPort string
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
	c := Config{
		LogLevel:     env("LOG_LEVEL", "info"),
		SessionID:    getenv("SESSION_ID"),
		LeaseID:      getenv("LEASE_ID"),
		CoreGRPCAddr: env("CORE_GRPC_ADDR", "agent-fleet-core.agent-fleet.svc.cluster.local:9090"),
		WorktreePath: env("WORKTREE_PATH", "/workspace"),
		MCPPort:      env("MCP_PORT", "9090"),
		LocalAPIPort: env("LOCAL_API_PORT", "9091"),
		HealthPort:   env("HEALTH_PORT", "9092"),
		VaultProxy:   getenv("FLEET_VAULT_PROXY"),
		VaultHosts:   getenv("FLEET_VAULT_HOSTS"),
		EgressPort:   env("EGRESS_PORT", "9093"),
	}
	var errs []error
	if c.SessionID == "" {
		errs = append(errs, ErrNoSessionID)
	}
	if c.LeaseID == "" {
		errs = append(errs, ErrNoLeaseID)
	}
	return c, errors.Join(errs...)
}
