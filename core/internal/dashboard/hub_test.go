package dashboard

import (
	"context"
	"sync"
	"testing"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

func TestHub_FanOut(t *testing.T) {
	store := &fakeStore{entries: []transcript.Entry{{Seq: 1, From: "human", Text: "hi", Type: "discussion"}}}
	hub := NewHub()
	ch1, cancel1 := hub.Subscribe("task-1", 0)
	defer cancel1()
	ch2, cancel2 := hub.Subscribe("task-1", 0)
	defer cancel2()
	hub.pollOnce(context.Background(), store)

	for i, ch := range []chan *agentfleetv1.TranscriptEntry{ch1, ch2} {
		select {
		case e := <-ch:
			if e.GetSeq() != 1 {
				t.Errorf("subscriber %d: seq = %d, want 1", i, e.GetSeq())
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d: did not receive the entry", i)
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
}

// A stream that reopens from an older cursor while another is already ahead
// still gets every entry after its own cursor: the missed ones are what the
// console only showed after a reload.
func TestHub_LateSubscriberKeepsItsOwnCursor(t *testing.T) {
	store := &fakeStore{entries: []transcript.Entry{
		{Seq: 1, Type: "assistant"}, {Seq: 2, Type: "interrupt"},
	}}
	hub := NewHub()
	ahead, cancelAhead := hub.Subscribe("task-1", 1)
	defer cancelAhead()
	hub.pollOnce(context.Background(), store)
	<-ahead
	<-ahead
	behind, cancelBehind := hub.Subscribe("task-1", 2)
	defer cancelBehind()
	hub.pollOnce(context.Background(), store)
	select {
	case e := <-behind:
		if e.GetSeq() != 2 {
			t.Errorf("late subscriber seq = %d, want 2", e.GetSeq())
		}
	case <-time.After(time.Second):
		t.Fatal("late subscriber missed the entry after its cursor")
	}
}

// A full subscriber is not skipped past the entry it could not take.
func TestHub_FullSubscriberLosesNothing(t *testing.T) {
	store := &fakeStore{}
	for i := int64(1); i <= 100; i++ {
		store.entries = append(store.entries, transcript.Entry{Seq: i, Type: "assistant"})
	}
	hub := NewHub()
	ch, cancel := hub.Subscribe("task-1", 1)
	defer cancel()
	got := int64(0)
	for range 10 {
		hub.pollOnce(context.Background(), store)
		for len(ch) > 0 {
			e := <-ch
			if e.GetSeq() != got+1 {
				t.Fatalf("seq %d after %d: an entry was skipped", e.GetSeq(), got)
			}
			got = e.GetSeq()
		}
	}
	if got != 100 {
		t.Errorf("received up to %d, want 100", got)
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

func (f *fakeStore) AppendReply(context.Context, string, string, string, string, string, int64) (int64, error) {
	panic("not used by pollOnce")
}

func (f *fakeStore) LatestSeq(context.Context, string) (int64, error) {
	panic("not used by pollOnce")
}

func (f *fakeStore) ReadSince(_ context.Context, _ string, sinceSeq int64, limit int) ([]transcript.Entry, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []transcript.Entry
	next := sinceSeq
	for _, e := range f.entries {
		if e.Seq >= sinceSeq && (limit <= 0 || len(out) < limit) {
			out = append(out, e)
			next = e.Seq + 1
		}
	}
	return out, next, nil
}

// readHook runs once at the start of the first read, so a test can act in
// the window between pollOnce choosing its read start and the read itself.
type readHook struct {
	*fakeStore
	before func()
}

func (h *readHook) ReadSince(ctx context.Context, id string, since int64, limit int) ([]transcript.Entry, int64, error) {
	if f := h.before; f != nil {
		h.before = nil
		f()
	}
	return h.fakeStore.ReadSince(ctx, id, since, limit)
}

func seqEntries(n int64) []transcript.Entry {
	var out []transcript.Entry
	for i := int64(1); i <= n; i++ {
		out = append(out, transcript.Entry{Seq: i, Type: "assistant"})
	}
	return out
}

// A subscriber that joins while a read is in flight, below its start, still
// gets every entry from its own cursor.
func TestHub_JoinDuringReadLosesNothing(t *testing.T) {
	st := &readHook{fakeStore: &fakeStore{entries: seqEntries(10)}}
	hub := NewHub()
	_, c1 := hub.Subscribe("s", 8)
	defer c1()
	var late chan *agentfleetv1.TranscriptEntry
	st.before = func() {
		var cancel func()
		late, cancel = hub.Subscribe("s", 3)
		t.Cleanup(cancel)
	}
	for range 3 {
		hub.pollOnce(context.Background(), st)
	}
	for want := int64(3); want <= 10; want++ {
		select {
		case e := <-late:
			if e.GetSeq() != want {
				t.Fatalf("late subscriber got seq %d, want %d", e.GetSeq(), want)
			}
		default:
			t.Fatalf("late subscriber missing seq %d", want)
		}
	}
}

// A subscriber that never reads must not hold back the others of the session.
func TestHub_StalledSubscriberDoesNotPinOthers(t *testing.T) {
	st := &fakeStore{entries: seqEntries(300)}
	hub := NewHub()
	_, c1 := hub.Subscribe("s", 1) // never read
	defer c1()
	fast, c2 := hub.Subscribe("s", 1)
	defer c2()
	var got int64
	for range 20 {
		hub.pollOnce(context.Background(), st)
		for len(fast) > 0 {
			e := <-fast
			if e.GetSeq() != got+1 {
				t.Fatalf("fast subscriber got seq %d after %d", e.GetSeq(), got)
			}
			got = e.GetSeq()
		}
	}
	if got != 300 {
		t.Errorf("fast subscriber reached %d of 300 behind a stalled one", got)
	}
}

func TestHub_PollOnceOnlyPollsSubscribedTasks(t *testing.T) {
	store := &fakeStore{entries: []transcript.Entry{{Seq: 0, From: "human", Text: "hi", Type: "discussion"}}}
	hub := NewHub()
	ch, cancel := hub.Subscribe("task-1", 0)
	defer cancel()

	hub.pollOnce(context.Background(), store)

	select {
	case e := <-ch:
		if e.GetText() != "hi" {
			t.Errorf("text = %q, want %q", e.GetText(), "hi")
		}
		if e.GetType() != agentfleetv1.TranscriptEntryType_TRANSCRIPT_ENTRY_TYPE_DISCUSSION {
			t.Errorf("type = %v, want DISCUSSION (string->enum mapping)", e.GetType())
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive polled entry")
	}
}

func (*fakeStore) AppendAuthored(context.Context, string, transcript.Author, string, string, string, *int64) (int64, error) {
	return 0, nil
}
