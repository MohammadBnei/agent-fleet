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

func (f *fakeNotifier) snapshot() []Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Entry, len(f.posts))
	copy(out, f.posts)
	return out
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

// TestRelay_AllowlistOnlyRelaysDiscordSafeTypes covers reliability-
// findings.md #0: relayPending flipped from a denylist of exactly one
// type (tool_call) to an allowlist — logSdkMessage now tags raw SDK
// message types (system/assistant/user/result) that must never reach
// Discord verbatim (tool input, tool output, session metadata), and a
// denylist would silently forward any *new* type nobody remembered to
// exclude. This fires every type the fleet actually produces and checks
// only the allowlisted ones post.
func TestRelay_AllowlistOnlyRelaysDiscordSafeTypes(t *testing.T) {
	pool := newTestPool(t)
	store := NewPostgresStore(pool)
	notifier := &fakeNotifier{}
	ctx := context.Background()

	var taskID string
	if err := pool.QueryRow(ctx, `INSERT INTO tasks DEFAULT VALUES RETURNING id`).Scan(&taskID); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	discordSafe := []string{"discussion", "approve", "abort", "question", "answer"}
	discordUnsafe := []string{"system", "assistant", "user", "result", "tool_call"}
	for _, ty := range append(append([]string{}, discordSafe...), discordUnsafe...) {
		if _, err := store.Append(ctx, taskID, "planner", "msg-"+ty, ty, ""); err != nil {
			t.Fatalf("append %s: %v", ty, err)
		}
	}

	relayPending(ctx, pool, notifier)

	posted := make(map[string]bool)
	for _, e := range notifier.snapshot() {
		posted[e.Type] = true
	}
	for _, ty := range discordSafe {
		if !posted[ty] {
			t.Errorf("expected allowlisted type %q to reach Discord, it didn't", ty)
		}
	}
	for _, ty := range discordUnsafe {
		if posted[ty] {
			t.Errorf("expected non-allowlisted type %q NOT to reach Discord, but it did", ty)
		}
	}

	// Every entry, allowlisted or not, must still end up relayed_to_discord
	// = true — the non-safe ones are marked relayed without ever posting,
	// not left pending forever (which would make relayPending re-scan them
	// on every future tick).
	var pendingCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM planning_transcript WHERE task_id = $1 AND relayed_to_discord = false
	`, taskID).Scan(&pendingCount); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pendingCount != 0 {
		t.Fatalf("expected all %d entries marked relayed, %d still pending", len(discordSafe)+len(discordUnsafe), pendingCount)
	}
}
