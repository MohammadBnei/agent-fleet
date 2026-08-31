// Package config parses core's environment. Same AGENTFLEET_DB_*
// convention worker/bot already use (see db.ts in both packages).
package config

import (
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port     string
	GRPCPort string
	// MetricsPort carries /metrics on its OWN listener, deliberately not on
	// Port. fleet.bnei.dev's IngressRoute matches on host with no path
	// constraint, so everything Port serves is internet-routed and only the
	// OIDC gate refuses it — see docs/adr/0059.
	MetricsPort string
	// LogLevel is one of debug/info/warn/error (case-insensitive), parsed
	// via slog.Level.UnmarshalText in cmd/core/main.go.
	LogLevel              string
	DBHost                string
	DBPort                int
	DBName                string
	DBUser                string
	DBPassword            string
	DiscordBotToken       string
	DiscordTriggerChannel string
	// Alert path (docs/adr/0037). An empty token disables the webhook
	// rather than serving it unauthenticated — it creates tasks, and a
	// thot task runs an agent with cluster access.
	AlertWebhookToken string
	// ProvisionerToken authenticates the provisioner to CoreService. It holds
	// no session lease — it exists before any pod does — so it is the one
	// caller that needs a shared secret rather than a per-session credential.
	// From the same Infisical scope both components already consume whole, so
	// adding the key needs no manifest change on either side.
	ProvisionerToken   string
	ThotDiscordChannel string
	ThotRepo           string
	LokiURL            string
	// PrometheusURL backs the dashboard's Observability page. The service
	// name is the kube-prometheus-stack release's, which the chart mangles
	// to 'kube-p' — not a typo. Confirmed live from infra-bootstrap's
	// gitops/platform/values/prometheus/values.yaml.
	PrometheusURL       string
	ProvisionerGRPCAddr string
	// MaxInFlight caps fleet-wide concurrent WARM PODS (docs/adr/0019: ~5,
	// the actual human-followable ceiling, not a technical limit).
	//
	// It is no longer a queue depth — nothing queues. It is enforced in
	// sessions.ReserveSlot against CountLivePods, under an advisory lock,
	// and a session that cannot get a slot is refused outright rather than
	// left pending.
	MaxInFlight int
	// StopGrace is how long sessions.Loop waits after a Stop request before
	// force-tearing down a worker pod that hasn't gone terminal on its own —
	// long enough for a healthy pod to notice the cooperative abort message
	// and shut down cleanly, short enough that a hung/unreachable pod
	// doesn't outlive a Stop click indefinitely.
	StopGrace time.Duration
	// IdleTimeout is the sessions redesign's idle-timeout backstop (supersedes
	// docs/adr/0021/0025's phase-boundary framing) — how long a warm pod can
	// go with no real transcript activity (tasks.last_active_at, bumped by a
	// transcript.Store decorator on every Append/AppendReply, not the
	// sidecar's own git-diff-gated telemetry or the unconditional heartbeat
	// timer, neither of which prove a human is actually present) before
	// dispatch.Loop tears it down — the same TearDownSession call
	// enforceStopGrace already makes, no status change.
	IdleTimeout time.Duration
	// StartupStall bounds "pod scheduled" -> "first sign of life"
	// (docs/adr/0040). A worker that comes up and never posts anything is
	// torn down after this instead of holding a concurrency slot for the
	// full IdleTimeout plus a heartbeat reclaim (~40 min before this
	// existed). Env: STARTUP_STALL_MS, default 3 min — generous next to a
	// normal pod's few seconds to first message, but a fraction of what a
	// silent pod used to cost.
	StartupStall time.Duration
	// TurnStall is how long a session may owe a response to something a
	// human said before its derived live state reports `stalled`
	// (docs/adr/0040). Purely informational — nothing is torn down on this
	// clock, since a slow turn is not a dead one. Env: TURN_STALL_MS,
	// default 90s.
	TurnStall time.Duration
	// DashboardPublicURL is the externally-reachable dashboard base, used to
	// build the deep link in a Discord notification. Empty posts the
	// notification without a link rather than a broken one.
	// Env: DASHBOARD_PUBLIC_URL.
	DashboardPublicURL string
	// OIDC federates the console's login to authentik (infra-bootstrap
	// ADR-0041). The redirect URI is derived from DashboardPublicURL above and
	// must match the provider's registered redirect_uris exactly — authentik's
	// matching_mode is `strict`.
	//
	// Unlike every other optional secret here, an unset value must REFUSE TO
	// SERVE rather than disable the gate. See run.go: the Infisical operator
	// renders a stale or empty Secret often enough that "feature off when
	// unset" would bring core up wide open and looking healthy.
	OIDCIssuerURL    string
	OIDCClientID     string
	OIDCClientSecret string
	// SessionKeys signs the browser session cookie. Comma-separated: sign with
	// the first, verify against any — the whole rotation mechanism.
	SessionKeys string
	// AuthDisabled is the explicit local-stack opt-out (/dashboard-e2e,
	// /kind-local). Explicit so that "no auth" is always something someone
	// wrote down, never something that happened because a secret failed to
	// render.
	AuthDisabled bool
	// SessionRetention is how long an untouched session keeps its disk — its
	// working directory and per-session SDK state (docs/adr/0048). After
	// this the retention GC reclaims both and marks the session swept:
	// readable history, no longer resumable.
	//
	// Deliberately much longer than IdleTimeout, which only reclaims a POD.
	// Losing a pod costs a warm-up; losing the directory costs whatever was
	// never committed, so the two clocks are not the same kind of decision.
	// Env: SESSION_RETENTION, a duration string with a `d` unit ("6d"),
	// default 3 days. Set in k8s/core.yaml so the window is a gitops edit
	// rather than a release.
	//
	// Was 14 days, shortened once session volumes moved to `local-path`
	// (docs/adr/0048 §4). That is a hostPath directory on the node's OS disk,
	// where the PVC's size request is advisory and unenforced — and the two
	// worker nodes have ~85 and ~50 GiB allocatable. Five concurrent sessions
	// is fine; two weeks of un-swept ones is a full node disk, which breaks
	// kubelet rather than just the fleet.
	//
	// Three days is not a guess about how long work takes: git is the durable
	// copy, and a tree nobody has touched in three days is not work in
	// progress. The row and its transcript survive either way.
	SessionRetention time.Duration
	// GarageS3Endpoint must be externally reachable, not the in-cluster
	// garage.bnei.lan host — filestore.PresignUpload/PresignDownload sign
	// the endpoint into the URL (SigV4), and the dashboard's browser can't
	// resolve a .lan hostname (docs/adr/0030).
	GarageS3Endpoint     string
	GarageFilesBucket    string
	GarageFilesAccessKey string
	GarageFilesSecret    string
}

func Load() Config {
	return Config{
		Port:                  env("CORE_PORT", "8080"),
		GRPCPort:              env("CORE_GRPC_PORT", "9090"),
		MetricsPort:           env("CORE_METRICS_PORT", "9093"),
		LogLevel:              env("LOG_LEVEL", "info"),
		DBHost:                env("AGENTFLEET_DB_HOST", "postgres.bnei.lan"),
		DBPort:                envInt("AGENTFLEET_DB_PORT", 5432),
		DBName:                env("AGENTFLEET_DB_NAME", "agentfleetdb"),
		DBUser:                env("AGENTFLEET_DB_USER", "dbuser_agentfleet"),
		DBPassword:            os.Getenv("AGENTFLEET_DB_PASSWORD"),
		DiscordBotToken:       os.Getenv("DISCORD_BOT_TOKEN"),
		DiscordTriggerChannel: os.Getenv("DISCORD_TRIGGER_CHANNEL_ID"),
		AlertWebhookToken:     os.Getenv("ALERT_WEBHOOK_TOKEN"),
		ProvisionerToken:      os.Getenv("FLEET_PROVISIONER_TOKEN"),
		ThotDiscordChannel:    os.Getenv("THOT_DISCORD_CHANNEL_ID"),
		ThotRepo:              env("THOT_REPO", "infra-bootstrap"),
		LokiURL:               env("LOKI_URL", "http://platform-loki.monitoring.svc.cluster.local:3100"),
		PrometheusURL:         env("PROMETHEUS_URL", "http://platform-prometheus-kube-p-prometheus.monitoring.svc.cluster.local:9090"),
		ProvisionerGRPCAddr:   env("PROVISIONER_GRPC_ADDR", "provisioner.agent-fleet.svc.cluster.local:9090"),
		MaxInFlight:           envInt("MAX_IN_FLIGHT_TASKS", 5),
		StopGrace:             time.Duration(envInt("STOP_GRACE_MS", 30000)) * time.Millisecond,
		IdleTimeout:           time.Duration(envInt("IDLE_TIMEOUT_MS", 30*60*1000)) * time.Millisecond,
		StartupStall:          time.Duration(envInt("STARTUP_STALL_MS", 3*60*1000)) * time.Millisecond,
		TurnStall:             time.Duration(envInt("TURN_STALL_MS", 90*1000)) * time.Millisecond,
		SessionRetention:      envDuration("SESSION_RETENTION", 3*24*time.Hour),
		DashboardPublicURL:    os.Getenv("DASHBOARD_PUBLIC_URL"),
		OIDCIssuerURL:         os.Getenv("OIDC_ISSUER_URL"),
		OIDCClientID:          os.Getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret:      os.Getenv("OIDC_CLIENT_SECRET"),
		SessionKeys:           os.Getenv("FLEET_SESSION_KEYS"),
		AuthDisabled:          os.Getenv("FLEET_AUTH_DISABLED") == "1",
		GarageS3Endpoint:      env("GARAGE_S3_ENDPOINT", "https://s3.bnei.dev"),
		GarageFilesBucket:     env("GARAGE_FILES_BUCKET", "agent-fleet-files"),
		GarageFilesAccessKey:  os.Getenv("AGENTFLEET_FILES_S3_ACCESS_KEY"),
		GarageFilesSecret:     os.Getenv("AGENTFLEET_FILES_S3_SECRET"),
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// envDuration reads a Go duration string, extended with the `d` (day) unit
// that time.ParseDuration lacks: "6d" is 144h. Retention is argued in days by
// everyone who touches it, so it is configured in days — a window nobody can
// read off the value is a window nobody reviews.
//
// Only a bare "<int>d" is special-cased. Everything else goes to
// time.ParseDuration, so "144h" and "90s" work and "6d12h" is a warn plus the
// fallback rather than a silent misparse. An unparseable value never stops
// core from starting: a typo here must not take the console down, and the
// compiled default is always a safe window. That does mean a typo is only
// visible in the log — grep for "invalid duration" after changing one.
//
// Zero and negative are REJECTED, not merely odd. These durations become a
// Postgres interval in the retention and idle sweeps (sessions/store.go), and
// `now() - interval '0s'` is now, while a negative interval is a cutoff in the
// FUTURE — either one matches every session that exists. Verified against a
// real Postgres: a row one minute old is swept by both. So "-6d", which
// strconv.Atoi is perfectly happy to read, would reclaim the whole fleet's
// disk on the next 60s tick instead of nothing. Falling back to the compiled
// default is the only safe reading of a duration nobody meant.
func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if days, derr := strconv.Atoi(strings.TrimSuffix(v, "d")); derr == nil && strings.HasSuffix(v, "d") {
		d, err = time.Duration(days)*24*time.Hour, nil
		if int64(days) > math.MaxInt64/int64(24*time.Hour) {
			d = 0 // the multiply above wrapped; fall through to the reject
		}
	}
	if err != nil || d <= 0 {
		slog.Warn("invalid duration, using default", "key", key, "value", v, "default", fallback)
		return fallback
	}
	return d
}
