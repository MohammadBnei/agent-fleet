package dashboard

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/MohammadBnei/agent-fleet/fleet-core/internal/transcript"
)

// Event is one transcript update pushed to a task's SSE subscribers.
type Event struct {
	Seq  int64  `json:"seq"`
	From string `json:"from"`
	Text string `json:"text"`
	Type string `json:"type"`
}

// Hub fans out transcript updates to subscribed SSE connections behind one
// shared poller, instead of giving each open browser tab its own DB
// poller — the untuned Postgres pool already serves the MCP
// wait_for_messages long-poll and the Discord RelayLoop (see
// docs/adr/0013).
type Hub struct {
	mu   sync.Mutex
	subs map[string][]chan Event
	seen map[string]int64 // last-broadcast seq per task, seeded by that task's first subscriber
}

func NewHub() *Hub {
	return &Hub{subs: make(map[string][]chan Event), seen: make(map[string]int64)}
}

// Subscribe registers a new SSE listener for taskID, starting from since
// (the caller's already-known cursor, typically from its initial
// GET .../transcript fetch) if this is the task's first live subscriber.
//
// ponytail: a later subscriber joins the already-progressing stream rather
// than getting its own independent cursor — fine for a solo-operator tool
// where two concurrent viewers of one task's live feed isn't a real
// scenario; give each subscriber its own cursor if that ever changes.
func (h *Hub) Subscribe(taskID string, since int64) (ch chan Event, cancel func()) {
	ch = make(chan Event, 16)
	h.mu.Lock()
	if _, exists := h.seen[taskID]; !exists {
		h.seen[taskID] = since
	}
	h.subs[taskID] = append(h.subs[taskID], ch)
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		list := h.subs[taskID]
		for i, c := range list {
			if c == ch {
				h.subs[taskID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(h.subs[taskID]) == 0 {
			delete(h.subs, taskID)
			delete(h.seen, taskID)
		}
		close(ch)
	}
}

func (h *Hub) broadcast(taskID string, e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs[taskID] {
		select {
		case ch <- e:
		default: // slow subscriber — drop rather than block the shared poller
		}
	}
}

// PollLoop is the one poller behind every open SSE connection: each tick it
// reads only tasks with a live subscriber, via transcript.Store.ReadSince —
// the same read path the REST transcript endpoint and worker/'s MCP
// long-poll already use.
func (h *Hub) PollLoop(ctx context.Context, store transcript.Store, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.pollOnce(ctx, store)
		}
	}
}

func (h *Hub) pollOnce(ctx context.Context, store transcript.Store) {
	h.mu.Lock()
	pending := make(map[string]int64, len(h.seen))
	maps.Copy(pending, h.seen)
	h.mu.Unlock()

	for id, since := range pending {
		entries, next, err := store.ReadSince(ctx, id, since, 100)
		if err != nil {
			slog.Error("dashboard sse poll failed", "taskId", id, "error", err)
			continue
		}
		if len(entries) == 0 {
			continue
		}
		for _, e := range entries {
			h.broadcast(id, Event{Seq: e.Seq, From: e.From, Text: e.Text, Type: e.Type})
		}
		h.mu.Lock()
		if _, stillSubscribed := h.seen[id]; stillSubscribed {
			h.seen[id] = next
		}
		h.mu.Unlock()
	}
}
