package main

import (
	"bytes"
	"context"
	"encoding/json"
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
// first and logged, then the configuration's warnings, then the core token's refusal, the one required key.
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
		{"a bad optional value is warned about and does not hide the token's refusal", []string{"FLEET_GIT_URL_REWRITE=ftp://x"},
			[]string{`"level":"WARN"`, `FLEET_GIT_URL_REWRITE \"ftp://x\" must be an http(s) URL`, `"provisioner starting"`, "FLEET_CORE_PROVISIONER_TOKEN is unset"}},
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

// warnings is the msg of each WARN line main logged, in order.
func warnings(t *testing.T, out string) []string {
	t.Helper()
	var msgs []string
	for _, line := range strings.Split(out, "\n") {
		var l struct{ Level, Msg string }
		if json.Unmarshal([]byte(line), &l) == nil && l.Level == "WARN" {
			msgs = append(msgs, l.Msg)
		}
	}
	return msgs
}

// An optional variable that is set but invalid is warned about, naming the variable and the rule, and takes its default
// (FLEET_GIT_URL_REWRITE is dropped): main logs one WARN line per variable, as JSON, after the handler is installed,
// and goes on past the configuration, to the cluster, which a test has none of. A zero RECONCILE_INTERVAL_MS used to
// refuse the start here (it would panic the reconcile loop's ticker), and so did the other three. The required key,
// the core token, is still refused ahead of everything else, and a good value draws no warning.
func TestMain_WarnsAndStartsOnBadOptionalValues(t *testing.T) {
	token := "FLEET_CORE_PROVISIONER_TOKEN=" + strings.Repeat("t", 40)
	for _, c := range []struct {
		name string
		env  []string
		want []string // the WARN messages, each a substring, in this order
	}{
		{"zero interval", []string{token, "RECONCILE_INTERVAL_MS=0"}, []string{`RECONCILE_INTERVAL_MS must be a positive integer (milliseconds), got "0"; using the default 10s`}},
		{"interval that is no integer", []string{token, "RECONCILE_INTERVAL_MS=abc"}, []string{`RECONCILE_INTERVAL_MS must be a positive integer (milliseconds), got "abc"; using the default 10s`}},
		{"negative idle timeout", []string{token, "SHARED_INSTANCE_IDLE_TIMEOUT_MS=-5"}, []string{`SHARED_INSTANCE_IDLE_TIMEOUT_MS must be a positive integer (milliseconds), got "-5"; using the default 12h`}},
		{"idle timeout that is no integer", []string{token, "SHARED_INSTANCE_IDLE_TIMEOUT_MS=1.5"}, []string{`SHARED_INSTANCE_IDLE_TIMEOUT_MS must be a positive integer (milliseconds), got "1.5"; using the default 12h`}},
		{"pvc size that is no quantity", []string{token, "SHARED_INSTANCE_PVC_SIZE=garbage"}, []string{`SHARED_INSTANCE_PVC_SIZE must be a positive Kubernetes quantity (e.g. 2Gi), got "garbage"`}},
		{"rewrite that is no URL", []string{token, "FLEET_GIT_URL_REWRITE=ftp://x"}, []string{`FLEET_GIT_URL_REWRITE "ftp://x" must be an http(s) URL with a host, no user info and a trailing /; ignored`}},
		{"rewrite against the production vault", []string{token, "FLEET_GIT_URL_REWRITE=http://forge.local/"}, []string{"it is for the local stack only (docs/adr/0080); ignored"}},
		{"all of them, one warning each, in field order", []string{token, "RECONCILE_INTERVAL_MS=0", "SHARED_INSTANCE_PVC_SIZE=garbage", "SHARED_INSTANCE_IDLE_TIMEOUT_MS=0", "FLEET_GIT_URL_REWRITE=ftp://x"},
			[]string{"RECONCILE_INTERVAL_MS must be", "SHARED_INSTANCE_PVC_SIZE must be", "SHARED_INSTANCE_IDLE_TIMEOUT_MS must be", "FLEET_GIT_URL_REWRITE"}},
	} {
		code, out := runMain(t, c.env...)
		// The cluster's refusal, not the configuration's: main got past the configuration.
		if code != 1 || !strings.Contains(out, "k8s client init failed") || strings.Contains(out, "refusing to start") {
			t.Errorf("%s: exit %d, want startup to go on to the cluster and not refuse:\n%s", c.name, code, out)
		}
		got := warnings(t, out)
		if len(got) != len(c.want) {
			t.Errorf("%s: %d WARN lines %q, want %d", c.name, len(got), got, len(c.want))
			continue
		}
		for i, w := range c.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("%s: WARN %d is %q, want it to contain %q", c.name, i, got[i], w)
			}
			if !strings.Contains(got[i], "; using the default ") && !strings.HasSuffix(got[i], "; ignored") {
				t.Errorf("%s: WARN %d is %q, want it to say what happened to the value", c.name, i, got[i])
			}
		}
		// The handler is installed before the warnings are logged: they are JSON, and come before "starting".
		if w, st := strings.Index(out, `"level":"WARN"`), strings.Index(out, `"provisioner starting"`); w < 0 || st < w {
			t.Errorf("%s: the warnings are not logged ahead of \"provisioner starting\":\n%s", c.name, out)
		}
	}
	// The required key still comes first: a bad optional value with no token is the token's refusal, after the warning.
	if code, out := runMain(t, "RECONCILE_INTERVAL_MS=0"); code != 1 || !strings.Contains(out, "FLEET_CORE_PROVISIONER_TOKEN is unset") || strings.Contains(out, "k8s client init failed") {
		t.Errorf("no token: exit %d\n%s", code, out)
	}

	// Positive control: good values draw no warning, and main goes on to the cluster just the same.
	code, out := runMain(t, token, "RECONCILE_INTERVAL_MS=5000", "SHARED_INSTANCE_IDLE_TIMEOUT_MS=60000", "SHARED_INSTANCE_PVC_SIZE=5Gi")
	if w := warnings(t, out); code != 1 || len(w) != 0 || !strings.Contains(out, "k8s client init failed") {
		t.Errorf("good values: exit %d, WARN lines %q, want none and the cluster's refusal:\n%s", code, w, out)
	}
}
