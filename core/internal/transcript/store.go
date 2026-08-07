// Package transcript replaces the Redis list (agentfleet:planning:<taskId>)
// as the durable store for the agent/human conversation. See
// docs/adr/0013: reads are pull/cursor-based (mirroring LRANGE from an
// index), never a bare streaming-watch RPC, so a client reconnect can't
// silently drop messages the way pub/sub could.
package transcript

import "context"

// Entry is one transcript message, seq-ordered per task. JSON
// tags: also serialized directly by the dashboard API (internal/dashboard,
// see docs/adr/0014), the first direct-JSON consumer — the MCP server
// builds its own map[string]any instead of encoding this struct.
type Entry struct {
	Seq  int64  `json:"seq"`
	From string `json:"from"`
	Text string `json:"text"`
	Type string `json:"type"` // "" | "discussion" | "approve" | "abort" | "question" | "answer"
	// ReplyTo is non-nil only for an "answer" entry replying to a specific
	// "question" entry's own seq (reliability-findings.md #0's question-seq
	// correlation) — without it, "any pending question + any reply"
	// (today's actual behavior) lets an unrelated message satisfy a
	// blocked AskUserQuestion call.
	ReplyTo *int64 `json:"replyTo,omitempty"`
}

// Store is the durable, per-task append/read-since transcript.
type Store interface {
	// Append durably persists one entry and returns its assigned seq.
	// Retrying the same (taskID, idempotencyKey) returns the original seq
	// without appending twice.
	Append(ctx context.Context, taskID, from, text, msgType, idempotencyKey string) (seq int64, err error)

	// AppendReply is Append plus reply-to-seq correlation — a second
	// method rather than a signature change to Append, so the ~6 existing
	// call sites that never need this don't all have to pass a meaningless
	// zero value.
	AppendReply(ctx context.Context, taskID, from, text, msgType, idempotencyKey string, replyToSeq int64) (seq int64, err error)

	// ReadSince mirrors LRANGE(key, sinceSeq, -1): every entry with
	// seq >= sinceSeq, in seq order, plus the next cursor to poll from.
	ReadSince(ctx context.Context, taskID string, sinceSeq int64, limit int) (entries []Entry, nextSeq int64, err error)
}
