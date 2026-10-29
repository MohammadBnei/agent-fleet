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
	// Load warns and turns the pools off (the defaults) unless it is >= 0 and < MaxInFlight.
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
	// DecisionWait is how long an asking side waits on a decision before it
	// hears "pending" and carries on (docs/adr/0082 §5): the worker's gate,
	// told on GetSession, and AskUserQuestion's long poll, which also stays
	// within the sidecar's own timeout. Env: DECISION_WAIT_MS, default 45 s,
	// at most MaxDecisionWait.
	DecisionWait time.Duration
	// DashboardPublicURL is the externally-reachable dashboard base, used to
	// build the deep link in a Discord notification. Empty posts the
	// notification without a link rather than a broken one.
	// Env: DASHBOARD_PUBLIC_URL. Also the OIDC redirect URI base and the accepted
	// origin: a malformed value refuses the start while the console gate is on.
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
	// DevOrigins are browser origins (scheme://host[:port]) that AgentService treats as the console's own while the
	// gate is off, so a local page (the game's dev server, the dashboard's vite proxy) can call it as the dev
	// identity. Only with FLEET_AUTH_DISABLED=1: set otherwise, core refuses to start.
	DevOrigins []string
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
	// stack — errs toward keeping the disk (so does a value that is set and does
	// not parse: Load warns and uses this), and the tighter production number is
	// stated explicitly in k8s/core.yaml where somebody chose it against real
	// capacity.
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
	// Warnings are the problems with optional values (a set value that does not satisfy its rule), each replaced by
	// its default: "KEY=value: rule; using the default". cmd/core/main.go logs each at WARN, whatever LOG_LEVEL is. Nil when
	// there are none.
	Warnings []string
}

// RefusalError is the error Load returns for a refusal. Its text is the one error naming every refused variable;
// Warnings are the optional problems found alongside (the refusal returns no Config, so they would otherwise be lost),
// for the caller to log before it exits.
type RefusalError struct {
	err      error
	Warnings []string
}

func (e *RefusalError) Error() string { return e.err.Error() }
func (e *RefusalError) Unwrap() error { return e.err }

// FromProcess is core's configuration from its environment: the one call that reads it. An error is a refusal to
// start (cmd/core/main.go logs it and exits 1): see Load.
func FromProcess() (Config, error) { return Load(os.Getenv) }

// Load is core's configuration from getenv (mountain plan/15 §5: one Config per binary, the environment read only
// here; FromProcess passes os.Getenv, a test a map), validated once. Unset (or empty: getenv cannot tell the two
// apart) is the default. A value that is SET and does not satisfy its rule is one of two kinds of problem:
//
//   - an optional key (anything with a default, or that may be unset) WARNS: the field takes its default, the problem
//     is one line of Config.Warnings naming the variable and the rule, and core starts;
//   - a required key (core cannot run without it) REFUSES: every such problem is one line of the one error. The only
//     required keys Load checks are OIDC_ISSUER_URL and DASHBOARD_PUBLIC_URL (the OIDC discovery target and the
//     redirect URI base and accepted origin), while the console gate is on (FLEET_AUTH_DISABLED is not 1); that
//     the issuer, DASHBOARD_PUBLIC_URL, OIDC_CLIENT_ID, OIDC_CLIENT_SECRET and FLEET_SESSION_KEYS are present is
//     run()'s check (cmd/core requireOIDC and auth.New), the same gate-on condition (a Load of an empty environment
//     must succeed, and a missing value is not a problem here).
//
// A refusal's error is a *RefusalError, which also carries the warnings found. The BRIDGE_* variables are one
// setting: if any of BRIDGE_URL, BRIDGE_TOKEN_URL and BRIDGE_AUTH_HOST is set and invalid, each problem is a warning
// and the bridge is off (Bridge.URL is blank).
//
// The Config returned with an error is the zero value, so nothing can run on a half-read one.
// MaxDecisionWait is DECISION_WAIT_MS's ceiling (docs/adr/0082 §5: up to 10 min).
const MaxDecisionWait = 10 * time.Minute

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
		DecisionWait:          l.millis("DECISION_WAIT_MS", 45*1000),
		SessionRetention:      l.retention("SESSION_RETENTION", 10*24*time.Hour),
		OIDCClientID:          getenv("OIDC_CLIENT_ID"),
		OIDCClientSecret:      getenv("OIDC_CLIENT_SECRET"),
		SessionKeys:           getenv("FLEET_SESSION_KEYS"),
		AuthDisabled:          l.authDisabled("FLEET_AUTH_DISABLED"),
		GarageS3Endpoint:      l.url("GARAGE_S3_ENDPOINT", "https://s3.bnei.dev"),
		GarageFilesBucket:     l.str("GARAGE_FILES_BUCKET", "agent-fleet-files"),
		GarageFilesAccessKey:  getenv("AGENTFLEET_FILES_S3_ACCESS_KEY"),
		GarageFilesSecret:     getenv("AGENTFLEET_FILES_S3_SECRET"),
	}
	// The bridge is one setting: URL, TokenURL and AuthHost go together, and a client with the URL and without a token
	// endpoint or auth host fails every call. So any of the three set and invalid warns and blanks URL, which is the
	// bridge's own not-configured path (client.go: an empty URL runs core without it).
	nb := len(l.warns)
	c.Bridge = bridgeclient.Config{
		URL:          l.url("BRIDGE_URL", ""),
		TokenURL:     l.url("BRIDGE_TOKEN_URL", ""),
		AuthHost:     l.host("BRIDGE_AUTH_HOST"),
		ClientID:     l.str("BRIDGE_CLIENT_ID", "ukubi-bridge-api"),
		Username:     l.str("BRIDGE_USERNAME", "bridge-core"),
		PasswordFile: l.str("BRIDGE_PASSWORD_FILE", "/etc/bridge-core/app_password"),
	}
	if len(l.warns) > nb {
		c.Bridge.URL = ""
		for i := nb; i < len(l.warns); i++ {
			l.warns[i] += "; the bridge is off"
		}
	}
	// The issuer and the dashboard's public URL are required while the gate is on (run() builds the OIDC relying
	// party from them: discovery target, redirect URI base); a malformed one makes the console unusable for every
	// human. With the gate explicitly off nothing reads them, so a malformed one is only a warning.
	c.DevOrigins = l.devOrigins("FLEET_DEV_ORIGINS", c.AuthDisabled)
	c.OIDCIssuerURL = l.urlWith("OIDC_ISSUER_URL", "", l.requiredIf(!c.AuthDisabled))
	c.DashboardPublicURL = l.urlWith("DASHBOARD_PUBLIC_URL", "", l.requiredIf(!c.AuthDisabled))
	// The three pools are one rule, checked on the values in use (a count that did not parse is its default).
	c.MaxInFlight = l.count("MAX_IN_FLIGHT_TASKS", 5)
	c.MaxDefinitionSessions = l.count("MAX_DEFINITION_SESSIONS", 0)
	c.MaxExternalSessions = l.count("MAX_EXTERNAL_SESSIONS", 0)
	c.MaxSessionsPerMember = l.count("MAX_SESSIONS_PER_MEMBER", 1)
	if err := checkCapacity(c.MaxInFlight, c.MaxDefinitionSessions, c.MaxExternalSessions, c.MaxSessionsPerMember); err != nil {
		// The relation is the pools' own rule, not one value's: a broken one turns the inner pools off (the defaults)
		// rather than keep a cap the numbers contradict, and says why.
		l.warns = append(l.warns, err.Error()+"; using the defaults (5, 0, 0, 1): the definition, member and client pools are off")
		c.MaxInFlight, c.MaxDefinitionSessions, c.MaxExternalSessions, c.MaxSessionsPerMember = 5, 0, 0, 1
	}
	if c.DecisionWait > MaxDecisionWait {
		l.warns = append(l.warns, fmt.Sprintf("DECISION_WAIT_MS=%d: must be at most %d; using %[2]d",
			c.DecisionWait.Milliseconds(), MaxDecisionWait.Milliseconds()))
		c.DecisionWait = MaxDecisionWait
	}
	if len(l.errs) > 0 {
		return Config{}, &RefusalError{
			err:      fmt.Errorf("invalid configuration, %d problem(s):\n%w", len(l.errs), errors.Join(l.errs...)),
			Warnings: l.warns,
		}
	}
	c.Warnings = l.warns
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
// variable is unset or empty, and on a bad value records the problem and returns the default too: as a warning (the
// default is what the Config keeps) unless the key is required, where it is an error and the Config is discarded.
type loader struct {
	getenv func(string) string
	errs   []error
	warns  []string
}

// report records a set value that does not satisfy its rule.
type report func(key, v, rule string)

// warn is an optional key's problem: the default is used instead.
func (l *loader) warn(key, v, rule string) {
	l.warns = append(l.warns, fmt.Sprintf("%s=%s: %s; using the default", key, shown(v), rule))
}

// refuse is a required key's problem: core does not start.
func (l *loader) refuse(key, v, rule string) {
	l.errs = append(l.errs, fmt.Errorf("%s=%s: %s", key, shown(v), rule))
}

// requiredIf is refuse when required, else warn.
func (l *loader) requiredIf(required bool) report {
	if required {
		return l.refuse
	}
	return l.warn
}

// shown quotes a value for the message, except one with userinfo in it (a URL's user:password@), which is never
// echoed: a refusal and a warning are both logged.
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

// count is an integer with no range of its own (the pools' rule is checkCapacity's, on the values in use).
func (l *loader) count(key string, fallback int) int {
	v := l.getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.warn(key, v, "must be a whole number")
		return fallback
	}
	return n
}

func (l *loader) dbPort(key string, fallback int) int {
	v := l.getenv(key)
	if v == "" {
		return fallback
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 65535 {
		return n
	}
	l.warn(key, v, "must be a port number, a whole number from 1 to 65535")
	return fallback
}

// port is a listen port, kept as the string the listeners append to ":".
func (l *loader) port(key, fallback string) string {
	v := l.getenv(key)
	if v == "" {
		return fallback
	}
	if n, err := strconv.ParseUint(v, 10, 16); err != nil || n == 0 {
		l.warn(key, v, "must be a port number, digits only, from 1 to 65535")
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
	// slog also reads "info+2" and "WARN-4"; the rule here is the four names, so an offset is a bad value.
	if err := lv.UnmarshalText([]byte(v)); err != nil || strings.ContainsAny(v, "+-") {
		l.warn(key, v, "must be debug, info, warn or error (case-insensitive, no offset)")
		return fallback
	}
	return v
}

// authDisabled is the explicit local-stack opt-out: only "1" turns the gate off. Anything else that is set and is
// not "0" is a bad value, never read as "on": it warns and the gate stays on, and a "true" that someone believes disables it (or a typo they believe leaves it enabled) cannot pass for a decision.
func (l *loader) authDisabled(key string) bool {
	switch v := l.getenv(key); v {
	case "", "0":
		return false
	case "1":
		return true
	default:
		l.warn(key, v, "must be 1 (the console is unauthenticated: local stacks only), or 0 or unset")
		return false
	}
}

// devOrigins is a comma-separated list of http(s) origins, honoured only with the gate off. Any problem refuses: a
// dev origin set on a gated core is a mistake about which core this is, and a malformed one would silently admit
// nothing.
func (l *loader) devOrigins(key string, authDisabled bool) []string {
	v := l.getenv(key)
	if v == "" {
		return nil
	}
	if !authDisabled {
		l.refuse(key, v, "must be unset while the console gate is on (dev origins call AgentService as the dev identity)")
		return nil
	}
	var out []string
	for _, o := range strings.Split(v, ",") {
		o = strings.TrimSpace(o)
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			l.refuse(key, v, "must be comma-separated origins, each scheme://host[:port]")
			return nil
		}
		out = append(out, u.Scheme+"://"+u.Host)
	}
	return out
}

// url is an absolute http(s) URL with a host. Nothing else is parsed out of it here: each consumer takes the string.
func (l *loader) url(key, fallback string) string { return l.urlWith(key, fallback, l.warn) }

// urlWith is url with the problem going to bad: a required URL passes refuse.
func (l *loader) urlWith(key, fallback string, bad report) string {
	v := l.getenv(key)
	if v == "" {
		return fallback
	}
	if u, err := url.Parse(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		bad(key, v, "must be an absolute http or https URL with a host")
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
		l.warn(key, v, "must be host:port, a host and a port from 1 to 65535")
		return fallback
	}
	return v
}

// sttAddr is hostPort, but only while dictation is on. With no STT_TOKEN_FLEET run() never dials the address
// (a supported state: dictation off), so a stale malformed value there is carried as given and not reported. With a
// token a malformed value is a warning and the default address (STT_ADDR is optional).
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
		l.warn(key, v, "must be a host or host:port (a port from 1 to 65535), with no scheme and no path")
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

// millis is a whole number of milliseconds, positive. Zero and negative are a bad value for the reason retention's are
// (see retention), and the default is used instead: these feed the same Postgres interval arithmetic and the stop
// and stall timers, where "now" or a cutoff in the future matches every session.
func (l *loader) millis(key string, fallbackMS int) time.Duration {
	v := l.getenv(key)
	if v == "" {
		return time.Duration(fallbackMS) * time.Millisecond
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || int64(n) > math.MaxInt64/int64(time.Millisecond) {
		l.warn(key, v, "must be a positive whole number of milliseconds")
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
// time.ParseDuration, so "144h" and "90s" work and "6d12h" is a bad value.
// A value that does not parse warns and uses the default, the long end of the
// range (see Config.SessionRetention for why that end).
//
// Zero and negative are a bad value, not merely odd. These durations become a
// Postgres interval in the retention and idle sweeps (sessions/store.go), and
// `now() - interval '0s'` is now, while a negative interval is a cutoff in the
// FUTURE — either one matches every session that exists. Verified against a
// real Postgres: a row one minute old is swept by both. So "-6d", which
// strconv.Atoi is perfectly happy to read, would reclaim the whole fleet's
// disk on the next 60s tick instead of nothing. (A bad value never reaches a
// sweep: it is replaced by the default.)
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
		l.warn(key, v, `must be a positive duration: a Go duration ("144h", "90s") or whole days ("6d"; "6d12h" is not accepted)`)
		return fallback
	}
	return d
}
