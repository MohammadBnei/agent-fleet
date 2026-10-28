package config

import (
	"reflect"
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

// refusals: per variable, values that must be refused and values that must load. Every key Load validates is here
// (TestRefusalsCoverEveryRule).
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
	{"SESSION_RETENTION", []string{"abc", "0", "0d", "-6d", "6d12h", "10", "-213503d", "-106752d", "106752d", "9223372036854775807d"}, []string{"6d", "144h"}},
	{"FLEET_AUTH_DISABLED", []string{"true", "false", "yes", "2", " 1", "on"}, []string{"1", "0"}},
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

func TestLoadRefusals(t *testing.T) {
	for _, r := range refusals {
		for _, v := range r.bad {
			c, err := Load(env(map[string]string{r.key: v, "STT_TOKEN_FLEET": "t"})) // STT_ADDR is read only with a token
			if err == nil {
				t.Errorf("%s=%q loaded; want a refusal", r.key, v)
				continue
			}
			if !strings.Contains(err.Error(), r.key+"=") || !strings.Contains(err.Error(), "must be") {
				t.Errorf("%s=%q: error %q does not name the variable and the rule", r.key, v, err)
			}
			if !reflect.DeepEqual(c, Config{}) {
				t.Errorf("%s=%q: a refusal returned a Config that could be run", r.key, v)
			}
		}
		for _, v := range r.good {
			if _, err := Load(env(map[string]string{r.key: v, "STT_TOKEN_FLEET": "t"})); err != nil {
				t.Errorf("%s=%q refused: %v", r.key, v, err)
			}
		}
	}
}

// The ints the pools are made of: a parse failure is a refusal here, the relation between them is checkCapacity's.
func TestLoadPoolsRefuse(t *testing.T) {
	for _, k := range []string{"MAX_IN_FLIGHT_TASKS", "MAX_DEFINITION_SESSIONS", "MAX_EXTERNAL_SESSIONS", "MAX_SESSIONS_PER_MEMBER"} {
		for _, v := range []string{"abc", "1.5", "2x", " 2"} {
			if _, err := Load(env(map[string]string{k: v})); err == nil || !strings.Contains(err.Error(), k+"=") {
				t.Errorf("%s=%q: %v; want a refusal naming it", k, v, err)
			}
		}
	}
	// The relation, through Load (the same cases as the standalone rule below, with the keys).
	ok := map[string]string{"MAX_IN_FLIGHT_TASKS": "5", "MAX_DEFINITION_SESSIONS": "2", "MAX_EXTERNAL_SESSIONS": "1"}
	if c := mustLoad(t, ok); c.MaxInFlight != 5 || c.MaxDefinitionSessions != 2 || c.MaxExternalSessions != 1 || c.MaxSessionsPerMember != 1 {
		t.Errorf("pools: %+v", c)
	}
	for _, m := range []map[string]string{
		{"MAX_IN_FLIGHT_TASKS": "5", "MAX_DEFINITION_SESSIONS": "5"},
		{"MAX_IN_FLIGHT_TASKS": "0"},
		{"MAX_IN_FLIGHT_TASKS": "5", "MAX_DEFINITION_SESSIONS": "2", "MAX_EXTERNAL_SESSIONS": "2"},
		{"MAX_IN_FLIGHT_TASKS": "5", "MAX_DEFINITION_SESSIONS": "2", "MAX_EXTERNAL_SESSIONS": "1", "MAX_SESSIONS_PER_MEMBER": "0"},
	} {
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%v loaded; want the capacity refusal", m)
		}
	}
	// One unparsed pool value: reported once, as itself, with no capacity rule computed from a default it did not read.
	_, err := Load(env(map[string]string{"MAX_IN_FLIGHT_TASKS": "abc", "MAX_DEFINITION_SESSIONS": "9"}))
	if err == nil || strings.Contains(err.Error(), "must be >= 0 and <") || !strings.Contains(err.Error(), "MAX_IN_FLIGHT_TASKS=") {
		t.Errorf("unparsed cap: %v", err)
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

// Every problem in one error, each naming its variable, in a stable order; nothing is fixed first.
func TestLoadReportsEveryProblem(t *testing.T) {
	m := map[string]string{}
	var keys []string
	for _, r := range refusals {
		m[r.key] = r.bad[0]
		keys = append(keys, r.key)
	}
	m["STT_TOKEN_FLEET"] = "t" // STT_ADDR is checked only with a token
	m["MAX_DEFINITION_SESSIONS"] = "x"
	keys = append(keys, "MAX_DEFINITION_SESSIONS")
	_, err := Load(env(m))
	if err == nil {
		t.Fatal("loaded")
	}
	msg := err.Error()
	for _, k := range keys {
		if !strings.Contains(msg, k+"=") {
			t.Errorf("the error does not name %s:\n%s", k, msg)
		}
	}
	if want := "invalid configuration, " + strconv.Itoa(len(keys)) + " problem(s):"; msg[:strings.Index(msg, "\n")] != want {
		t.Errorf("count line: %q, want %q", msg[:strings.Index(msg, "\n")], want)
	}
	_, again := Load(env(m))
	if again.Error() != msg {
		t.Error("the report's order changes between runs")
	}
	if lines := strings.Split(msg, "\n"); len(lines) != len(keys)+1 {
		t.Errorf("%d lines, want one per problem plus the count line", len(lines))
	}
}

// A URL's userinfo is never echoed into a refusal, which is logged.
func TestLoadRefusalDoesNotEchoCredentials(t *testing.T) {
	_, err := Load(env(map[string]string{"LOKI_URL": "ftp://user:hunter2@loki"}))
	if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "LOKI_URL=") {
		t.Errorf("refusal: %v", err)
	}
	_, err = Load(env(map[string]string{"LOKI_URL": "ftp://loki"}))
	if err == nil || !strings.Contains(err.Error(), `"ftp://loki"`) {
		t.Errorf("a value without userinfo is echoed so it can be found: %v", err)
	}
}

// TestRefusalsCoverEveryRule: the keys Load validates are the keys refusals (and the pools' test) cover, both ways, so a
// new rule without a test fails here, and a row for a rule that is gone does. They are found by asking Load with
// every variable set to something no rule accepts (a NUL byte).
func TestRefusalsCoverEveryRule(t *testing.T) {
	_, err := Load(func(string) string { return "\x00bad" })
	if err == nil {
		t.Fatal("a NUL-laden environment loaded")
	}
	refused := map[string]bool{}
	for _, line := range strings.Split(err.Error(), "\n")[1:] {
		if k, _, ok := strings.Cut(line, "="); ok {
			refused[k] = true
		}
	}
	covered := map[string]bool{}
	for _, r := range refusals {
		covered[r.key] = true
	}
	for _, k := range []string{"MAX_IN_FLIGHT_TASKS", "MAX_DEFINITION_SESSIONS", "MAX_EXTERNAL_SESSIONS", "MAX_SESSIONS_PER_MEMBER"} {
		covered[k] = true // TestLoadPoolsRefuse
	}
	if !reflect.DeepEqual(refused, covered) {
		t.Errorf("validated keys and tested keys differ:\n validated %v\n tested    %v", refused, covered)
	}
}

// STT_ADDR is validated only while dictation is on: with no STT_TOKEN_FLEET run() never dials it, so a stale malformed
// value is carried, not refused. The positive control is the same value with a token (TestLoadRefusals).
func TestSTTAddrOnlyWithToken(t *testing.T) {
	c, err := Load(env(map[string]string{"STT_ADDR": "a b:x"}))
	if err != nil || c.STTAddr != "a b:x" {
		t.Errorf("a malformed STT_ADDR with no token: %q, %v; want it carried and no refusal", c.STTAddr, err)
	}
	if _, err := Load(env(map[string]string{"STT_ADDR": "a b:x", "STT_TOKEN_FLEET": "t"})); err == nil || !strings.Contains(err.Error(), "STT_ADDR=") {
		t.Errorf("the same value with a token: %v; want a refusal naming STT_ADDR", err)
	}
}
