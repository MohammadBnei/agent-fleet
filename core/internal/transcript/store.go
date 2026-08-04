// Package transcript replaces the Redis list (agentfleet:planning:<taskId>)
// as the durable store for the planner/human planning conversation. See
// docs/adr/0013: reads are pull/cursor-based (mirroring LRANGE from an
// index), never a bare streaming-watch RPC, so a client reconnect can't
// silently drop messages the way pub/sub could.
package transcript

import "context"

// Entry is one planning-transcript message, seq-ordered per task. JSON
// tags: also serialized directly by the dashboard API (internal/dashboard,
// see docs/adr/0014), the first direct-JSON consumer — the MCP server
// builds its own map[string]any instead of encoding this struct.
type Entry struct {
	Seq  int64  `json:"seq"`
	From string `json:"from"`
	Text string `json:"text"`
	Type string `json:"type"` // "" | "discussion" | "approve" | "abort" | "question" | "answer"
}

// Store is the durable, per-task append/read-since transcript.
type Store interface {
	// Append durably persists one entry and returns its assigned seq.
	// Retrying the same (taskID, idempotencyKey) returns the original seq
	// without appending twice.
	Append(ctx context.Context, taskID, from, text, msgType, idempotencyKey string) (seq int64, err error)

	// ReadSince mirrors LRANGE(key, sinceSeq, -1): every entry with
	// seq >= sinceSeq, in seq order, plus the next cursor to poll from.
	ReadSince(ctx context.Context, taskID string, sinceSeq int64, limit int) (entries []Entry, nextSeq int64, err error)
}
