package config

import (
	"testing"
	"time"
)

// The `d` unit is ours, not time.ParseDuration's, so it is the part that can
// silently mean the wrong thing: "6d" parsed as 6ns would sweep every session
// on the next tick.
func TestEnvDuration(t *testing.T) {
	const key = "TEST_ENV_DURATION"
	fallback := 3 * 24 * time.Hour

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
		{name: "zero days", set: true, val: "0d", want: 0},
		{name: "hours still work", set: true, val: "144h", want: 144 * time.Hour},
		{name: "seconds still work", set: true, val: "90s", want: 90 * time.Second},
		// Not supported on purpose: falls through to time.ParseDuration, which
		// rejects it. Fallback, not a misparse.
		{name: "mixed day unit", set: true, val: "6d12h", want: fallback},
		{name: "fractional day", set: true, val: "0.5d", want: fallback},
		{name: "bare number", set: true, val: "604800000", want: fallback},
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
