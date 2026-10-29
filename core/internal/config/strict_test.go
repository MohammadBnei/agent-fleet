package config

import (
	"errors"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/bridgeclient"
)

// TestLoadDefaults: with nothing set, every field is its default, exactly as before the refusals existed (the
// values here are the ones Load had when it fell back).
func TestLoadDefaults(t *testing.T) {
	want := Config{
		Port: "8080", GRPCPort: "9090", MetricsPort: "9093", ClientPort: "9091", LogLevel: "info",
		DBHost: "postgres.bnei.lan", DBPort: 5432, DBName: "agentfleetdb", DBUser: "dbuser_agentfleet",
		ThotRepo:            "infra-bootstrap",
		LokiURL:             "http://platform-loki.monitoring.svc.cluster.local:3100",
		PrometheusURL:       "http://platform-prometheus-kube-p-prometheus.monitoring.svc.cluster.local:9090",
		ProvisionerGRPCAddr: "provisioner.agent-fleet.svc.cluster.local:9090",
		STTAddr:             "ukubi-stt.ukubi-stt.svc.cluster.local:9090",
		MaxInFlight:         5, MaxDefinitionSessions: 0, MaxExternalSessions: 0, MaxSessionsPerMember: 1,
		StopGrace: 30 * time.Second, IdleTimeout: 30 * time.Minute, StartupStall: 3 * time.Minute, TurnStall: 90 * time.Second,
		DecisionWait:     45 * time.Second,
		SessionRetention: 240 * time.Hour,
		GarageS3Endpoint: "https://s3.bnei.dev", GarageFilesBucket: "agent-fleet-files",
		Bridge: bridgeclient.Config{ClientID: "ukubi-bridge-api", Username: "bridge-core", PasswordFile: "/etc/bridge-core/app_password"},
	}
	// An empty value cannot be told from an unset one (getenv has one result), and a manifest that renders a
	// Secret key empty must not refuse where it did not.
	for name, getenv := range map[string]func(string) string{"unset": env(nil), "empty": func(string) string { return "" }} {
		got, err := Load(getenv)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got.TokenEnv = nil
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: defaults differ:\n got %+v\nwant %+v", name, got, want)
		}
	}
}

// refusals: per variable, values that are bad and values that must load. A bad value of a required key refuses the start
// (required: OIDC_ISSUER_URL and DASHBOARD_PUBLIC_URL, while the console gate is on); a bad value of any other key warns and takes the
// default. Every key Load validates is here (TestRefusalsCoverEveryRule).
var refusals = []struct {
	key  string
	bad  []string
	good []string
}{
	{"CORE_PORT", []string{"abc", "0", "65536", "-1", "+80", "80 ", "0.0.0.0:80"}, []string{"1", "8080", "65535"}},
	{"CORE_GRPC_PORT", []string{"abc", "0", "65536"}, []string{"9090"}},
	{"CORE_METRICS_PORT", []string{"abc", "0", "65536"}, []string{"9093"}},
	{"CORE_CLIENT_PORT", []string{"abc", "0", "65536"}, []string{"9091"}},
	{"LOG_LEVEL", []string{"verbose", "warning", "3x", "info+2", "WARN-4", "debug+0"}, []string{"debug", "DEBUG", "Info", "warn", "error"}},
	{"AGENTFLEET_DB_PORT", []string{"abc", "0", "65536", "-5432", "5432.0", " 5432"}, []string{"5432", "1", "65535"}},
	{"STOP_GRACE_MS", []string{"abc", "0", "-1", "1.5", "30s", "9223372036854775807"}, []string{"1", "30000"}},
	{"IDLE_TIMEOUT_MS", []string{"abc", "0", "-1", "30m"}, []string{"1800000"}},
	{"STARTUP_STALL_MS", []string{"abc", "0", "-1", "3m"}, []string{"180000"}},
	{"TURN_STALL_MS", []string{"abc", "0", "-1", "90s"}, []string{"90000"}},
	{"DECISION_WAIT_MS", []string{"abc", "0", "-1", "45s"}, []string{"45000", "600000"}},
	{"SESSION_RETENTION", []string{"abc", "0", "0d", "-6d", "6d12h", "10", "-213503d", "-106752d", "106752d", "9223372036854775807d"}, []string{"6d", "144h"}},
	{"FLEET_AUTH_DISABLED", []string{"true", "false", "yes", "2", " 1", "on"}, []string{"1", "0"}},
	// Any value refuses on a gated core (this table loads with the gate on); TestLoadDevOrigins has the good ones.
	{"FLEET_DEV_ORIGINS", []string{"http://127.0.0.1:7410", "x"}, nil},
	{"LOKI_URL", []string{"loki:3100", "ftp://loki", "http://", "//loki", "not a url", "http://[::1"}, []string{"http://loki:3100", "https://loki.example/"}},
	{"PROMETHEUS_URL", []string{"prom:9090", "ws://prom", "http:///x"}, []string{"http://prom:9090"}},
	{"DASHBOARD_PUBLIC_URL", []string{"fleet.bnei.dev", "https://", "javascript:alert(1)"}, []string{"https://fleet.bnei.dev", "http://localhost:8080"}},
	{"OIDC_ISSUER_URL", []string{"authentik.bnei.dev/application/o/fleet/", "file:///x"}, []string{"https://authentik.bnei.dev/application/o/fleet/"}},
	{"GARAGE_S3_ENDPOINT", []string{"s3.bnei.dev", "s3://bucket"}, []string{"https://s3.bnei.dev"}},
	{"BRIDGE_URL", []string{"ukubi-bridge:8080", "tcp://x:1"}, []string{"http://ukubi-bridge.ukubi-bridge.svc.cluster.local:8080"}},
	{"BRIDGE_TOKEN_URL", []string{"authentik/application/o/token/", "http://"}, []string{"http://platform-authentik-server.authentik.svc.cluster.local/application/o/token/"}},
	{"BRIDGE_AUTH_HOST", []string{"https://authentik.bnei.dev", "authentik.bnei.dev/", "a b", "user@host", ":443", "authentik.bnei.dev:99999", "authentik.bnei.dev:0", "authentik.bnei.dev:x"}, []string{"authentik.bnei.dev", "authentik.bnei.dev:443"}},
	{"PROVISIONER_GRPC_ADDR", []string{"provisioner", "provisioner:", ":9090", "provisioner:abc", "provisioner:0", "provisioner:70000", "http://provisioner:9090", "a b:9090", "a/b:9090"}, []string{"127.0.0.1:9094", "provisioner.agent-fleet.svc.cluster.local:9090", "[::1]:9090"}},
	{"STT_ADDR", []string{"stt", "stt:x", ":1", "a b:9090"}, []string{"ukubi-stt:9090"}},
}

// defaultsFor is the Config Load builds from base alone: what a warned value must be replaced by (Warnings and
// TokenEnv are cleared, the latter being a func, so the two can be compared).
func defaultsFor(t *testing.T, base map[string]string) Config {
	t.Helper()
	c := mustLoad(t, base)
	c.Warnings, c.TokenEnv = nil, nil
	return c
}

// required is the keys Load refuses on (the rest of refusals warns).
var required = map[string]bool{"OIDC_ISSUER_URL": true, "DASHBOARD_PUBLIC_URL": true, "FLEET_DEV_ORIGINS": true}

// TestLoadRefusals: a bad value of a required key refuses the start; a bad value of any other key loads with one
// warning naming it and the Config of the same environment without it. Every good value loads with no warning (the
// positive control: a warning is the bad value's doing).
func TestLoadRefusals(t *testing.T) {
	base := map[string]string{"STT_TOKEN_FLEET": "t"} // STT_ADDR is read only with a token
	defaults := defaultsFor(t, base)
	for _, r := range refusals {
		for _, v := range r.bad {
			m := map[string]string{r.key: v, "STT_TOKEN_FLEET": "t"}
			c, err := Load(env(m))
			if required[r.key] {
				if err == nil {
					t.Errorf("%s=%q loaded; want a refusal (it is required)", r.key, v)
					continue
				}
				if !strings.Contains(err.Error(), r.key+"=") || !strings.Contains(err.Error(), "must be") {
					t.Errorf("%s=%q: error %q does not name the variable and the rule", r.key, v, err)
				}
				if !reflect.DeepEqual(c, Config{}) {
					t.Errorf("%s=%q: a refusal returned a Config that could be run", r.key, v)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s=%q refused: %v; want a warning and the default (it is optional)", r.key, v, err)
				continue
			}
			if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], r.key+"=") || !strings.Contains(c.Warnings[0], "must be") ||
				!strings.Contains(c.Warnings[0], "; using the default") {
				t.Errorf("%s=%q: warnings %q; want one naming the variable, the rule and the default", r.key, v, c.Warnings)
			}
			c.Warnings, c.TokenEnv = nil, nil
			if !reflect.DeepEqual(c, defaults) {
				t.Errorf("%s=%q: the Config is not the default one:\n got %+v\nwant %+v", r.key, v, c, defaults)
			}
		}
		for _, v := range r.good {
			c, err := Load(env(map[string]string{r.key: v, "STT_TOKEN_FLEET": "t"}))
			if err != nil {
				t.Errorf("%s=%q refused: %v", r.key, v, err)
			} else if c.Warnings != nil {
				t.Errorf("%s=%q warned: %q", r.key, v, c.Warnings)
			}
		}
	}
}

// DECISION_WAIT_MS above its ceiling warns and is the ceiling (docs/adr/0082 §5: up to 10 min).
func TestLoadDecisionWaitClamps(t *testing.T) {
	c := mustLoad(t, map[string]string{"DECISION_WAIT_MS": "600001"})
	if c.DecisionWait != MaxDecisionWait || len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "DECISION_WAIT_MS=600001") {
		t.Errorf("DecisionWait %v, warnings %q; want %v and one warning", c.DecisionWait, c.Warnings, MaxDecisionWait)
	}
}

// The pools: an int that does not parse warns and is its default; a relation that fails on the values in use warns and
// turns all four back to the defaults (5, 0, 0, 1), which switches the inner pools off.
func TestLoadPoolsWarn(t *testing.T) {
	defaults := defaultsFor(t, nil)
	for _, k := range []string{"MAX_IN_FLIGHT_TASKS", "MAX_DEFINITION_SESSIONS", "MAX_EXTERNAL_SESSIONS", "MAX_SESSIONS_PER_MEMBER"} {
		for _, v := range []string{"abc", "1.5", "2x", " 2"} {
			c, err := Load(env(map[string]string{k: v}))
			if err != nil || len(c.Warnings) != 1 || !strings.HasPrefix(c.Warnings[0], k+"=") {
				t.Errorf("%s=%q: %+v, %v; want one warning naming it", k, v, c, err)
				continue
			}
			c.Warnings, c.TokenEnv = nil, nil
			if !reflect.DeepEqual(c, defaults) {
				t.Errorf("%s=%q: the Config is not the default one: %+v", k, v, c)
			}
		}
	}
	ok := map[string]string{"MAX_IN_FLIGHT_TASKS": "5", "MAX_DEFINITION_SESSIONS": "2", "MAX_EXTERNAL_SESSIONS": "1"}
	if c := mustLoad(t, ok); c.MaxInFlight != 5 || c.MaxDefinitionSessions != 2 || c.MaxExternalSessions != 1 || c.MaxSessionsPerMember != 1 || c.Warnings != nil {
		t.Errorf("pools: %+v", c) // the positive control: a valid relation is kept, with no warning
	}
	// The relation, through Load (the same cases as the standalone rule below, with the keys): each is one warning
	// carrying checkCapacity's own message, and all four pools are the defaults whatever was set.
	for _, m := range []map[string]string{
		{"MAX_IN_FLIGHT_TASKS": "7", "MAX_DEFINITION_SESSIONS": "7"},
		{"MAX_IN_FLIGHT_TASKS": "0"},
		{"MAX_IN_FLIGHT_TASKS": "7", "MAX_DEFINITION_SESSIONS": "2", "MAX_EXTERNAL_SESSIONS": "2"},
		{"MAX_IN_FLIGHT_TASKS": "7", "MAX_DEFINITION_SESSIONS": "2", "MAX_EXTERNAL_SESSIONS": "1", "MAX_SESSIONS_PER_MEMBER": "0"},
		{"MAX_IN_FLIGHT_TASKS": "7", "MAX_DEFINITION_SESSIONS": "-1"},
	} {
		c, err := Load(env(m))
		if err != nil || len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "must be >= ") || !strings.HasSuffix(c.Warnings[0], "the definition, member and client pools are off") {
			t.Errorf("%v: %q, %v; want the capacity warning", m, c.Warnings, err)
		}
		if c.MaxInFlight != 5 || c.MaxDefinitionSessions != 0 || c.MaxExternalSessions != 0 || c.MaxSessionsPerMember != 1 {
			t.Errorf("%v: pools %d/%d/%d/%d, want the defaults 5/0/0/1", m, c.MaxInFlight, c.MaxDefinitionSessions, c.MaxExternalSessions, c.MaxSessionsPerMember)
		}
	}
	// An unparsed count is its default, and the relation is checked on that: here MAX_IN_FLIGHT_TASKS is 5 and the
	// definition pool 9 is outside it, so two warnings (the count, then the relation), and the defaults.
	c, err := Load(env(map[string]string{"MAX_IN_FLIGHT_TASKS": "abc", "MAX_DEFINITION_SESSIONS": "9"}))
	if err != nil || len(c.Warnings) != 2 || !strings.HasPrefix(c.Warnings[0], "MAX_IN_FLIGHT_TASKS=") || !strings.Contains(c.Warnings[1], "must be >= 0 and <") {
		t.Errorf("unparsed cap: %q, %v", c.Warnings, err)
	}
	if c.MaxDefinitionSessions != 0 {
		t.Errorf("the relation fell back to the defaults: definition pool %d, want 0", c.MaxDefinitionSessions)
	}
}

// The definition pool sits inside the global cap (docs/adr/0062): a pool as large as the cap would let defined
// agents lock admins out. (Moved from cmd/core with the rule; the cases are unchanged.)
func TestCheckCapacity(t *testing.T) {
	for _, c := range []struct {
		inFlight, def, ext, perMember int
		ok                            bool
	}{
		{5, 0, 0, 1, true}, {5, 2, 0, 1, true}, {5, 4, 0, 1, true},
		{5, 5, 0, 1, false}, {5, 6, 0, 1, false}, {5, -1, 0, 1, false},
		// docs/adr/0066 §8: the external pool sits inside the definition
		// pool, and a member needs a share of it.
		{5, 2, 1, 1, true}, {5, 2, 2, 1, false}, {5, 0, 1, 1, false}, {5, 2, -1, 1, false},
		{5, 2, 1, 0, false}, {5, 2, 0, 0, true},
	} {
		if err := checkCapacity(c.inFlight, c.def, c.ext, c.perMember); (err == nil) != c.ok {
			t.Errorf("checkCapacity(%d, %d, %d, %d) = %v, want ok=%v", c.inFlight, c.def, c.ext, c.perMember, err, c.ok)
		}
	}
}

// A required key refuses, in one error that names it; the optional keys' problems are not in it (a refusal returns
// no Config, so no warnings either). With the required key valid, every other problem is one warning of the one
// Config, each naming its variable, in a stable order; nothing is fixed first.
func TestLoadReportsEveryProblem(t *testing.T) {
	m := map[string]string{}
	var requiredKeys, otherKeys []string
	for _, r := range refusals {
		m[r.key] = r.bad[0]
		if required[r.key] {
			requiredKeys = append(requiredKeys, r.key)
		} else {
			otherKeys = append(otherKeys, r.key)
		}
	}
	m["STT_TOKEN_FLEET"] = "t" // STT_ADDR is checked only with a token
	m["MAX_DEFINITION_SESSIONS"] = "x"
	m["MAX_SESSIONS_PER_MEMBER"] = "x"
	otherKeys = append(otherKeys, "MAX_DEFINITION_SESSIONS", "MAX_SESSIONS_PER_MEMBER") // pools: no rows of their own
	_, err := Load(env(m))
	if err == nil {
		t.Fatal("loaded")
	}
	msg := err.Error()
	for _, k := range requiredKeys {
		if !strings.Contains(msg, k+"=") {
			t.Errorf("the error does not name %s:\n%s", k, msg)
		}
	}
	for _, k := range otherKeys {
		if strings.Contains(msg, k+"=") {
			t.Errorf("the error names %s, which is optional:\n%s", k, msg)
		}
	}
	if want := "invalid configuration, " + strconv.Itoa(len(requiredKeys)) + " problem(s):"; msg[:strings.Index(msg, "\n")] != want {
		t.Errorf("count line: %q, want %q", msg[:strings.Index(msg, "\n")], want)
	}
	_, again := Load(env(m))
	if again.Error() != msg {
		t.Error("the report's order changes between runs")
	}
	if lines := strings.Split(msg, "\n"); len(lines) != len(requiredKeys)+1 {
		t.Errorf("%d lines, want one per problem plus the count line", len(lines))
	}

	// The other half, with the required key valid.
	for _, k := range requiredKeys {
		delete(m, k)
	}
	c := mustLoad(t, m)
	for _, k := range otherKeys {
		n := 0
		for _, w := range c.Warnings {
			if strings.HasPrefix(w, k+"=") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d warnings name %s, want 1:\n%q", n, k, c.Warnings)
		}
	}
	// One per variable and no more: the pools' relation holds on the defaults the two unparsed counts fell back to.
	if len(c.Warnings) != len(otherKeys) {
		t.Errorf("%d warnings, want one per optional variable (%d): %q", len(c.Warnings), len(otherKeys), c.Warnings)
	}
	if again := mustLoad(t, m); !reflect.DeepEqual(again.Warnings, c.Warnings) {
		t.Error("the warnings' order changes between runs")
	}
}

// OIDC_ISSUER_URL and DASHBOARD_PUBLIC_URL are only required while the console gate is on: with FLEET_AUTH_DISABLED=1
// nothing reads them, so a malformed one is a warning. The positive control is the same value with the gate on
// (TestLoadRefusals), and an unparsed FLEET_AUTH_DISABLED reads as gate on, so it refuses. A missing one is no problem
// in Load (run() checks the issuer and the session keys are present).
func TestGateKeysRequiredOnlyWithTheGateOn(t *testing.T) {
	for _, k := range []string{"OIDC_ISSUER_URL", "DASHBOARD_PUBLIC_URL"} {
		c, err := Load(env(map[string]string{k: "not a url", "FLEET_AUTH_DISABLED": "1"}))
		if err != nil || len(c.Warnings) != 1 || !strings.HasPrefix(c.Warnings[0], k+"=") || c.OIDCIssuerURL != "" || c.DashboardPublicURL != "" {
			t.Errorf("%s, gate off: %q, %q, %v; want one warning and the value unset", k, c.Warnings, c.DashboardPublicURL, err)
		}
		for _, off := range []string{"0", "true", ""} {
			_, err := Load(env(map[string]string{k: "not a url", "FLEET_AUTH_DISABLED": off}))
			if err == nil || !strings.Contains(err.Error(), k+"=") {
				t.Errorf("%s, FLEET_AUTH_DISABLED=%q: %v; want a refusal (the gate is on)", k, off, err)
			}
		}
		if c, err := Load(env(nil)); err != nil || c.Warnings != nil {
			t.Errorf("%s unset: %q, %v; a missing value is no problem in Load", k, c.Warnings, err)
		}
	}
	// Both malformed: one error, both named.
	_, err := Load(env(map[string]string{"OIDC_ISSUER_URL": "x", "DASHBOARD_PUBLIC_URL": "y"}))
	if err == nil || !strings.Contains(err.Error(), "OIDC_ISSUER_URL=") || !strings.Contains(err.Error(), "DASHBOARD_PUBLIC_URL=") ||
		!strings.HasPrefix(err.Error(), "invalid configuration, 2 problem(s):") {
		t.Errorf("both malformed: %v", err)
	}
}

// A URL's userinfo is never echoed into a refusal or a warning, which are logged.
func TestLoadRefusalDoesNotEchoCredentials(t *testing.T) {
	_, err := Load(env(map[string]string{"OIDC_ISSUER_URL": "ftp://user:hunter2@authentik"}))
	if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "OIDC_ISSUER_URL=") {
		t.Errorf("refusal: %v", err)
	}
	_, err = Load(env(map[string]string{"OIDC_ISSUER_URL": "ftp://authentik"}))
	if err == nil || !strings.Contains(err.Error(), `"ftp://authentik"`) {
		t.Errorf("a value without userinfo is echoed so it can be found: %v", err)
	}
	// PROMETHEUS_URL is optional: a warning, with the same redaction.
	c, err := Load(env(map[string]string{"PROMETHEUS_URL": "ftp://user:hunter2@prom"}))
	if err != nil || len(c.Warnings) != 1 || strings.Contains(c.Warnings[0], "hunter2") || !strings.Contains(c.Warnings[0], "PROMETHEUS_URL=") {
		t.Errorf("warning: %q, %v", c.Warnings, err)
	}
	c, err = Load(env(map[string]string{"PROMETHEUS_URL": "ftp://prom"}))
	if err != nil || len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], `"ftp://prom"`) {
		t.Errorf("a value without userinfo is echoed in a warning too: %q, %v", c.Warnings, err)
	}
}

// validatedKeys asks Load which variables it validates, with every one set to something no rule accepts (a NUL
// byte): the ones that refuse the start are the error's lines, and with those left unset the rest load and are the
// Config's warnings. STT_TOKEN_FLEET is set, so STT_ADDR is read.
func validatedKeys(t *testing.T) (refused, warned map[string]bool) {
	t.Helper()
	name := regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	_, err := Load(func(string) string { return "\x00bad" })
	if err == nil {
		t.Fatal("a NUL-laden environment loaded")
	}
	refused, warned = map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(err.Error(), "\n")[1:] {
		if k, _, ok := strings.Cut(line, "="); ok && name.MatchString(k) {
			refused[k] = true
		}
	}
	c, err := Load(func(k string) string {
		if refused[k] {
			return ""
		}
		return "\x00bad"
	})
	if err != nil {
		t.Fatalf("the NUL-laden environment without the refusing keys still refuses: %v", err)
	}
	for _, w := range c.Warnings {
		if k, _, ok := strings.Cut(w, "="); ok && name.MatchString(k) {
			warned[k] = true
		}
	}
	return refused, warned
}

// TestRefusalsCoverEveryRule: the keys Load validates (refusing or warning) are the keys refusals (and the pools' test)
// cover, both ways, so a new rule without a test fails here, and a row for a rule that is gone does. The refusing keys
// are the required ones, exactly. They are found by asking Load with every variable set to something no rule accepts
// (validatedKeys).
func TestRefusalsCoverEveryRule(t *testing.T) {
	refused, warned := validatedKeys(t)
	validated := map[string]bool{}
	for k := range refused {
		validated[k] = true
	}
	for k := range warned {
		if refused[k] {
			t.Errorf("%s both refuses and warns", k)
		}
		validated[k] = true
	}
	if !reflect.DeepEqual(refused, required) {
		t.Errorf("refusing keys and required keys differ:\n refusing %v\n required %v", refused, required)
	}
	covered := map[string]bool{}
	for _, r := range refusals {
		covered[r.key] = true
	}
	for _, k := range []string{"MAX_IN_FLIGHT_TASKS", "MAX_DEFINITION_SESSIONS", "MAX_EXTERNAL_SESSIONS", "MAX_SESSIONS_PER_MEMBER"} {
		covered[k] = true // TestLoadPoolsWarn
	}
	if !reflect.DeepEqual(validated, covered) {
		t.Errorf("validated keys and tested keys differ:\n validated %v\n tested    %v", validated, covered)
	}
}

// STT_ADDR is validated only while dictation is on: with no STT_TOKEN_FLEET run() never dials it, so a stale malformed
// value is carried, not reported. With a token it is a warning and the default address. The positive control is a valid
// value with a token (TestLoadRefusals).
func TestSTTAddrOnlyWithToken(t *testing.T) {
	c, err := Load(env(map[string]string{"STT_ADDR": "a b:x"}))
	if err != nil || c.STTAddr != "a b:x" || c.Warnings != nil {
		t.Errorf("a malformed STT_ADDR with no token: %q, %q, %v; want it carried, no warning and no refusal", c.STTAddr, c.Warnings, err)
	}
	c, err = Load(env(map[string]string{"STT_ADDR": "a b:x", "STT_TOKEN_FLEET": "t"}))
	if err != nil || len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "STT_ADDR=") ||
		c.STTAddr != "ukubi-stt.ukubi-stt.svc.cluster.local:9090" {
		t.Errorf("the same value with a token: %q, %q, %v; want one warning naming STT_ADDR and the default address", c.STTAddr, c.Warnings, err)
	}
}

// The BRIDGE_* variables are one setting: any of BRIDGE_URL, BRIDGE_TOKEN_URL and BRIDGE_AUTH_HOST set and invalid
// warns (each problem says the bridge is off) and blanks Bridge.URL, the bridge's not-configured path, rather than keep
// a URL whose token request cannot work. The positive control is the same three valid: the URL is kept.
func TestBridgeOffOnABadSetting(t *testing.T) {
	good := map[string]string{
		"BRIDGE_URL":       "http://ukubi-bridge.ukubi-bridge.svc.cluster.local:8080",
		"BRIDGE_TOKEN_URL": "http://platform-authentik-server.authentik.svc.cluster.local/application/o/token/",
		"BRIDGE_AUTH_HOST": "authentik.bnei.dev",
		"BRIDGE_USERNAME":  "bridge-core",
		"BRIDGE_CLIENT_ID": "ukubi-bridge-api",
	}
	if c := mustLoad(t, good); c.Bridge.URL != good["BRIDGE_URL"] || c.Bridge.TokenURL == "" || c.Bridge.AuthHost == "" || c.Warnings != nil {
		t.Fatalf("a valid bridge setting: %+v, %q", c.Bridge, c.Warnings)
	}
	for _, k := range []string{"BRIDGE_URL", "BRIDGE_TOKEN_URL", "BRIDGE_AUTH_HOST"} {
		m := map[string]string{}
		for key, v := range good {
			m[key] = v
		}
		m[k] = "a b"
		c := mustLoad(t, m)
		if c.Bridge.URL != "" {
			t.Errorf("%s invalid: Bridge.URL %q, want the bridge off", k, c.Bridge.URL)
		}
		if len(c.Warnings) != 1 || !strings.HasPrefix(c.Warnings[0], k+"=") || !strings.HasSuffix(c.Warnings[0], "; the bridge is off") {
			t.Errorf("%s invalid: warnings %q; want one naming it that says the bridge is off", k, c.Warnings)
		}
	}
	// Two bad settings: each says it; and a warning of another variable does not.
	m := map[string]string{"BRIDGE_URL": "x", "BRIDGE_AUTH_HOST": "a b", "LOG_LEVEL": "loud"}
	c := mustLoad(t, m)
	off := 0
	for _, w := range c.Warnings {
		if strings.HasSuffix(w, "; the bridge is off") {
			off++
		} else if !strings.HasPrefix(w, "LOG_LEVEL=") {
			t.Errorf("a warning that does not say the bridge is off: %q", w)
		}
	}
	if off != 2 || len(c.Warnings) != 3 {
		t.Errorf("warnings %q; want 2 bridge ones and LOG_LEVEL's", c.Warnings)
	}
}

// A refusal's error carries the optional warnings found alongside it (the zero Config cannot), and its text is the same
// error without them.
func TestRefusalCarriesTheWarnings(t *testing.T) {
	_, err := Load(env(map[string]string{"OIDC_ISSUER_URL": "x", "CORE_PORT": "abc"}))
	var r *RefusalError
	if !errors.As(err, &r) || len(r.Warnings) != 1 || !strings.HasPrefix(r.Warnings[0], "CORE_PORT=") {
		t.Fatalf("refusal: %#v; want a *RefusalError with the CORE_PORT warning", err)
	}
	if strings.Contains(err.Error(), "CORE_PORT") || !strings.HasPrefix(err.Error(), "invalid configuration, 1 problem(s):") {
		t.Errorf("the error's text carries the warnings: %v", err)
	}
	if _, err := Load(env(map[string]string{"CORE_PORT": "abc"})); err != nil {
		t.Errorf("a warning alone refused: %v", err)
	}
}
