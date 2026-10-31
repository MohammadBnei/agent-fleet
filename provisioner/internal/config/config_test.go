package config

import (
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// An unparseable selector must mean "schedule anywhere", never "match nothing".
//
// A selector matching no node leaves sessions Pending with no error anyone
// sees — the pod is never created and the session simply looks hung. Degrading
// a typo to the previous unconstrained behaviour is the safe direction; the
// alternative fails silently and looks like a broken fleet.
func TestNodeSelectorMap(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want map[string]string
	}{
		{"the real one", "agent-fleet.dev/session-node=true", map[string]string{"agent-fleet.dev/session-node": "true"}},
		{"tolerates whitespace", "  key = value  ", map[string]string{"key": "value"}},
		{"unset means unconstrained", "", nil},
		{"no separator is not a selector", "no-equals-sign", nil},
		{"an empty key is not a selector", "=orphan-value", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Config{SessionNodeSelector: tc.in}.NodeSelectorMap()
			if len(got) != len(tc.want) {
				t.Fatalf("NodeSelectorMap(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("NodeSelectorMap(%q)[%s] = %q, want %q", tc.in, k, got[k], v)
				}
			}
		})
	}
}

// docs/adr/0080: FLEET_GIT_URL_REWRITE contains the local stack, so it fails closed. A MALFORMED value refuses the start
// (Load's error names the variable and the rule; a dropped rewrite would send kind workers to the real github.com). One
// set while the Agent Vault address is production's warns ("...; ignored") and is dropped, which is production's own
// behaviour. The production vault is matched by host, whatever form the address takes. A good one loads clean and is
// kept. No message echoes a value containing "@".
func TestLoad_GitURLRewrite(t *testing.T) {
	const localAPI, localProxy = "http://agent-vault-local.agent-fleet.svc.cluster.local:14321", "agent-vault-local.agent-fleet.svc.cluster.local:14322"
	local := func(rewrite string) map[string]string {
		return map[string]string{"FLEET_GIT_URL_REWRITE": rewrite, "AGENT_VAULT_API_ADDR": localAPI, "AGENT_VAULT_PROXY_ADDR": localProxy}
	}
	for _, tc := range []struct {
		name   string
		env    map[string]string
		refuse string // substring of the error; "" = no error
		warn   string // substring of the one warning; "" = no warning
	}{
		{"production, nothing set", map[string]string{}, "", ""},
		{"local stack", local("http://host.docker.internal:7480/"), "", ""},
		{"rewrite against the production vault", map[string]string{"FLEET_GIT_URL_REWRITE": "http://forge:7480/"}, "",
			"FLEET_GIT_URL_REWRITE is set while the Agent Vault address is production's: it is for the local stack only (docs/adr/0080); ignored"},
		{"rewrite against a production vault spelled with a slash", map[string]string{"FLEET_GIT_URL_REWRITE": "http://forge:7480/", "AGENT_VAULT_API_ADDR": "http://agent-vault.agent-vault.svc:14321/", "AGENT_VAULT_PROXY_ADDR": localProxy},
			"", "it is for the local stack only"},
		{"rewrite against the production proxy", map[string]string{"FLEET_GIT_URL_REWRITE": "http://forge:7480/", "AGENT_VAULT_API_ADDR": localAPI, "AGENT_VAULT_PROXY_ADDR": "agent-vault.agent-vault:14322"},
			"", "it is for the local stack only"},
		{"rewrite with user info", local("http://u:p@forge:7480/"), `FLEET_GIT_URL_REWRITE "<redacted: contains @>" must be an http(s) URL with a host, no user info and a trailing /`, ""},
		{"rewrite with a password in its user info", local("http://u:hunter2@forge/"), `FLEET_GIT_URL_REWRITE "<redacted: contains @>" must be an http(s) URL`, ""},
		{"rewrite with no scheme and a password", local("u:hunter2@c:4318"), `FLEET_GIT_URL_REWRITE "<redacted: contains @>" must be an http(s) URL`, ""},
		{"rewrite that does not parse and holds an @", local("http://u:hunter2@forge:bad/"), `FLEET_GIT_URL_REWRITE "<redacted: contains @>" must be an http(s) URL`, ""},
		{"rewrite without a trailing slash", local("http://forge.local"), `FLEET_GIT_URL_REWRITE "http://forge.local" must be an http(s) URL`, ""},
		{"rewrite that is not http", local("file:///tmp/"), `FLEET_GIT_URL_REWRITE "file:///tmp/" must be an http(s) URL`, ""},
		{"malformed, and against the production vault, is the refusal", map[string]string{"FLEET_GIT_URL_REWRITE": "http://forge"}, `FLEET_GIT_URL_REWRITE "http://forge" must be an http(s) URL`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(func(k string) string { return tc.env[k] })
			all := strings.Join(c.Warnings, "\n")
			if err != nil {
				all += "\n" + err.Error()
			}
			if strings.Contains(all, "hunter2") {
				t.Errorf("a message echoes the password: %q", all)
			}
			switch {
			case tc.refuse != "":
				if err == nil || !strings.Contains(err.Error(), tc.refuse) {
					t.Errorf("error %v, want it to contain %q", err, tc.refuse)
				}
				if len(c.Warnings) != 0 {
					t.Errorf("a refusal also warned: %q", c.Warnings)
				}
			case tc.warn != "":
				if err != nil {
					t.Errorf("refused: %v", err)
				}
				if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], tc.warn) || !strings.HasSuffix(c.Warnings[0], "; ignored") {
					t.Errorf("warnings %q, want one containing %q and ending \"; ignored\"", c.Warnings, tc.warn)
				}
				if c.GitURLRewrite != "" {
					t.Errorf("the rewrite was kept: %q", c.GitURLRewrite)
				}
			default:
				if err != nil || len(c.Warnings) != 0 || c.GitURLRewrite != tc.env["FLEET_GIT_URL_REWRITE"] {
					t.Errorf("good value: err %v, warnings %q, rewrite %q, want none and %q", err, c.Warnings, c.GitURLRewrite, tc.env["FLEET_GIT_URL_REWRITE"])
				}
			}
		})
	}
}

// A value containing "@" is not echoed by the other warnings either.
func TestLoad_WarningsNeverEchoAnAt(t *testing.T) {
	for _, key := range []string{"RECONCILE_INTERVAL_MS", "SHARED_INSTANCE_IDLE_TIMEOUT_MS", "SHARED_INSTANCE_PVC_SIZE"} {
		c, err := Load(func(k string) string {
			if k == key {
				return "u:hunter2@c"
			}
			return ""
		})
		if err != nil || len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], key) || !strings.Contains(c.Warnings[0], "<redacted: contains @>") || strings.Contains(c.Warnings[0], "hunter2") {
			t.Errorf("%s: %v, %q", key, err, c.Warnings)
		}
	}
}

// TestLoad: every field comes from its own variable, the default applies when the variable is unset (or blank: getenv cannot tell), and
// an optional variable that is invalid warns instead of refusing.
func TestLoad(t *testing.T) {
	env := map[string]string{
		"NAMESPACE": "ns", "WORKER_IMAGE": "w:1", "SIDECAR_IMAGE": "s:1", "WORKSPACE_PVC": "pvc", "SESSION_STORAGE_CLASS": "fast",
		"SESSION_NODE_SELECTOR": "a/b=c", "WORKSPACE_ROOT": "/ws", "E2E_HOST": "e2e.test", "PORT": "1", "GRPC_PORT": "2",
		"CORE_GRPC_ADDR": "core:3", "FLEET_PROVISIONER_TOKEN": "to-core", "FLEET_CORE_PROVISIONER_TOKEN": "from-core",
		"THOT_AUTH_TOKEN": "thot", "EXECUTOR_ADDR": "exec:4", "RECONCILE_INTERVAL_MS": "5", "LOG_LEVEL": "debug",
		"FLEET_SHARED_REPO_URL": "https://x/y.git", "FLEET_SHARED_BRANCH": "dev", "FLEET_DEFINITIONS_REF": "ref",
		"POSTGRES_IMAGE": "pg:1", "REDIS_IMAGE": "rd:1", "SHARED_INSTANCE_PVC_SIZE": "9Gi", "SHARED_INSTANCE_IDLE_TIMEOUT_MS": "6",
		"AGENT_VAULT_API_ADDR": "http://vault-local:1", "AGENT_VAULT_PROXY_ADDR": "vault-local:2", "AGENT_VAULT_SECRETS_DIR": "/v",
		"GITHUB_APP_DIR": "/app", "FLEET_GITHUB_ACCOUNTS": "1,2", "FLEET_GIT_AUTHOR_NAME": "me", "FLEET_GIT_AUTHOR_EMAIL": "me@x",
		"FLEET_GIT_URL_REWRITE": "http://forge.local/", "FLEET_VERSION": "v1.2.3", "CLAUDE_MODEL": "m", "MAX_TURNS": "9",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://otel:4318/v1/traces",
	}
	c, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Namespace: "ns", WorkerImage: "w:1", SidecarImage: "s:1", WorkspacePVC: "pvc", SessionStorageClass: "fast",
		SessionNodeSelector: "a/b=c", WorkspaceRoot: "/ws", E2eHost: "e2e.test", Port: "1", GRPCPort: "2", CoreGRPCAddr: "core:3",
		CoreToken: "to-core", CoreProvisionerToken: "from-core", ThotAuthToken: "thot", ExecutorAddr: "exec:4", ReconcileInterval: 5 * time.Millisecond,
		LogLevel: "debug", FleetSharedRepoURL: "https://x/y.git", FleetSharedBranch: "dev", FleetDefinitionsRef: "ref",
		PostgresImage: "pg:1", RedisImage: "rd:1", SharedInstancePVCSize: "9Gi", SharedInstanceIdleTimeout: 6 * time.Millisecond,
		AgentVaultAPI: "http://vault-local:1", AgentVaultProxy: "vault-local:2", AgentVaultSecretsDir: "/v", GitHubAppDir: "/app",
		GitHubAccounts: "1,2", GitAuthorName: "me", GitAuthorEmail: "me@x", GitURLRewrite: "http://forge.local/",
		Version: "v1.2.3", ClaudeModel: "m", MaxTurns: "9", TracesEndpoint: "http://otel:4318/v1/traces",
	}
	if !reflect.DeepEqual(c, want) { // Warnings is nil too: every value here is good
		t.Errorf("set:\n got %+v\nwant %+v", c, want)
	}

	defaults := Config{
		Namespace: "agent-fleet", WorkerImage: "registry.bnei.lan:5000/agent-fleet-worker:latest",
		SidecarImage: "registry.bnei.lan:5000/agent-fleet-sidecar:latest", WorkspacePVC: "agent-fleet-workspace", WorkspaceRoot: "/workspace",
		E2eHost: "bnei.dev", Port: "8080", GRPCPort: "9090", CoreGRPCAddr: DefaultCoreGRPCAddr,
		ExecutorAddr: "thot-executor.thot.svc.cluster.local:9090", ReconcileInterval: 10 * time.Second, LogLevel: "info",
		FleetSharedRepoURL: "https://github.com/MohammadBnei/agent-fleet.git", FleetSharedBranch: "main", PostgresImage: "postgres:16-alpine",
		RedisImage: "redis:7-alpine", SharedInstancePVCSize: "2Gi", SharedInstanceIdleTimeout: 12 * time.Hour, AgentVaultAPI: DefaultAgentVaultAPI,
		AgentVaultProxy: DefaultAgentVaultProxy, AgentVaultSecretsDir: "/var/run/agent-vault", GitHubAppDir: "/var/run/github-app",
		GitAuthorName: "ukubi", GitAuthorEmail: "ukubi@bnei.dev",
	}
	// Through getenv a blank variable and an unset one are the same "", so there is one case.
	if c, err := Load(func(string) string { return "" }); err != nil || !reflect.DeepEqual(c, defaults) {
		t.Errorf("unset: %+v, %v; want the defaults %+v", c, err, defaults)
	}

	// An optional key that is invalid is not a refusal: the Config comes back with a warning, and no error. A malformed
	// rewrite is: the error, and the Config with it.
	c, err = Load(func(k string) string {
		if k == "SHARED_INSTANCE_PVC_SIZE" {
			return "garbage"
		}
		return ""
	})
	if err != nil || len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "SHARED_INSTANCE_PVC_SIZE") || c.SharedInstancePVCSize != "2Gi" {
		t.Errorf("a bad size: %+v, %v", c, err)
	}
	c, err = Load(func(k string) string {
		if k == "FLEET_GIT_URL_REWRITE" {
			return "ftp://x"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "FLEET_GIT_URL_REWRITE") || len(c.Warnings) != 0 {
		t.Errorf("a bad rewrite: %+v, %v", c, err)
	}
}

// The two duration variables are positive integers of milliseconds. Anything else loads with a warning naming the
// variable and the rule and ending "; using the default <d>", and the field at its default: Load returns no error, so
// the process starts. Unset (or empty: getenv cannot tell) keeps the default with no warning. The warnings come in
// field order, and the other variable's value is not lost to a bad one.
func TestLoad_Durations(t *testing.T) {
	maxMs := strconv.FormatInt(math.MaxInt64/int64(time.Millisecond), 10)
	overMs := strconv.FormatInt(math.MaxInt64/int64(time.Millisecond)+1, 10)
	for _, v := range []struct {
		key      string
		fallback time.Duration
		text     string // the default as the warning spells it
		field    func(Config) time.Duration
	}{
		{"RECONCILE_INTERVAL_MS", 10 * time.Second, "10s", func(c Config) time.Duration { return c.ReconcileInterval }},
		{"SHARED_INSTANCE_IDLE_TIMEOUT_MS", 12 * time.Hour, "12h", func(c Config) time.Duration { return c.SharedInstanceIdleTimeout }},
	} {
		for _, tc := range []struct {
			in      string
			want    time.Duration // 0 = the default
			problem string        // substring of the warning; "" = none
		}{
			{"", 0, ""},
			{"1", time.Millisecond, ""},
			{"2500", 2500 * time.Millisecond, ""},
			{"+7", 7 * time.Millisecond, ""},
			{maxMs, time.Duration(math.MaxInt64/int64(time.Millisecond)) * time.Millisecond, ""},
			{"0", 0, v.key + " must be a positive integer (milliseconds), got \"0\""},
			{"-5", 0, v.key + " must be a positive integer (milliseconds), got \"-5\""},
			{"abc", 0, v.key + " must be a positive integer (milliseconds), got \"abc\""},
			{"1.5", 0, v.key + " must be a positive integer (milliseconds), got \"1.5\""},
			{"10s", 0, v.key + " must be a positive integer (milliseconds), got \"10s\""},
			{"  ", 0, v.key + " must be a positive integer (milliseconds), got \"  \""}, // whitespace is not unset
			{" 5", 0, v.key + " must be a positive integer (milliseconds), got \" 5\""},
			{overMs, 0, v.key + " must be a positive integer (milliseconds) no larger than " + maxMs},
			{"99999999999999999999", 0, v.key + " must be a positive integer (milliseconds) no larger than " + maxMs},
			{"-99999999999999999999", 0, v.key + " must be a positive integer (milliseconds), got"},
		} {
			c, err := Load(func(k string) string {
				if k == v.key {
					return tc.in
				}
				return ""
			})
			if err != nil {
				t.Errorf("%s=%q: an optional key refused the start: %v", v.key, tc.in, err)
			}
			want := tc.want
			if want == 0 {
				want = v.fallback
			}
			if got := v.field(c); got != want {
				t.Errorf("%s=%q: duration %v, want %v", v.key, tc.in, got, want)
			}
			switch {
			case tc.problem == "" && len(c.Warnings) != 0:
				t.Errorf("%s=%q: warned: %q", v.key, tc.in, c.Warnings)
			case tc.problem != "" && (len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], tc.problem) || !strings.HasSuffix(c.Warnings[0], "; using the default "+v.text)):
				t.Errorf("%s=%q: warnings %q, want one containing %q ending %q", v.key, tc.in, c.Warnings, tc.problem, "; using the default "+v.text)
			}
		}
	}

	// Both bad, and a malformed rewrite: a warning each for the durations, in field order, and the rewrite's refusal.
	c, err := Load(func(k string) string {
		return map[string]string{"RECONCILE_INTERVAL_MS": "abc", "SHARED_INSTANCE_IDLE_TIMEOUT_MS": "-1", "FLEET_GIT_URL_REWRITE": "ftp://x"}[k]
	})
	if err == nil || !strings.HasPrefix(err.Error(), "FLEET_GIT_URL_REWRITE") {
		t.Fatalf("a malformed rewrite did not refuse: %v", err)
	}
	if len(c.Warnings) != 2 || !strings.HasPrefix(c.Warnings[0], "RECONCILE_INTERVAL_MS must be") || !strings.HasPrefix(c.Warnings[1], "SHARED_INSTANCE_IDLE_TIMEOUT_MS must be") {
		t.Errorf("warnings:\n%q", c.Warnings)
	}
	if c, _ := Load(func(k string) string {
		return map[string]string{"RECONCILE_INTERVAL_MS": "abc", "SHARED_INSTANCE_IDLE_TIMEOUT_MS": "9"}[k]
	}); c.SharedInstanceIdleTimeout != 9*time.Millisecond || c.ReconcileInterval != 10*time.Second {
		t.Errorf("a bad interval cost the other variable its value or its own default: idle %v, interval %v", c.SharedInstanceIdleTimeout, c.ReconcileInterval)
	}
}

// SHARED_INSTANCE_PVC_SIZE is a positive Kubernetes quantity. Anything resource.ParseQuantity refuses, or that is not
// above zero, loads with a warning naming the variable and the rule and ending "; using the default 2Gi", and the
// field is "2Gi": it used to reach resource.MustParse unchecked, which panics at the first Postgres shared instance.
// A good value loads with no warning, as written.
func TestLoad_PVCSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		good bool
	}{
		{"", true}, // unset: the default, silently
		{"2Gi", true},
		{"9Gi", true},
		{"500Mi", true},
		{"10G", true},
		{"1.5Gi", true},
		{"1e3", true},
		{"garbage", false},
		{"2 Gi", false},
		{" 2Gi", false},
		{"2GB", false},
		{"Gi", false},
		{"-1Gi", false},
		{"0", false},
		{"0Gi", false},
		{"1e", false},
	} {
		c, err := Load(func(k string) string {
			if k == "SHARED_INSTANCE_PVC_SIZE" {
				return tc.in
			}
			return ""
		})
		if err != nil {
			t.Errorf("%q: an optional key refused the start: %v", tc.in, err)
		}
		wantSize := tc.in
		if tc.in == "" {
			wantSize = "2Gi"
		}
		switch {
		case tc.good && (len(c.Warnings) != 0 || c.SharedInstancePVCSize != wantSize):
			t.Errorf("%q: warnings %q, size %q; want none and %q", tc.in, c.Warnings, c.SharedInstancePVCSize, wantSize)
		case !tc.good && (len(c.Warnings) != 1 || !strings.HasPrefix(c.Warnings[0], "SHARED_INSTANCE_PVC_SIZE must be a positive Kubernetes quantity (e.g. 2Gi), got "+strconv.Quote(tc.in)) ||
			!strings.HasSuffix(c.Warnings[0], "; using the default 2Gi") || c.SharedInstancePVCSize != "2Gi"):
			t.Errorf("%q: warnings %q, size %q; want one naming the rule, and 2Gi", tc.in, c.Warnings, c.SharedInstancePVCSize)
		}
	}
}

// TestLoadMoved: the reads that lived outside Load (main's FLEET_VERSION, pod.go's CLAUDE_MODEL and MAX_TURNS) land
// on their fields, raw.
func TestLoadMoved(t *testing.T) {
	env := map[string]string{"FLEET_VERSION": "v3.4.5", "CLAUDE_MODEL": "claude-sonnet-5", "MAX_TURNS": " 12 "}
	c, err := Load(func(k string) string { return env[k] })
	if err != nil || c.Version != "v3.4.5" || c.ClaudeModel != "claude-sonnet-5" || c.MaxTurns != " 12 " {
		t.Errorf("version %q model %q turns %q err %v", c.Version, c.ClaudeModel, c.MaxTurns, err)
	}
	if c, _ := Load(func(string) string { return "" }); c.Version != "" || c.ClaudeModel != "" || c.MaxTurns != "" {
		t.Errorf("unset: version %q model %q turns %q", c.Version, c.ClaudeModel, c.MaxTurns)
	}
}

// FromProcess is Load over the real environment, and reads nothing else.
func TestFromProcess(t *testing.T) {
	t.Setenv("NAMESPACE", "from-process")
	t.Setenv("FLEET_VERSION", "v7")
	c, err := FromProcess()
	if err != nil || c.Namespace != "from-process" || c.Version != "v7" {
		t.Errorf("%+v, %v", c, err)
	}
}

// durationText drops the zero units a Duration prints, and nothing else.
func TestDurationText(t *testing.T) {
	for in, want := range map[time.Duration]string{10 * time.Second: "10s", 12 * time.Hour: "12h", time.Minute: "1m", 90 * time.Minute: "1h30m", 100 * time.Hour: "100h", 20 * time.Minute: "20m"} {
		if got := durationText(in); got != want {
			t.Errorf("durationText(%v) = %q, want %q", in, got, want)
		}
	}
}
