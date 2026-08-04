package dashboard

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/transcript"
)

func TestHub_BroadcastFanOut(t *testing.T) {
	hub := NewHub()
	ch1, cancel1 := hub.Subscribe("task-1", 0)
	defer cancel1()
	ch2, cancel2 := hub.Subscribe("task-1", 0)
	defer cancel2()

	hub.broadcast("task-1", Event{Seq: 1, From: "human", Text: "hi", Type: "discussion"})

	for i, ch := range []chan Event{ch1, ch2} {
		select {
		case e := <-ch:
			if e.Seq != 1 {
				t.Errorf("subscriber %d: seq = %d, want 1", i, e.Seq)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d: did not receive broadcast", i)
		}
	}
}

func TestHub_UnsubscribeCleansUpState(t *testing.T) {
	hub := NewHub()
	_, cancel := hub.Subscribe("task-1", 0)
	cancel()

	hub.mu.Lock()
	defer hub.mu.Unlock()
	if _, exists := hub.subs["task-1"]; exists {
		t.Error("subs still tracks task-1 after its only subscriber unsubscribed")
	}
	if _, exists := hub.seen["task-1"]; exists {
		t.Error("seen still tracks task-1 after its only subscriber unsubscribed")
	}
}

// fakeStore exercises pollOnce's fan-out/polling glue without a real
// Postgres — the new logic here is the hub's own bookkeeping, not
// transcript.Store's durability guarantees (already that store's own
// responsibility).
type fakeStore struct {
	mu      sync.Mutex
	entries []transcript.Entry
}

func (f *fakeStore) Append(context.Context, string, string, string, string, string) (int64, error) {
	panic("not used by pollOnce")
}

func (f *fakeStore) ReadSince(_ context.Context, _ string, sinceSeq int64, _ int) ([]transcript.Entry, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []transcript.Entry
	next := sinceSeq
	for _, e := range f.entries {
		if e.Seq >= sinceSeq {
			out = append(out, e)
			next = e.Seq + 1
		}
	}
	return out, next, nil
}

func TestHub_PollOnceOnlyPollsSubscribedTasks(t *testing.T) {
	store := &fakeStore{entries: []transcript.Entry{{Seq: 0, From: "human", Text: "hi", Type: "discussion"}}}
	hub := NewHub()
	ch, cancel := hub.Subscribe("task-1", 0)
	defer cancel()

	hub.pollOnce(context.Background(), store)

	select {
	case e := <-ch:
		if e.Text != "hi" {
			t.Errorf("text = %q, want %q", e.Text, "hi")
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive polled entry")
	}

	hub.mu.Lock()
	seen := hub.seen["task-1"]
	hub.mu.Unlock()
	if seen != 1 {
		t.Errorf("seen[task-1] = %d, want 1 (advanced past the delivered entry)", seen)
	}
}
