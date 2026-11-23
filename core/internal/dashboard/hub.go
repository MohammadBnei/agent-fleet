package dashboard

import (
	"context"
	"log/slog"
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
// Every subscriber has its own cursor. A shared per-session cursor lost
// entries: a stream that reopened from the client's cursor while the old one
// was still being torn down joined at the other's position, and a full
// buffer dropped the entry for good. Either way the console never saw it
// until a reload (an interrupt, a stop, a burst of parallel tool results). A
// subscriber that cannot take an entry now is retried from that entry on the
// next tick, never skipped.
//
// Values are *agentfleetv1.TranscriptEntry pointers, not a local value
// type — generated protobuf structs are pointer-idiomatic and go vet's
// copylocks check flags value copies of them.
type Hub struct {
	mu   sync.Mutex
	subs map[string][]*subscriber
}

type subscriber struct {
	ch     chan *agentfleetv1.TranscriptEntry
	cursor int64 // the next seq this subscriber has not been given
}

func NewHub() *Hub {
	return &Hub{subs: make(map[string][]*subscriber)}
}

// Subscribe registers a listener for taskID that is sent every entry from
// since (the caller's cursor, typically from its initial GetTranscript call).
func (h *Hub) Subscribe(taskID string, since int64) (ch chan *agentfleetv1.TranscriptEntry, cancel func()) {
	sub := &subscriber{ch: make(chan *agentfleetv1.TranscriptEntry, 64), cursor: since}
	h.mu.Lock()
	h.subs[taskID] = append(h.subs[taskID], sub)
	h.mu.Unlock()

	return sub.ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		list := h.subs[taskID]
		for i, c := range list {
			if c == sub {
				h.subs[taskID] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(h.subs[taskID]) == 0 {
			delete(h.subs, taskID)
		}
		close(sub.ch)
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
	lowest := make(map[string]int64, len(h.subs))
	for id, list := range h.subs {
		for _, s := range list {
			// A subscriber with a full channel cannot take anything now; it
			// must not pin the read window for the ones that can.
			if len(s.ch) >= cap(s.ch) {
				continue
			}
			if cur, ok := lowest[id]; !ok || s.cursor < cur {
				lowest[id] = s.cursor
			}
		}
	}
	h.mu.Unlock()

	for id, since := range lowest {
		entries, _, err := store.ReadSince(ctx, id, since, 100)
		if err != nil {
			slog.Error("dashboard stream poll failed", "sessionId", id, "error", err)
			continue
		}
		if len(entries) == 0 {
			continue
		}
		h.mu.Lock()
		for _, s := range h.subs[id] {
			// Joined (or fell behind) below what was read: the next tick reads
			// from its cursor, so it is never jumped over a gap.
			if s.cursor < since {
				continue
			}
			for _, e := range entries {
				if e.Seq < s.cursor {
					continue
				}
				select {
				case s.ch <- entryToProto(id, e):
					s.cursor = e.Seq + 1
				default:
					// A slow subscriber keeps its cursor on this entry and
					// gets it on the next tick.
				}
				if s.cursor <= e.Seq {
					break
				}
			}
		}
		h.mu.Unlock()
	}
}
