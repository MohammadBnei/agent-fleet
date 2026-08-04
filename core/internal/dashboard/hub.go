package dashboard

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// Hub fans out transcript updates to subscribed StreamTranscript RPC
// connections behind one shared poller, instead of giving each open
// browser tab its own DB poller — the untuned Postgres pool already
// serves the MCP wait_for_messages long-poll and the Discord relay loop
// (see docs/adr/0013, docs/adr/0015).
//
// Values are *agentfleetv1.TranscriptEntry pointers, not a local value
// type — generated protobuf structs are pointer-idiomatic and go vet's
// copylocks check flags value copies of them.
type Hub struct {
	mu   sync.Mutex
	subs map[string][]chan *agentfleetv1.TranscriptEntry
	seen map[string]int64 // last-broadcast seq per task, seeded by that task's first subscriber
}

func NewHub() *Hub {
	return &Hub{
		subs: make(map[string][]chan *agentfleetv1.TranscriptEntry),
		seen: make(map[string]int64),
	}
}

// Subscribe registers a new listener for taskID, starting from since (the
// caller's already-known cursor, typically from its initial GetTranscript
// call) if this is the task's first live subscriber.
//
// ponytail: a later subscriber joins the already-progressing stream rather
// than getting its own independent cursor — fine for a solo-operator tool
// where two concurrent viewers of one task's live feed isn't a real
// scenario; give each subscriber its own cursor if that ever changes.
func (h *Hub) Subscribe(taskID string, since int64) (ch chan *agentfleetv1.TranscriptEntry, cancel func()) {
	ch = make(chan *agentfleetv1.TranscriptEntry, 16)
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

func (h *Hub) broadcast(taskID string, e *agentfleetv1.TranscriptEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs[taskID] {
		select {
		case ch <- e:
		default: // slow subscriber — drop rather than block the shared poller
		}
	}
}

// PollLoop is the one poller behind every open stream: each tick it reads
// only tasks with a live subscriber, via transcript.Store.ReadSince — the
// same read path GetTranscript and worker/'s MCP long-poll already use.
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
			slog.Error("dashboard stream poll failed", "taskId", id, "error", err)
			continue
		}
		if len(entries) == 0 {
			continue
		}
		for _, e := range entries {
			h.broadcast(id, &agentfleetv1.TranscriptEntry{
				TaskId: id,
				Seq:    e.Seq,
				From:   e.From,
				Text:   e.Text,
				Type:   stringToProtoType(e.Type),
			})
		}
		h.mu.Lock()
		if _, stillSubscribed := h.seen[id]; stillSubscribed {
			h.seen[id] = next
		}
		h.mu.Unlock()
	}
}
