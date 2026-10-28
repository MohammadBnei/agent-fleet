package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestResolveLogLevel(t *testing.T) {
	cases := []struct {
		raw       string
		wantLevel slog.Level
		wantWarn  bool
	}{
		{"", slog.LevelInfo, false},
		{"debug", slog.LevelDebug, false},
		{"DEBUG", slog.LevelDebug, false},
		{"warn", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"bogus", slog.LevelInfo, true},
	}
	for _, c := range cases {
		level, warning := resolveLogLevel(c.raw)
		if level != c.wantLevel {
			t.Errorf("resolveLogLevel(%q) level = %v, want %v", c.raw, level, c.wantLevel)
		}
		if (warning != "") != c.wantWarn {
			t.Errorf("resolveLogLevel(%q) warning = %q, wantWarn %v", c.raw, warning, c.wantWarn)
		}
	}
}

// TestMainRefusesInvalidConfig runs the real main() in a child process (this test binary, re-entered): a set value of a
// required key that does not satisfy its rule (OIDC_ISSUER_URL or DASHBOARD_PUBLIC_URL, with the console gate on)
// exits 1 with one "invalid configuration" line, before anything is opened; a bad value of an optional key is logged
// as one WARN line each and core goes on to the next step (the database, here refused fast), as it does with valid values.
func TestMainRefusesInvalidConfig(t *testing.T) {
	if os.Getenv("CORE_TEST_RUN_MAIN") == "1" {
		main()
		return
	}
	child := func(env ...string) (out string, exit int) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestMainRefusesInvalidConfig$")
		// A clean environment: nothing the runner sets reaches the child's Load. The database is a closed port, so a
		// config that loads fails at the connection and not later.
		// GOCOVERDIR: a coverage-instrumented test binary writes its profile there when it exits, and would warn about it
		// into the output this test reads.
		cmd.Env = append([]string{"CORE_TEST_RUN_MAIN=1", "AGENTFLEET_DB_HOST=127.0.0.1", "AGENTFLEET_DB_PORT=1", "PATH=" + os.Getenv("PATH"), "GOCOVERDIR=" + t.TempDir()}, env...)
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		err := cmd.Run()
		var ee *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &ee):
			exit = ee.ExitCode()
		default:
			t.Fatal(err)
		}
		return buf.String(), exit
	}

	// The refusal line, and the optional problem found alongside it (CORE_PORT=abc), which is logged before it.
	out, exit := child("OIDC_ISSUER_URL=authentik", "DASHBOARD_PUBLIC_URL=fleet.bnei.dev", "CORE_PORT=abc")
	if exit != 1 {
		t.Fatalf("exit %d, want 1:\n%s", exit, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var line struct{ Level, Msg, Error string }
	refusalAt := -1
	for i, l := range lines {
		var rec struct{ Level, Msg, Error string }
		if json.Unmarshal([]byte(l), &rec) == nil && rec.Msg == "invalid configuration" {
			line, refusalAt = rec, i
		}
	}
	if refusalAt != len(lines)-1 || line.Level != "ERROR" {
		t.Fatalf("the refusal is not the last line, as JSON at ERROR:\n%s", out)
	}
	if !strings.Contains(line.Error, "OIDC_ISSUER_URL=") || !strings.Contains(line.Error, "DASHBOARD_PUBLIC_URL=") || strings.Contains(line.Error, "CORE_PORT=") {
		t.Errorf("the refusal names the required keys and only them: %s", line.Error)
	}
	if p := warnedProblems(t, strings.Join(lines[:refusalAt], "\n")); len(p) != 1 || !strings.HasPrefix(p[0], "CORE_PORT=") {
		t.Errorf("the optional problem is logged before the refusal, once: %q\n%s", p, out)
	}
	if strings.Contains(out, "core starting") {
		t.Errorf("core started before refusing:\n%s", out)
	}

	// Optional keys: not a refusal. Core gets as far as the database, and logs one WARN line per problem, as JSON, with
	// the problem naming the variable.
	bad := []string{"IDLE_TIMEOUT_MS=0", "LOG_LEVEL=loud", "FLEET_AUTH_DISABLED=true", "CORE_PORT=abc", "SESSION_RETENTION=0", "PROMETHEUS_URL=ftp://user:hunter2@prom"}
	out, exit = child(bad...)
	if exit != 1 || strings.Contains(out, "invalid configuration\"") || !strings.Contains(out, "db connect failed") {
		t.Fatalf("optional bad values: exit %d, want 1 at the database:\n%s", exit, out)
	}
	assertOneWarningEach(t, out, bad)
	if strings.Contains(out, "hunter2") {
		t.Errorf("a warning echoed a credential:\n%s", out)
	}

	// LOG_LEVEL=error does not hide them: the warnings do not go through the configured level (the control is that the
	// level is in force: "core starting" is INFO and is not printed).
	out, exit = child("LOG_LEVEL=error", "CORE_PORT=abc", "IDLE_TIMEOUT_MS=0")
	if exit != 1 || !strings.Contains(out, "db connect failed") || strings.Contains(out, "core starting") {
		t.Fatalf("LOG_LEVEL=error: exit %d, want 1 at the database with INFO hidden:\n%s", exit, out)
	}
	assertOneWarningEach(t, out, []string{"CORE_PORT=abc", "IDLE_TIMEOUT_MS=0"})

	// Positive control: valid values get past Load with no warning (and fail at the closed database port instead).
	out, exit = child("CORE_PORT=8080", "IDLE_TIMEOUT_MS=60000", "LOG_LEVEL=info", "FLEET_AUTH_DISABLED=0")
	if exit != 1 || strings.Contains(out, "invalid configuration") || !strings.Contains(out, "db connect failed") {
		t.Errorf("valid config: exit %d, want 1 at the database:\n%s", exit, out)
	}
}

// warnedProblems is the "problem" of every config warning line in out, which must each be JSON at WARN.
func warnedProblems(t *testing.T, out string) (problems []string) {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		var rec struct{ Level, Msg, Problem string }
		if json.Unmarshal([]byte(l), &rec) != nil || rec.Msg != "invalid configuration value, using the default" {
			continue
		}
		if rec.Level != "WARN" {
			t.Errorf("a warning logged at %q: %s", rec.Level, l)
		}
		problems = append(problems, rec.Problem)
	}
	return problems
}

// assertOneWarningEach: out has exactly one config warning per KEY=value in bad, each naming its variable.
func assertOneWarningEach(t *testing.T, out string, bad []string) {
	t.Helper()
	problems := warnedProblems(t, out)
	if len(problems) != len(bad) {
		t.Errorf("%d warning lines, want one per problem (%d):\n%s", len(problems), len(bad), out)
	}
	for _, b := range bad {
		k, _, _ := strings.Cut(b, "=")
		n := 0
		for _, p := range problems {
			if strings.HasPrefix(p, k+"=") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d warnings name %s, want 1:\n%s", n, k, out)
		}
	}
}
