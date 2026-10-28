// Package config is the configuration of the executor's two binaries,
// thot-executor and the kubectl shim: one struct each, read from the
// environment once (ExecutorFromProcess, ShimFromProcess) or from a test's map
// (LoadExecutor, LoadShim). Nothing else in this module reads the environment
// (TestEnvBan; mountain plan/15 §5), apart from the child-process pass-through
// that test names.
package config

import (
	"errors"
	"os"
)

// ErrNoExecutorAddr is the shim's one refusal, with the words it has always
// printed.
var ErrNoExecutorAddr = errors.New("kubectl: EXECUTOR_ADDR unset — this pod has no cluster access configured")

// Executor is what thot-executor runs with. Each field names its variable; a
// blank variable is an unset one except where a field says otherwise.
//
// It reports no problems of its own: the one rule, the bearer token's length,
// is newGRPCServer's (cmd/executor), which the tests call and which is twinned
// with the provisioner's, so the refusal is made where the server is built and
// the shipped wiring is the tested wiring.
type Executor struct {
	// GRPCPort is the ExecutorService listener's port (GRPC_PORT, default
	// "9090"). A string, as the listener wants it: a value that is no port
	// fails at Listen.
	GRPCPort string
	// HTTPPort is the liveness listener's port (HTTP_PORT, default "8080").
	HTTPPort string
	// AuthToken is the bearer every call must present (THOT_AUTH_TOKEN, read
	// raw). Empty, blank or short refuses to start.
	AuthToken string
}

// Shim is what the kubectl shim runs with, in a thot session pod.
type Shim struct {
	// ExecutorAddr is where argv is sent (EXECUTOR_ADDR, required). Unset means
	// the pod has no cluster access.
	ExecutorAddr string
	// AuthToken is sent as the bearer when set (THOT_AUTH_TOKEN, read raw); the
	// executor refuses a call without a right one.
	AuthToken string
}

// ExecutorFromProcess is thot-executor's configuration from its environment:
// the one call that reads it.
func ExecutorFromProcess() Executor { return LoadExecutor(os.Getenv) }

// LoadExecutor is thot-executor's configuration from getenv.
func LoadExecutor(getenv func(string) string) Executor {
	port := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	return Executor{
		GRPCPort:  port("GRPC_PORT", "9090"),
		HTTPPort:  port("HTTP_PORT", "8080"),
		AuthToken: getenv("THOT_AUTH_TOKEN"),
	}
}

// ShimFromProcess is the shim's configuration from its environment: the one
// call that reads it.
func ShimFromProcess() (Shim, error) { return LoadShim(os.Getenv) }

// LoadShim is the shim's configuration from getenv. The error is
// ErrNoExecutorAddr when EXECUTOR_ADDR is unset or empty; the Shim comes back
// with it.
func LoadShim(getenv func(string) string) (Shim, error) {
	s := Shim{ExecutorAddr: getenv("EXECUTOR_ADDR"), AuthToken: getenv("THOT_AUTH_TOKEN")}
	if s.ExecutorAddr == "" {
		return s, ErrNoExecutorAddr
	}
	return s, nil
}
