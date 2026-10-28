package config

import (
	"errors"
	"testing"
)

func getenv(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// TestLoadExecutor: each field comes from its own variable; the ports default when unset (or blank: getenv cannot tell), the token is read raw.
func TestLoadExecutor(t *testing.T) {
	if c := LoadExecutor(getenv(map[string]string{"GRPC_PORT": "1", "HTTP_PORT": "2", "THOT_AUTH_TOKEN": " tok "})); c != (Executor{GRPCPort: "1", HTTPPort: "2", AuthToken: " tok "}) {
		t.Errorf("set: %+v", c)
	}
	// Through getenv a blank variable and an unset one are the same "", so there is one case.
	if c := LoadExecutor(getenv(nil)); c != (Executor{GRPCPort: "9090", HTTPPort: "8080"}) {
		t.Errorf("unset: %+v", c)
	}
}

// TestLoadShim: EXECUTOR_ADDR is required, with the words the shim prints; the token is optional.
func TestLoadShim(t *testing.T) {
	s, err := LoadShim(getenv(map[string]string{"EXECUTOR_ADDR": "exec:1", "THOT_AUTH_TOKEN": "tok"}))
	if err != nil || s != (Shim{ExecutorAddr: "exec:1", AuthToken: "tok"}) {
		t.Errorf("set: %+v, %v", s, err)
	}
	if s, err := LoadShim(getenv(map[string]string{"EXECUTOR_ADDR": "exec:1"})); err != nil || s.AuthToken != "" {
		t.Errorf("no token: %+v, %v", s, err)
	}
	if _, err := LoadShim(getenv(nil)); !errors.Is(err, ErrNoExecutorAddr) {
		t.Errorf("unset: %v", err)
	}
	if ErrNoExecutorAddr.Error() != "kubectl: EXECUTOR_ADDR unset — this pod has no cluster access configured" {
		t.Errorf("the printed words moved: %q", ErrNoExecutorAddr)
	}
}

// The FromProcess calls are Load over the real environment.
func TestFromProcess(t *testing.T) {
	t.Setenv("GRPC_PORT", "7")
	t.Setenv("EXECUTOR_ADDR", "e:1")
	if c := ExecutorFromProcess(); c.GRPCPort != "7" {
		t.Errorf("%+v", c)
	}
	if s, err := ShimFromProcess(); err != nil || s.ExecutorAddr != "e:1" {
		t.Errorf("%+v, %v", s, err)
	}
}
