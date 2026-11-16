package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// The fleet-shared refresher runs at once (a fresh provisioner's first warm
// should find a refreshed clone), keeps ticking past a failure, and stops
// with ctx.
func TestRefreshEvery_RunsNowThenTicksUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := make(chan struct{}, 16)
	done := make(chan struct{})
	go func() {
		refreshEvery(ctx, "test", 5*time.Millisecond, func(context.Context) error {
			select {
			case calls <- struct{}{}:
			default:
			}
			return errors.New("github down")
		})
		close(done)
	}()
	for range 3 {
		select {
		case <-calls:
		case <-time.After(5 * time.Second):
			t.Fatal("refresh did not run")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refreshEvery did not stop on cancel")
	}
}

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
