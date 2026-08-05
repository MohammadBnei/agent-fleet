//go:build integration

package transcript

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeNotifier struct {
	mu    sync.Mutex
	posts []Entry
}

func (f *fakeNotifier) PostToThread(_ context.Context, _ string, e Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, e)
	return nil
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

// TestRelay_NudgeTriggersImmediateRelay covers reliability-findings.md #5:
// a transcript write shouldn't wait up to pollInterval for the next tick
// to reach Discord — PostgresStore.SetNudge(relay.Nudge) should get it
// relayed right away. Run is started with an hour-long ticker so only the
// nudge path could plausibly pass within this test's timeout.
func TestRelay_NudgeTriggersImmediateRelay(t *testing.T) {
	pool := newTestPool(t)
	store := NewPostgresStore(pool)
	notifier := &fakeNotifier{}
	relay := NewRelay(pool, notifier)
	store.SetNudge(relay.Nudge)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go relay.Run(ctx, time.Hour)

	var taskID string
	if err := pool.QueryRow(ctx, `INSERT INTO tasks DEFAULT VALUES RETURNING id`).Scan(&taskID); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	if _, err := store.Append(ctx, taskID, "planner", "hello", "discussion", ""); err != nil {
		t.Fatalf("append: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for {
		if notifier.count() >= 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("expected the nudge to trigger an immediate relay within 5s, none observed")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
