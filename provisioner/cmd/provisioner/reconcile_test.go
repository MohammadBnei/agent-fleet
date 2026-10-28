package main

import (
	"context"
	"sync"
	"testing"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/config"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/k8s"
)

// reconcileFake counts the loop's passes and holds one shared instance last used lastUsed ago.
type reconcileFake struct {
	mu       sync.Mutex
	lastUsed time.Time
	lists    int
	deleted  []string
}

func (f *reconcileFake) ListWorkerJobsByLabel(context.Context) ([]k8s.LiveWorkerJob, error) {
	return nil, nil
}
func (f *reconcileFake) DeleteWorkerJob(context.Context, string) error { return nil }
func (f *reconcileFake) ListSharedInstances(context.Context) ([]k8s.LiveSharedInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	return []k8s.LiveSharedInstance{{Repo: "r", ServiceKey: "postgres", LastUsedAt: f.lastUsed}}, nil
}
func (f *reconcileFake) DeleteSharedInstance(_ context.Context, repo, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, repo+"/"+key)
	return nil
}
func (f *reconcileFake) snapshot() (lists int, deleted []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists, append([]string(nil), f.deleted...)
}

type discardReporter struct{}

func (discardReporter) ReportEvent(context.Context, *agentfleetv1.PodEvent) {}

// startReconcile hands the Config's two durations to the loop: the interval is the tick (a short one passes often,
// a long one once in the test's window), and the idle timeout is what decides an instance is idle. The interval
// check shows the value reaches the loop and was neither swapped nor hardcoded; it does not pin that it arrives
// exactly (a pass count cannot tell 10ms from 30ms). The idle timeout is pinned from both sides of its edge.
func TestStartReconcile_UsesTheConfiguredDurations(t *testing.T) {
	run := func(interval, idle, age time.Duration) (lists int, deleted []string) {
		f := &reconcileFake{lastUsed: time.Now().Add(-age)}
		ctx, cancel := context.WithCancel(context.Background())
		startReconcile(ctx, config.Config{ReconcileInterval: interval, SharedInstanceIdleTimeout: idle}, f, discardReporter{}, nil)
		time.Sleep(300 * time.Millisecond)
		cancel()
		return f.snapshot()
	}

	// The interval: 10ms ticks pass many times in 300ms; a 1h tick passes once, at start.
	if lists, _ := run(10*time.Millisecond, time.Hour, time.Minute); lists < 5 {
		t.Errorf("a 10ms interval gave %d passes in 300ms, want several", lists)
	}
	if lists, _ := run(time.Hour, time.Hour, time.Minute); lists != 1 {
		t.Errorf("a 1h interval gave %d passes in 300ms, want the one at start", lists)
	}

	// The idle timeout, 60s: an instance last used 59s ago is kept, one used 61s ago is deleted.
	if _, deleted := run(time.Hour, 60*time.Second, 59*time.Second); len(deleted) != 0 {
		t.Errorf("60s idle timeout, used 59s ago: deleted %v, want none", deleted)
	}
	if _, deleted := run(time.Hour, 60*time.Second, 61*time.Second); len(deleted) != 1 || deleted[0] != "r/postgres" {
		t.Errorf("60s idle timeout, used 61s ago: deleted %v, want r/postgres", deleted)
	}
}
