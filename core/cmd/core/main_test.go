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

// TestMainRefusesInvalidConfig runs the real main() in a child process (this test binary, re-entered): a set value
// that does not satisfy its rule exits 1 with one "invalid configuration" line naming every variable, before
// anything is opened; the same child with valid values goes on to the next step (the database, here refused fast).
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
		cmd.Env = append([]string{"CORE_TEST_RUN_MAIN=1", "AGENTFLEET_DB_HOST=127.0.0.1", "AGENTFLEET_DB_PORT=1", "PATH=" + os.Getenv("PATH")}, env...)
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

	out, exit := child("CORE_PORT=abc", "IDLE_TIMEOUT_MS=0", "LOG_LEVEL=loud", "FLEET_AUTH_DISABLED=true")
	if exit != 1 {
		t.Fatalf("exit %d, want 1:\n%s", exit, out)
	}
	var line struct{ Msg, Error string }
	if err := json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[0]), &line); err != nil || line.Msg != "invalid configuration" {
		t.Fatalf("first line is not the refusal as JSON: %v\n%s", err, out)
	}
	for _, k := range []string{"CORE_PORT=", "IDLE_TIMEOUT_MS=", "LOG_LEVEL=", "FLEET_AUTH_DISABLED="} {
		if !strings.Contains(line.Error, k) {
			t.Errorf("the refusal does not name %s: %s", k, line.Error)
		}
	}
	if strings.Contains(out, "core starting") {
		t.Errorf("core started before refusing:\n%s", out)
	}

	// Positive control: valid values get past Load (and fail at the closed database port instead).
	out, exit = child("CORE_PORT=8080", "IDLE_TIMEOUT_MS=60000", "LOG_LEVEL=info", "FLEET_AUTH_DISABLED=0")
	if exit != 1 || strings.Contains(out, "invalid configuration") || !strings.Contains(out, "db connect failed") {
		t.Errorf("valid config: exit %d, want 1 at the database:\n%s", exit, out)
	}
}
