package config

import (
	"errors"
	"testing"
)

func getenv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// TestLoad: every field comes from its own variable, and the default applies when the variable is unset (or blank: getenv cannot tell).
func TestLoad(t *testing.T) {
	c, err := Load(getenv(map[string]string{
		"LOG_LEVEL": "debug", "SESSION_ID": "s1", "LEASE_ID": "l1", "CORE_GRPC_ADDR": "core:1", "WORKTREE_PATH": "/wt",
		"MCP_PORT": "2", "LOCAL_API_PORT": "3", "HEALTH_PORT": "4", "FLEET_VAULT_PROXY": "http://t:p@v:5", "FLEET_VAULT_HOSTS": "a.test,b.test",
		"EGRESS_PORT": "6", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://otel:4318/v1/traces",
	}))
	want := Config{LogLevel: "debug", SessionID: "s1", LeaseID: "l1", CoreGRPCAddr: "core:1", WorktreePath: "/wt", MCPPort: "2",
		LocalAPIPort: "3", HealthPort: "4", VaultProxy: "http://t:p@v:5", VaultHosts: "a.test,b.test", EgressPort: "6",
		TracesEndpoint: "http://otel:4318/v1/traces"}
	if err != nil || c != want {
		t.Errorf("set: %+v, %v\nwant %+v", c, err, want)
	}

	defaults := Config{LogLevel: "info", SessionID: "s1", LeaseID: "l1", CoreGRPCAddr: "agent-fleet-core.agent-fleet.svc.cluster.local:9090",
		WorktreePath: "/workspace", MCPPort: "9090", LocalAPIPort: "9091", HealthPort: "9092", EgressPort: "9093"}
	// Through getenv a blank variable and an unset one are the same "", so there is one case.
	if c, err := Load(getenv(map[string]string{"SESSION_ID": "s1", "LEASE_ID": "l1"})); err != nil || c != defaults {
		t.Errorf("unset: %+v, %v\nwant %+v", c, err, defaults)
	}
}

// TestLoadRefusals: SESSION_ID and LEASE_ID are required, each refusal is its own sentinel with the text the sidecar logs, and
// both come back, in the order they have always been checked, when both are missing.
func TestLoadRefusals(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
		want []error
	}{
		{"both unset", nil, []error{ErrNoSessionID, ErrNoLeaseID}},
		{"both blank", map[string]string{"SESSION_ID": "", "LEASE_ID": ""}, []error{ErrNoSessionID, ErrNoLeaseID}},
		{"session only", map[string]string{"SESSION_ID": "s"}, []error{ErrNoLeaseID}},
		{"lease only", map[string]string{"LEASE_ID": "l"}, []error{ErrNoSessionID}},
		{"both", map[string]string{"SESSION_ID": "s", "LEASE_ID": "l"}, nil},
	} {
		_, err := Load(getenv(c.env))
		for _, s := range []error{ErrNoSessionID, ErrNoLeaseID} {
			if got, want := errors.Is(err, s), contains(c.want, s); got != want {
				t.Errorf("%s: errors.Is(%q) = %v, want %v (err %v)", c.name, s, got, want, err)
			}
		}
		if len(c.want) == 2 && err.Error() != ErrNoSessionID.Error()+"\n"+ErrNoLeaseID.Error() {
			t.Errorf("%s: %q: the session's refusal comes first", c.name, err)
		}
		if len(c.want) == 0 && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if ErrNoSessionID.Error() != "SESSION_ID is required" || ErrNoLeaseID.Error() != "LEASE_ID is required: without it every CoreService call is rejected" {
		t.Errorf("the logged words moved: %q, %q", ErrNoSessionID, ErrNoLeaseID)
	}
}

func contains(l []error, e error) bool {
	for _, x := range l {
		if x == e {
			return true
		}
	}
	return false
}

// FromProcess is Load over the real environment.
func TestFromProcess(t *testing.T) {
	t.Setenv("SESSION_ID", "from-process")
	t.Setenv("LEASE_ID", "")
	c, err := FromProcess()
	if c.SessionID != "from-process" || !errors.Is(err, ErrNoLeaseID) || errors.Is(err, ErrNoSessionID) {
		t.Errorf("%+v, %v", c, err)
	}
}
