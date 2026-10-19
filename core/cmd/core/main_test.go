package main

import (
	"log/slog"
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

// The definition pool sits inside the global cap (docs/adr/0062): a pool as
// large as the cap would let defined agents lock admins out.
func TestCheckCapacity(t *testing.T) {
	for _, c := range []struct {
		inFlight, def int
		ok            bool
	}{
		{5, 0, true}, {5, 2, true}, {5, 4, true},
		{5, 5, false}, {5, 6, false}, {5, -1, false},
	} {
		if err := checkCapacity(c.inFlight, c.def); (err == nil) != c.ok {
			t.Errorf("checkCapacity(%d, %d) = %v, want ok=%v", c.inFlight, c.def, err, c.ok)
		}
	}
}
