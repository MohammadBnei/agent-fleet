package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The test binary is main when FLEET_TEST_RUN_MAIN is set, so the refusals can be seen as a process does: the
// exit code and what it logged.
func TestMain(m *testing.M) {
	if os.Getenv("FLEET_TEST_RUN_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func runMain(t *testing.T, env ...string) (code int, out string) {
	t.Helper()
	// A main that does not refuse would serve for ever: the test fails instead of hanging.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append([]string{"FLEET_TEST_RUN_MAIN=1", "PATH=" + os.Getenv("PATH")}, env...)
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), b.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, b.String()
}

// main reads THOT_AUTH_TOKEN and GRPC_PORT through the config: a short token is refused before anything listens,
// and a long one gets as far as the port it was given.
func TestMain_Refusals(t *testing.T) {
	for _, c := range []struct {
		name string
		env  []string
		want string
	}{
		{"no token", nil, "THOT_AUTH_TOKEN is unset or shorter than 32 bytes"},
		{"short token", []string{"THOT_AUTH_TOKEN=" + strings.Repeat("t", 31)}, "THOT_AUTH_TOKEN is unset or shorter than 32 bytes"},
		{"blank token", []string{"THOT_AUTH_TOKEN=" + strings.Repeat(" ", 40)}, "THOT_AUTH_TOKEN is unset or shorter than 32 bytes"},
		{"a port that is no port", []string{"THOT_AUTH_TOKEN=" + strings.Repeat("t", 32), "GRPC_PORT=x"}, `"listen","error":"listen tcp: lookup tcp/x: unknown port"`},
	} {
		code, out := runMain(t, c.env...)
		if code != 1 || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, want 1 and %q in\n%s", c.name, code, c.want, out)
		}
	}
}
