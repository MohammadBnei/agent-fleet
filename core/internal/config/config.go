// Package config parses core's environment. Same AGENTFLEET_DB_*
// convention worker/bot already use (see db.ts in both packages).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/bridgeclient"
)

type Config struct {
	Port     string
	GRPCPort string
	// MetricsPort carries /metrics on its OWN listener, deliberately not on
	// ClientPort serves AgentService to registered clients only, behind a
	// bearer (docs/adr/0066 §3). Never routed by any ingress.
	ClientPort string
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
	ProvisionerToken string
	// CoreProvisionerToken is the reverse direction: core's bearer to
	// ProvisionerService (docs/adr/0069). A separate key so each direction
	// rotates on its own and a leaked header exposes one direction only. It is
	// not isolation: both pods sync the whole scope, so each holds both keys.
	CoreProvisionerToken string
	ThotDiscordChannel   string
	ThotRepo             string
	LokiURL              string
	// PrometheusURL backs the dashboard's Observability page. The service
	// name is the kube-prometheus-stack release's, which the chart mangles
	// to 'kube-p' — not a typo. Confirmed live from infra-bootstrap's
	// gitops/platform/values/prometheus/values.yaml.
	PrometheusURL       string
	ProvisionerGRPCAddr string

	// STTAddr / STTToken reach ukubi-stt, the GPU speech-to-text service
	// (infra-bootstrap ADR-0044). Both empty disables dictation, which is a
	// supported state: that service is single-replica on a node that reboots
	// for gaming, so the console must work without it.
	STTAddr  string
	STTToken string
	// MaxInFlight caps fleet-wide concurrent WARM PODS (docs/adr/0019: ~5,
	// the actual human-followable ceiling, not a technical limit).
	//
	// It is no longer a queue depth — nothing queues. It is enforced in
	// sessions.ReserveSlot against CountLivePods, under an advisory lock,
	// and a session that cannot get a slot is refused outright rather than
	// left pending.
	MaxInFlight int
	// MaxDefinitionSessions is the defined-agents pool INSIDE MaxInFlight
	// (docs/adr/0062). 0 (the default) means no definition session can warm;
	// Load refuses to start unless it is >= 0 and < MaxInFlight.
	MaxDefinitionSessions int
	// MaxExternalSessions is the member and client pool INSIDE
	// MaxDefinitionSessions, MaxSessionsPerMember one member's share of it
	// (docs/adr/0066 §8). 0 (the default) means no member or client session
	// can warm.
	MaxExternalSessions  int
	MaxSessionsPerMember int
	// RepoOverridesPath is FLEET_REPO_OVERRIDES, infra-bootstrap's
	// fleet-repo-overrides file as mounted (docs/adr/0074). Empty turns the
	// GitHub repos sync off, and repos keeps whatever rows it has (kind).
	RepoOverridesPath string

	// Bridge is core's event-bridge client (docs/adr/0078 §1): BRIDGE_URL,
	// BRIDGE_TOKEN_URL, BRIDGE_AUTH_HOST, BRIDGE_CLIENT_ID, BRIDGE_USERNAME and
	// BRIDGE_PASSWORD_FILE. Unset URL or no usable password file: core runs
	// without the bridge.
	Bridge bridgeclient.Config
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
	// default 10 days. Set in k8s/core.yaml so the window is a gitops edit
	// rather than a release — the deployed value, not this one, is what the
	// fleet actually runs on.
	//
	// The disk pressure that shortened this to 3 days is real and has not
	// gone away: session volumes are `local-path` (docs/adr/0048 §4), a
	// hostPath on the node's OS disk where the PVC size request is advisory
	// and unenforced, and the two worker nodes have ~85 and ~50 GiB
	// allocatable. Enough un-swept sessions is a full node disk, which breaks
	// kubelet rather than just the fleet.
	//
	// The default is nonetheless the LONG end of the range, because the two
	// failure directions are not symmetric. Too long costs disk on a node,
	// which is visible in `df` and recoverable by lowering this value. Too
	// short deletes a working tree that has no backup of any kind. So the
	// value that applies when nobody has said otherwise — a fresh local
	// stack — errs toward keeping the disk (a value that is set and does not
	// parse is not this case: Load refuses it), and the tighter production number is stated explicitly in
	// k8s/core.yaml where somebody chose it against real capacity.
	//
	// None of this is a claim about how long work takes: git is the durable
	// copy, and the row and its transcript survive the sweep either way.
	SessionRetention time.Duration
	// GarageS3Endpoint must be externally reachable, not the in-cluster
	// garage.bnei.lan host — filestore.PresignUpload/PresignDownload sign
	// the endpoint into the URL (SigV4), and the dashboard's browser can't
	// resolve a .lan hostname (docs/adr/0030).
	GarageS3Endpoint     string
	GarageFilesBucket    string
	GarageFilesAccessKey string
	GarageFilesSecret    string

	// Version is the release this binary runs as (FLEET_VERSION, set by deploy on the Deployment); "" = dev.
	Version string
	// ClaudeModel is the operator's model override for a repo session whose creator named none (CLAUDE_MODEL,
	// docs/adr/0077); "" stores NULL and Claude Code picks.
	ClaudeModel string
	// TokenEnv reads the variable a registered client's tokenRef names (docs/adr/0066 §3): the names come from
	// the synced definitions, so they are read by name at match time, through the same getenv.
	TokenEnv func(string) string
}

// FromProcess is core's configuration from its environment: the one call that reads it. An error is a refusal to
// start (cmd/core/main.go logs it and exits 1): see Load.
func FromProcess() (Config, error) { return Load(os.Getenv) }

// Load is core's configuration from getenv (mountain plan/15 §5: one Config per binary, the environment read only
// here; FromProcess passes os.Getenv, a test a map), validated once: a variable that is SET and does not satisfy its
// rule refuses the start, and every such problem is reported in the one error, each naming its variable and the
// rule. Unset (or empty: getenv cannot tell the two apart) is the default, as before. The Config returned with an
// error is the zero value, so nothing can run on a half-read one.
func Load(getenv func(string) string) (Config, error) {
	l := &loader{getenv: getenv}
	c := Config{
		Version:               getenv("FLEET_VERSION"),
		ClaudeModel:           getenv("CLAUDE_MODEL"),
		TokenEnv:              getenv,
		Port:                  l.port("CORE_PORT", "8080"),
		GRPCPort:              l.port("CORE_GRPC_PORT", "9090"),
		MetricsPort:           l.port("CORE_METRICS_PORT", "9093"),
		ClientPort:            l.port("CORE_CLIENT_PORT", "9091"),
		LogLevel:              l.level("LOG_LEVEL", "info"),
		DBHost:                l.str("AGENTFLEET_DB_HOST", "postgres.bnei.lan"),
		DBPort:                l.dbPort("AGENTFLEET_DB_PORT", 5432),
		DBName:                l.str("AGENTFLEET_DB_NAME", "agentfleetdb"),
		DBUser:                l.str("AGENTFLEET_DB_USER", "dbuser_agentfleet"),
		DBPassword:            getenv("AGENTFLEET_DB_PASSWORD"),
		DiscordBotToken:       getenv("DISCORD_BOT_TOKEN"),
		DiscordTriggerChannel: getenv("DISCORD_TRIGGER_CHANNEL_ID"),
		AlertWebhookToken:     getenv("ALERT_WEBHOOK_TOKEN"),
		ProvisionerToken:      getenv("FLEET_PROVISIONER_TOKEN"),
		CoreProvisionerToken:  getenv("FLEET_CORE_PROVISIONER_TOKEN"),
		ThotDiscordChannel:    getenv("THOT_DISCORD_CHANNEL_ID"),
		ThotRepo:              l.str("THOT_REPO", "infra-bootstrap"),
		LokiURL:               l.url("LOKI_URL", "http://platform-loki.monitoring.svc.cluster.local:3100"),
		PrometheusURL:         l.url("PROMETHEUS_URL", "http://platform-prometheus-kube-p-prometheus.monitoring.svc.cluster.local:9090"),
		ProvisionerGRPCAddr:   l.hostPort("PROVISIONER_GRPC_ADDR", "provisioner.agent-fleet.svc.cluster.local:9090"),
		STTAddr:               l.sttAddr("STT_ADDR", "STT_TOKEN_FLEET", "ukubi-stt.ukubi-stt.svc.cluster.local:9090"),
		STTToken:              getenv("STT_TOKEN_FLEET"),
		RepoOverridesPath:     getenv("FLEET_REPO_OVERRIDES"),
		StopGrace:             l.millis("STOP_GRACE_MS", 30000),
		IdleTimeout:           l.millis("IDLE_TIMEOUT_MS", 30*60*1000),
		StartupStall:          l.millis("STARTUP_STALL_MS", 3*60*1000),
		TurnStall:             l.millis("TURN_STALL_MS", 90*1000),
		SessionRetention:      l.retention("SESSION_RETENTION", 10*24*time.Hour),
		DashboardPublicURL:    l.url("DASHBOARD_PUBLIC_URL", ""),
		OIDCIssuerURL:         l.url("OIDC_ISSUER_URL", ""),
		OIDCClientID:          getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret:      getenv("OIDC_CLIENT_SECRET"),
		SessionKeys:           getenv("FLEET_SESSION_KEYS"),
		AuthDisabled:          l.authDisabled("FLEET_AUTH_DISABLED"),
		GarageS3Endpoint:      l.url("GARAGE_S3_ENDPOINT", "https://s3.bnei.dev"),
		GarageFilesBucket:     l.str("GARAGE_FILES_BUCKET", "agent-fleet-files"),
		GarageFilesAccessKey:  getenv("AGENTFLEET_FILES_S3_ACCESS_KEY"),
		GarageFilesSecret:     getenv("AGENTFLEET_FILES_S3_SECRET"),
		Bridge: bridgeclient.Config{
			URL:          l.url("BRIDGE_URL", ""),
			TokenURL:     l.url("BRIDGE_TOKEN_URL", ""),
			AuthHost:     l.host("BRIDGE_AUTH_HOST"),
			ClientID:     l.str("BRIDGE_CLIENT_ID", "ukubi-bridge-api"),
			Username:     l.str("BRIDGE_USERNAME", "bridge-core"),
			PasswordFile: l.str("BRIDGE_PASSWORD_FILE", "/etc/bridge-core/app_password"),
		},
	}
	// The three pools are one rule, checked once all three read (a value that did not parse has no relation to check).
	var capOK bool
	c.MaxInFlight, capOK = l.count("MAX_IN_FLIGHT_TASKS", 5)
	var ok2, ok3, ok4 bool
	c.MaxDefinitionSessions, ok2 = l.count("MAX_DEFINITION_SESSIONS", 0)
	c.MaxExternalSessions, ok3 = l.count("MAX_EXTERNAL_SESSIONS", 0)
	c.MaxSessionsPerMember, ok4 = l.count("MAX_SESSIONS_PER_MEMBER", 1)
	if capOK && ok2 && ok3 && ok4 {
		if err := checkCapacity(c.MaxInFlight, c.MaxDefinitionSessions, c.MaxExternalSessions, c.MaxSessionsPerMember); err != nil {
			l.errs = append(l.errs, err)
		}
	}
	if len(l.errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration, %d problem(s):\n%w", len(l.errs), errors.Join(l.errs...))
	}
	return c, nil
}

// checkCapacity: the definition pool sits INSIDE the global cap so admins are
// never locked out by defined agents (docs/adr/0062).
func checkCapacity(maxInFlight, maxDef, maxExternal, perMember int) error {
	if maxDef < 0 || maxDef >= maxInFlight {
		return fmt.Errorf("MAX_DEFINITION_SESSIONS (%d) must be >= 0 and < MAX_IN_FLIGHT_TASKS (%d): definition sessions draw from a pool inside the global cap so admins are never locked out", maxDef, maxInFlight)
	}
	// docs/adr/0066 §8: the same rule one level down. With no definition
	// pool, 0 is the only value.
	if maxExternal < 0 || (maxExternal > 0 && maxExternal >= maxDef) {
		return fmt.Errorf("MAX_EXTERNAL_SESSIONS (%d) must be >= 0 and < MAX_DEFINITION_SESSIONS (%d): member and client sessions draw from a pool inside the definition pool", maxExternal, maxDef)
	}
	if maxExternal > 0 && perMember < 1 {
		return fmt.Errorf("MAX_SESSIONS_PER_MEMBER (%d) must be >= 1 when MAX_EXTERNAL_SESSIONS is set", perMember)
	}
	return nil
}

// loader reads typed values and collects what is wrong with them. Every reader returns its default when the
// variable is unset or empty, and on a bad value records the problem and returns the default too (the Config built
// from it is discarded).
type loader struct {
	getenv func(string) string
	errs   []error
}

func (l *loader) bad(key, v, rule string) {
	l.errs = append(l.errs, fmt.Errorf("%s=%s: %s", key, shown(v), rule))
}

// shown quotes a value for the message, except one with userinfo in it (a URL's user:password@), which is never
// echoed: a refusal is logged.
func shown(v string) string {
	if strings.Contains(v, "@") {
		return "<redacted: contains @>"
	}
	return strconv.Quote(v)
}

func (l *loader) str(key, fallback string) string {
	if v := l.getenv(key); v != "" {
		return v
	}
	return fallback
}

// count is an integer with no range of its own (the pools' rule is checkCapacity's); ok is false for a refusal.
func (l *loader) count(key string, fallback int) (n int, ok bool) {
	v := l.getenv(key)
	if v == "" {
		return fallback, true
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.bad(key, v, "must be a whole number")
		return fallback, false
	}
	return n, true
}

func (l *loader) dbPort(key string, fallback int) int {
	v := l.getenv(key)
	if v == "" {
		return fallback
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 65535 {
		return n
	}
	l.bad(key, v, "must be a port number, a whole number from 1 to 65535")
	return fallback
}

// port is a listen port, kept as the string the listeners append to ":".
func (l *loader) port(key, fallback string) string {
	v := l.getenv(key)
	if v == "" {
		return fallback
	}
	if n, err := strconv.ParseUint(v, 10, 16); err != nil || n == 0 {
		l.bad(key, v, "must be a port number, digits only, from 1 to 65535")
		return fallback
	}
	return v
}

func (l *loader) level(key, fallback string) string {
	v := l.getenv(key)
	if v == "" {
		return fallback
	}
	var lv slog.Level
	// slog also reads "info+2" and "WARN-4"; the rule here is the four names, so an offset is refused.
	if err := lv.UnmarshalText([]byte(v)); err != nil || strings.ContainsAny(v, "+-") {
		l.bad(key, v, "must be debug, info, warn or error (case-insensitive, no offset)")
		return fallback
	}
	return v
}

// authDisabled is the explicit local-stack opt-out: only "1" turns the gate off. Anything else that is set and is
// not "0" is refused rather than read as "on", so a "true" that someone believes disables it (or a typo they believe
// leaves it enabled) cannot pass for a decision.
func (l *loader) authDisabled(key string) bool {
	switch v := l.getenv(key); v {
	case "", "0":
		return false
	case "1":
		return true
	default:
		l.bad(key, v, "must be 1 (the console is unauthenticated: local stacks only), or 0 or unset")
		return false
	}
}

// url is an absolute http(s) URL with a host. Nothing else is parsed out of it here: each consumer takes the string.
func (l *loader) url(key, fallback string) string {
	v := l.getenv(key)
	if v == "" {
		return fallback
	}
	if u, err := url.Parse(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		l.bad(key, v, "must be an absolute http or https URL with a host")
		return fallback
	}
	return v
}

// hostPort is a gRPC dial target as deployed: host:port.
func (l *loader) hostPort(key, fallback string) string {
	v := l.getenv(key)
	if v == "" {
		return fallback
	}
	h, p, err := net.SplitHostPort(v)
	if n, perr := strconv.ParseUint(p, 10, 16); err != nil || h == "" || strings.ContainsAny(h, " \t/") || perr != nil || n == 0 {
		l.bad(key, v, "must be host:port, a host and a port from 1 to 65535")
		return fallback
	}
	return v
}

// sttAddr is hostPort, but only while dictation is on. With no STT_TOKEN_FLEET run() never dials the address
// (a supported state: dictation off), so a stale malformed value there is carried as given and not refused.
func (l *loader) sttAddr(key, tokenKey, fallback string) string {
	if l.getenv(tokenKey) == "" {
		if v := l.getenv(key); v != "" {
			return v
		}
		return fallback
	}
	return l.hostPort(key, fallback)
}

// host is a bare host or host:port (BRIDGE_AUTH_HOST becomes a Host header), no scheme and no path. Unset is "".
func (l *loader) host(key string) string {
	v := l.getenv(key)
	if v == "" {
		return ""
	}
	u, err := url.Parse("//" + v)
	if err != nil || u.Host != v || u.Hostname() == "" || u.Path != "" || !validPort(u.Port()) {
		l.bad(key, v, "must be a host or host:port (a port from 1 to 65535), with no scheme and no path")
		return ""
	}
	return v
}

// validPort: empty (no port given) or digits from 1 to 65535.
func validPort(p string) bool {
	if p == "" {
		return true
	}
	n, err := strconv.ParseUint(p, 10, 16)
	return err == nil && n != 0
}

// millis is a whole number of milliseconds, positive. Zero and negative are refused for the reason retention's are
// (see retention): these feed the same Postgres interval arithmetic and the stop and stall timers, where "now" or a
// cutoff in the future matches every session.
func (l *loader) millis(key string, fallbackMS int) time.Duration {
	v := l.getenv(key)
	if v == "" {
		return time.Duration(fallbackMS) * time.Millisecond
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || int64(n) > math.MaxInt64/int64(time.Millisecond) {
		l.bad(key, v, "must be a positive whole number of milliseconds")
		return time.Duration(fallbackMS) * time.Millisecond
	}
	return time.Duration(n) * time.Millisecond
}

// retention reads a Go duration string, extended with the `d` (day) unit
// that time.ParseDuration lacks: "6d" is 144h. Retention is argued in days by
// everyone who touches it, so it is configured in days — a window nobody can
// read off the value is a window nobody reviews.
//
// Only a bare "<int>d" is special-cased. Everything else goes to
// time.ParseDuration, so "144h" and "90s" work and "6d12h" is refused.
// A value that does not parse stops core from starting (it used to warn and use the
// default): a retention nobody meant is a retention nobody chose, and the refusal
// is in the first log line instead of a warn to grep for.
//
// Zero and negative are REJECTED, not merely odd. These durations become a
// Postgres interval in the retention and idle sweeps (sessions/store.go), and
// `now() - interval '0s'` is now, while a negative interval is a cutoff in the
// FUTURE — either one matches every session that exists. Verified against a
// real Postgres: a row one minute old is swept by both. So "-6d", which
// strconv.Atoi is perfectly happy to read, would reclaim the whole fleet's
// disk on the next 60s tick instead of nothing.
func (l *loader) retention(key string, fallback time.Duration) time.Duration {
	v := l.getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if days, derr := strconv.Atoi(strings.TrimSuffix(v, "d")); derr == nil && strings.HasSuffix(v, "d") {
		d, err = 0, nil // out of range, or not positive: the reject below
		if days > 0 && int64(days) <= math.MaxInt64/int64(24*time.Hour) {
			d = time.Duration(days) * 24 * time.Hour
		}
	}
	if err != nil || d <= 0 {
		l.bad(key, v, `must be a positive duration: a Go duration ("144h", "90s") or whole days ("6d"; "6d12h" is not accepted)`)
		return fallback
	}
	return d
}
