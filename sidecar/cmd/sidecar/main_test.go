package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	uktel "github.com/MohammadBnei/ukubi-go/telemetry"
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

// The agent-facing listeners must never bind the pod IP. Everything they serve
// acts under this session's authority, and worker pods have no NetworkPolicy
// since docs/adr/0048 §6 deleted the sandbox — so the bind address is the only
// thing between another pod and this session's human-message feed. The health
// listener is the deliberate exception: kubelet dials probes at the pod IP.
func TestServers_OnlyHealthBindsBeyondLoopback(t *testing.T) {
	mcp, localAPI, health := servers(nil, nil, "9090", "9091", "9092")

	for _, c := range []struct {
		name string
		srv  *http.Server
		want string
	}{
		{"mcp", mcp, "127.0.0.1:9090"},
		{"local api", localAPI, "127.0.0.1:9091"},
		{"health", health, ":9092"},
	} {
		if c.srv.Addr != c.want {
			t.Errorf("%s listener Addr = %q, want %q", c.name, c.srv.Addr, c.want)
		}
	}
}

// The health listener is the one reachable from off-pod, so it must expose
// nothing but readiness. Adding a route to localapi.New is free; adding one to
// localapi.NewHealth publishes it to the whole namespace.
func TestNewHealth_ServesNothingButReadyz(t *testing.T) {
	_, _, health := servers(nil, nil, "9090", "9091", "9092")

	for _, path := range []string{"/message", "/human-messages", "/permission-mode", "/session-id", "/journal", "/task", "/telemetry"} {
		rec := httptest.NewRecorder()
		health.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("health listener served %s with %d, want 404 — it is reachable from other pods", path, rec.Code)
		}
	}
}

// TestServers_TracesAllButHealth: the two agent-facing listeners run each
// request in a span, so their access log line carries a trace_id; health does
// not (a probe every few seconds is noise).
func TestServers_TracesAllButHealth(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(uktel.NewLogger(&logs))
	t.Cleanup(func() { slog.SetDefault(prev) })
	mcp, localAPI, health := servers(uktel.WithExporter(nil, "agent-fleet-sidecar"), nil, "9090", "9091", "9092")

	for _, c := range []struct {
		name   string
		srv    *http.Server
		traced bool
	}{
		{"mcp", mcp, true},
		{"local api", localAPI, true},
		{"health", health, false},
	} {
		logs.Reset()
		c.srv.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/no-such-path", nil))
		var line map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &line); err != nil {
			t.Fatalf("%s: not one JSON line: %q: %v", c.name, logs.String(), err)
		}
		if _, ok := line["trace_id"]; ok != c.traced {
			t.Errorf("%s: trace_id on the line %v, want %v", c.name, ok, c.traced)
		}
	}
}
