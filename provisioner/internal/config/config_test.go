package config

import "testing"

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

// docs/adr/0080: the local stack's settings are refused where they could do
// harm. The production vault is matched by host, whatever form the address takes.
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
		{"local stack", with(local, func(c *Config) {
			c.GitURLRewrite = "http://host.docker.internal:7480/"
			c.OwnerTokenFile = "/var/run/agent-vault/OWNER_GITHUB_TOKEN"
		}), true},
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
		{"owner token outside the vault's secrets", with(local, func(c *Config) {
			c.OwnerTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
		}), false},
		{"owner token in a subdirectory", with(local, func(c *Config) { c.OwnerTokenFile = "/var/run/agent-vault/x/y" }), false},
		{"owner token climbing out", with(local, func(c *Config) { c.OwnerTokenFile = "/var/run/agent-vault/../github-app/key" }), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.c.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
