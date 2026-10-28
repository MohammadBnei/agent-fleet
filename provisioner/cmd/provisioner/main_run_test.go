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
	cmd.Env = append([]string{"FLEET_TEST_RUN_MAIN=1", "PATH=" + os.Getenv("PATH"), "GOCOVERDIR=" + t.TempDir()}, env...)
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

// The refusals, in the order main makes them: the log level and the release are read from the environment
// first and logged, then the core token's refusal, then the configuration's (docs/adr/0080).
func TestMain_Refusals(t *testing.T) {
	token := strings.Repeat("t", 40)
	for _, c := range []struct {
		name string
		env  []string
		want []string // substrings, in this order
	}{
		{"no token", []string{"FLEET_VERSION=v9.1"}, []string{`"provisioner starting","version":"v9.1"`, "FLEET_CORE_PROVISIONER_TOKEN is unset or shorter than 32 bytes"}},
		{"no release is dev", nil, []string{`"version":"dev"`, "FLEET_CORE_PROVISIONER_TOKEN"}},
		{"bad level warns, then refuses", []string{"LOG_LEVEL=bogus"}, []string{`"invalid LOG_LEVEL, defaulting to info","value":"bogus"`, `"provisioner starting"`, "refusing to start"}},
		{"the token is refused before the rewrite", []string{"FLEET_GIT_URL_REWRITE=ftp://x"}, []string{"FLEET_CORE_PROVISIONER_TOKEN is unset"}},
		{"bad rewrite", []string{"FLEET_CORE_PROVISIONER_TOKEN=" + token, "FLEET_GIT_URL_REWRITE=ftp://x"}, []string{`"provisioner starting"`, `FLEET_GIT_URL_REWRITE \"ftp://x\" must be an http(s) URL`}},
		{"rewrite against the production vault", []string{"FLEET_CORE_PROVISIONER_TOKEN=" + token, "FLEET_GIT_URL_REWRITE=http://forge.local/"}, []string{"it is for the local stack only"}},
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
	}
	if _, out := runMain(t, "FLEET_CORE_PROVISIONER_TOKEN="+token, "FLEET_GIT_URL_REWRITE=ftp://x"); strings.Contains(out, "FLEET_CORE_PROVISIONER_TOKEN is unset") {
		t.Errorf("a long enough token was refused:\n%s", out)
	}
}

// RECONCILE_INTERVAL_MS and SHARED_INSTANCE_IDLE_TIMEOUT_MS that are no positive integer of milliseconds are refused
// at start, before main goes near the cluster: a zero interval panics the reconcile loop's ticker once main is
// running, and a zero idle timeout deletes every annotated shared instance on every tick. The core token's refusal
// still comes first, and a good value is not refused.
func TestMain_RefusesBadDurations(t *testing.T) {
	token := "FLEET_CORE_PROVISIONER_TOKEN=" + strings.Repeat("t", 40)
	for _, c := range []struct {
		name string
		env  []string
		want []string // substrings, in this order
	}{
		{"zero interval", []string{token, "RECONCILE_INTERVAL_MS=0"}, []string{`"provisioner starting"`, `RECONCILE_INTERVAL_MS must be a positive integer (milliseconds), got \"0\"`}},
		{"interval that is no integer", []string{token, "RECONCILE_INTERVAL_MS=abc"}, []string{`"provisioner starting"`, `RECONCILE_INTERVAL_MS must be a positive integer (milliseconds), got \"abc\"`}},
		{"negative idle timeout", []string{token, "SHARED_INSTANCE_IDLE_TIMEOUT_MS=-5"}, []string{`"provisioner starting"`, `SHARED_INSTANCE_IDLE_TIMEOUT_MS must be a positive integer (milliseconds), got \"-5\"`}},
		{"idle timeout that is no integer", []string{token, "SHARED_INSTANCE_IDLE_TIMEOUT_MS=1.5"}, []string{`SHARED_INSTANCE_IDLE_TIMEOUT_MS must be a positive integer (milliseconds), got \"1.5\"`}},
		{"both, and the rewrite, joined", []string{token, "RECONCILE_INTERVAL_MS=0", "SHARED_INSTANCE_IDLE_TIMEOUT_MS=0", "FLEET_GIT_URL_REWRITE=ftp://x"},
			[]string{"RECONCILE_INTERVAL_MS must be", "SHARED_INSTANCE_IDLE_TIMEOUT_MS must be", "FLEET_GIT_URL_REWRITE"}},
		{"the token is refused before the interval", []string{"RECONCILE_INTERVAL_MS=0"}, []string{"FLEET_CORE_PROVISIONER_TOKEN is unset"}},
	} {
		code, out := runMain(t, c.env...)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1\n%s", c.name, code, out)
		}
		if !strings.Contains(out, "refusing to start") {
			t.Errorf("%s: not the refusal path:\n%s", c.name, out)
		}
		if strings.Contains(out, "k8s client init failed") {
			t.Errorf("%s: went on to the cluster:\n%s", c.name, out)
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
	}
	if _, out := runMain(t, "RECONCILE_INTERVAL_MS=0"); strings.Contains(out, "RECONCILE_INTERVAL_MS must be") {
		t.Errorf("the interval was reported ahead of the token:\n%s", out)
	}

	// Positive control: good values pass the configuration, so main goes on (to the cluster, which a test has none of).
	code, out := runMain(t, token, "RECONCILE_INTERVAL_MS=5000", "SHARED_INSTANCE_IDLE_TIMEOUT_MS=60000")
	if code != 1 || strings.Contains(out, "must be a positive integer") || !strings.Contains(out, "k8s client init failed") {
		t.Errorf("good durations: exit %d, want the cluster's refusal and not the configuration's:\n%s", code, out)
	}
}
