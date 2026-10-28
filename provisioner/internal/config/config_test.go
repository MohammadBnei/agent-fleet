package config

import (
	"strings"
	"testing"
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

// docs/adr/0080: the local stack's rewrite is refused where it could do harm.
// The production vault is matched by host, whatever form the address takes.
func TestValidate(t *testing.T) {
	local := Config{AgentVaultAPI: "http://agent-vault-local.agent-fleet.svc.cluster.local:14321", AgentVaultProxy: "agent-vault-local.agent-fleet.svc.cluster.local:14322", AgentVaultSecretsDir: "/var/run/agent-vault"}
	prod := Config{AgentVaultAPI: DefaultAgentVaultAPI, AgentVaultProxy: DefaultAgentVaultProxy, AgentVaultSecretsDir: "/var/run/agent-vault"}
	with := func(c Config, f func(*Config)) Config { f(&c); return c }
	for _, tc := range []struct {
		name string
		c    Config
		ok   bool
	}{
		{"production, nothing set", prod, true},
		{"local stack", with(local, func(c *Config) { c.GitURLRewrite = "http://host.docker.internal:7480/" }), true},
		{"rewrite against the production vault", with(prod, func(c *Config) { c.GitURLRewrite = "http://forge:7480/" }), false},
		{"rewrite against a production vault spelled with a slash", with(local, func(c *Config) {
			c.GitURLRewrite = "http://forge:7480/"
			c.AgentVaultAPI = "http://agent-vault.agent-vault.svc:14321/"
		}), false},
		{"rewrite against the production proxy", with(local, func(c *Config) {
			c.GitURLRewrite = "http://forge:7480/"
			c.AgentVaultProxy = "agent-vault.agent-vault:14322"
		}), false},
		{"rewrite with user info", with(local, func(c *Config) { c.GitURLRewrite = "http://u:p@forge:7480/" }), false},
		{"rewrite without a trailing slash", with(local, func(c *Config) { c.GitURLRewrite = "http://forge:7480" }), false},
		{"rewrite that is not http", with(local, func(c *Config) { c.GitURLRewrite = "file:///tmp/" }), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.c.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// TestLoad: every field comes from its own variable, the default applies when the variable is unset or blank, and
// the problems Validate finds come back with the Config.
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
	}
	c, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Namespace: "ns", WorkerImage: "w:1", SidecarImage: "s:1", WorkspacePVC: "pvc", SessionStorageClass: "fast",
		SessionNodeSelector: "a/b=c", WorkspaceRoot: "/ws", E2eHost: "e2e.test", Port: "1", GRPCPort: "2", CoreGRPCAddr: "core:3",
		CoreToken: "to-core", CoreProvisionerToken: "from-core", ThotAuthToken: "thot", ExecutorAddr: "exec:4", ReconcileInterval: "5",
		LogLevel: "debug", FleetSharedRepoURL: "https://x/y.git", FleetSharedBranch: "dev", FleetDefinitionsRef: "ref",
		PostgresImage: "pg:1", RedisImage: "rd:1", SharedInstancePVCSize: "9Gi", SharedInstanceIdleTimeoutMs: "6",
		AgentVaultAPI: "http://vault-local:1", AgentVaultProxy: "vault-local:2", AgentVaultSecretsDir: "/v", GitHubAppDir: "/app",
		GitHubAccounts: "1,2", GitAuthorName: "me", GitAuthorEmail: "me@x", GitURLRewrite: "http://forge.local/",
		Version: "v1.2.3", ClaudeModel: "m", MaxTurns: "9",
	}
	if c != want {
		t.Errorf("set:\n got %+v\nwant %+v", c, want)
	}

	defaults := Config{
		Namespace: "agent-fleet", WorkerImage: "registry.bnei.lan:5000/agent-fleet-worker:latest",
		SidecarImage: "registry.bnei.lan:5000/agent-fleet-sidecar:latest", WorkspacePVC: "agent-fleet-workspace", WorkspaceRoot: "/workspace",
		E2eHost: "bnei.dev", Port: "8080", GRPCPort: "9090", CoreGRPCAddr: DefaultCoreGRPCAddr,
		ExecutorAddr: "thot-executor.thot.svc.cluster.local:9090", ReconcileInterval: "10000", LogLevel: "info",
		FleetSharedRepoURL: "https://github.com/MohammadBnei/agent-fleet.git", FleetSharedBranch: "main", PostgresImage: "postgres:16-alpine",
		RedisImage: "redis:7-alpine", SharedInstancePVCSize: "2Gi", SharedInstanceIdleTimeoutMs: "43200000", AgentVaultAPI: DefaultAgentVaultAPI,
		AgentVaultProxy: DefaultAgentVaultProxy, AgentVaultSecretsDir: "/var/run/agent-vault", GitHubAppDir: "/var/run/github-app",
		GitAuthorName: "ukubi", GitAuthorEmail: "ukubi@bnei.dev",
	}
	for name, get := range map[string]func(string) string{"unset": func(string) string { return "" }, "blank": func(k string) string { return "" }} {
		if c, err := Load(get); err != nil || c != defaults {
			t.Errorf("%s: %+v, %v; want the defaults %+v", name, c, err, defaults)
		}
	}

	// A problem comes back with the Config that has it.
	c, err = Load(func(k string) string {
		if k == "FLEET_GIT_URL_REWRITE" {
			return "ftp://x"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "FLEET_GIT_URL_REWRITE") || c.GitURLRewrite != "ftp://x" {
		t.Errorf("a bad rewrite: %+v, %v", c, err)
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
