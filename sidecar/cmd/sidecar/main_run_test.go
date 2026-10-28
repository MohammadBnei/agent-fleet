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

// SESSION_ID is refused before LEASE_ID, the level's warning comes first, and LEASE_ID's refusal already carries
// the session id.
func TestMain_Refusals(t *testing.T) {
	for _, c := range []struct {
		name string
		env  []string
		want []string // substrings, in this order
		not  string   // a substring that must be absent
	}{
		{"nothing", nil, []string{`"SESSION_ID is required"`}, "LEASE_ID"},
		{"blank ids", []string{"SESSION_ID=", "LEASE_ID="}, []string{`"SESSION_ID is required"`}, "LEASE_ID"},
		{"lease only", []string{"LEASE_ID=l"}, []string{`"SESSION_ID is required"`}, "sessionId"},
		{"session only", []string{"SESSION_ID=s1"}, []string{`"LEASE_ID is required: without it every CoreService call is rejected","sessionId":"s1"`}, "SESSION_ID is required"},
		{"bad level warns first", []string{"LOG_LEVEL=bogus"}, []string{`"invalid LOG_LEVEL, defaulting to info","value":"bogus"`, `"SESSION_ID is required"`}, ""},
		{"a vault proxy without a token", []string{"SESSION_ID=s1", "LEASE_ID=l1", "FLEET_VAULT_PROXY=http://v:1"}, []string{`"egress router","sessionId":"s1","error":"egress: FLEET_VAULT_PROXY is not a proxy URL with a token"`}, ""},
		{"an egress port that is no port", []string{"SESSION_ID=s1", "LEASE_ID=l1", "FLEET_VAULT_PROXY=http://t:p@v:1", "EGRESS_PORT=x"}, []string{`"egress router listen"`, "unknown port"}, ""},
	} {
		code, out := runMain(t, c.env...)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1\n%s", c.name, code, out)
		}
		rest := out
		for _, w := range c.want {
			i := strings.Index(rest, w)
			if i < 0 {
				t.Errorf("%s: %q not in order in\n%s", c.name, w, out)
				break
			}
			rest = rest[i+len(w):]
		}
		if c.not != "" && strings.Contains(out, c.not) {
			t.Errorf("%s: %q should not be logged\n%s", c.name, c.not, out)
		}
	}
}
