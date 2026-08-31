package config

import (
	"testing"
	"time"
)

// envDuration feeds the retention and idle sweeps, which turn it into a
// Postgres interval and delete session disk with it. Two classes of input
// matter here, and only one of them is a syntax question:
//
//   - the `d` unit is ours, not time.ParseDuration's, so it is what can
//     silently mean something else;
//   - zero and negative parse fine and are catastrophic. Verified against a
//     real Postgres: `now() - interval '0s'` and `now() - interval '-144h'`
//     both match a row created a minute ago, so either one sweeps every
//     session on the fleet at the next 60s tick.
func TestEnvDuration(t *testing.T) {
	const key = "TEST_ENV_DURATION"
	fallback := 10 * 24 * time.Hour

	cases := []struct {
		name string
		set  bool
		val  string
		want time.Duration
	}{
		{name: "unset", want: fallback},
		{name: "empty", set: true, val: "", want: fallback},

		{name: "days", set: true, val: "6d", want: 144 * time.Hour},
		{name: "one day", set: true, val: "1d", want: 24 * time.Hour},
		{name: "hours still work", set: true, val: "144h", want: 144 * time.Hour},
		{name: "seconds still work", set: true, val: "90s", want: 90 * time.Second},

		// Would sweep the entire fleet's disk. Must fall back, not parse.
		{name: "zero days", set: true, val: "0d", want: fallback},
		{name: "zero seconds", set: true, val: "0s", want: fallback},
		{name: "negative days", set: true, val: "-6d", want: fallback},
		{name: "negative hours", set: true, val: "-144h", want: fallback},

		// int64 nanoseconds cap out near 106751 days; past that the multiply
		// wraps to something arbitrary.
		{name: "overflows int64", set: true, val: "999999999999d", want: fallback},

		// Not supported on purpose: falls through to time.ParseDuration,
		// which rejects them. Fallback, not a misparse.
		{name: "mixed day unit", set: true, val: "6d12h", want: fallback},
		{name: "fractional day", set: true, val: "0.5d", want: fallback},
		{name: "bare number", set: true, val: "604800000", want: fallback},
		{name: "leading space", set: true, val: " 6d", want: fallback},
		{name: "lone unit", set: true, val: "d", want: fallback},
		{name: "garbage", set: true, val: "soon", want: fallback},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(key, tc.val)
			}
			if got := envDuration(key, fallback); got != tc.want {
				t.Errorf("envDuration(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

// The parser being right is worth nothing if Load() reads a different key, or
// assigns the result to a different field. That is this repo's own recurring
// trap — a config value existing at every layer and reaching nothing — and it
// is exactly what a rename like SESSION_RETENTION_MS -> SESSION_RETENTION can
// leave behind with every other test still green.
func TestLoadSessionRetention(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		if got := Load().SessionRetention; got != 10*24*time.Hour {
			t.Errorf("SessionRetention default = %v, want 240h", got)
		}
	})

	t.Run("from env", func(t *testing.T) {
		t.Setenv("SESSION_RETENTION", "6d")
		if got := Load().SessionRetention; got != 144*time.Hour {
			t.Errorf("SessionRetention = %v, want 144h", got)
		}
	})
}
