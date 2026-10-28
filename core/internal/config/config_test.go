package config

import (
	"strings"
	"testing"
	"time"
)

// env is a getenv over a map; mustLoad is a Load that must pass.
func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func mustLoad(t *testing.T, m map[string]string) Config {
	t.Helper()
	c, err := Load(env(m))
	if err != nil {
		t.Fatalf("Load(%v): %v", m, err)
	}
	return c
}

// SESSION_RETENTION feeds the retention sweep, which turns it into a
// Postgres interval and deletes session disk with it. Two classes of input
// matter here, and only one of them is a syntax question:
//
//   - the `d` unit is ours, not time.ParseDuration's, so it is what can
//     silently mean something else;
//   - zero and negative parse fine and are catastrophic. Verified against a
//     real Postgres: `now() - interval '0s'` and `now() - interval '-144h'`
//     both match a row created a minute ago, so either one sweeps every
//     session on the fleet at the next 60s tick.
//
// What does not satisfy the rule warns and is replaced by the default (10 days, the long end: it keeps the disk).
func TestRetention(t *testing.T) {
	const key = "SESSION_RETENTION"
	fallback := 10 * 24 * time.Hour

	cases := []struct {
		name string
		val  string
		want time.Duration // 0: a bad value, so the default and a warning
	}{
		{name: "unset", val: "", want: fallback},

		{name: "days", val: "6d", want: 144 * time.Hour},
		{name: "one day", val: "1d", want: 24 * time.Hour},
		{name: "hours still work", val: "144h", want: 144 * time.Hour},
		{name: "seconds still work", val: "90s", want: 90 * time.Second},

		// Would sweep the entire fleet's disk.
		{name: "zero days", val: "0d"},
		{name: "zero seconds", val: "0s"},
		{name: "negative days", val: "-6d"},
		{name: "negative hours", val: "-144h"},

		// int64 nanoseconds cap out near 106751 days; past that the multiply
		// wraps to something arbitrary.
		{name: "overflows int64", val: "999999999999d"},

		// Not supported on purpose: falls through to time.ParseDuration,
		// which rejects them.
		{name: "mixed day unit", val: "6d12h"},
		{name: "fractional day", val: "0.5d"},
		{name: "bare number", val: "604800000"},
		{name: "leading space", val: " 6d"},
		{name: "lone unit", val: "d"},
		{name: "garbage", val: "soon"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(env(map[string]string{key: tc.val}))
			if tc.want == 0 {
				if err != nil || len(c.Warnings) != 1 || !strings.HasPrefix(c.Warnings[0], key+"=") || c.SessionRetention != fallback {
					t.Errorf("%q: err %v, retention %v, warnings %q; want the default %v and one warning naming %s", tc.val, err, c.SessionRetention, c.Warnings, fallback, key)
				}
				return
			}
			if err != nil || c.SessionRetention != tc.want || c.Warnings != nil {
				t.Errorf("%q: %v, %v; want %v", tc.val, c.SessionRetention, err, tc.want)
			}
		})
	}
}

// The parser being right is worth nothing if Load reads a different key, or
// assigns the result to a different field. That is this repo's own recurring
// trap — a config value existing at every layer and reaching nothing — and it
// is exactly what a rename like SESSION_RETENTION_MS -> SESSION_RETENTION can
// leave behind with every other test still green.
func TestLoadSessionRetention(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		if got := mustLoad(t, nil).SessionRetention; got != 10*24*time.Hour {
			t.Errorf("SessionRetention default = %v, want 240h", got)
		}
	})

	t.Run("from env", func(t *testing.T) {
		t.Setenv("SESSION_RETENTION", "6d") // read through the process's own environment, as FromProcess does
		c, err := FromProcess()
		if err != nil {
			t.Fatal(err)
		}
		if c.SessionRetention != 144*time.Hour {
			t.Errorf("FromProcess SessionRetention = %v, want 144h", c.SessionRetention)
		}
		if got := mustLoad(t, map[string]string{"SESSION_RETENTION": "6d"}).SessionRetention; got != 144*time.Hour {
			t.Errorf("SessionRetention = %v, want 144h", got)
		}
	})
}

// TestLoadMoved: the reads that lived outside config (main's FLEET_VERSION, the dashboard's CLAUDE_MODEL,
// clientauth's tokenRef lookup) land on their fields; the lookup reads through Load's getenv, by name.
func TestLoadMoved(t *testing.T) {
	env := map[string]string{"FLEET_VERSION": "v3.4.5", "CLAUDE_MODEL": "claude-sonnet-5", "FLEET_CLIENT_TOKEN_GAME": "tok"}
	c := mustLoad(t, env)
	if c.Version != "v3.4.5" || c.ClaudeModel != "claude-sonnet-5" || c.TokenEnv == nil || c.TokenEnv("FLEET_CLIENT_TOKEN_GAME") != "tok" {
		t.Errorf("version %q model %q token %v", c.Version, c.ClaudeModel, c.TokenEnv != nil && c.TokenEnv("FLEET_CLIENT_TOKEN_GAME") == "tok")
	}
	if c := mustLoad(t, nil); c.Version != "" || c.ClaudeModel != "" {
		t.Errorf("unset: version %q model %q", c.Version, c.ClaudeModel)
	}
}
