package main

import (
	"context"
	"log/slog"

	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// activityTrackingStore wraps transcript.Store, bumping tasks.last_active_at
// on every real Append/AppendReply — the idle-timeout backstop's activity
// signal (sessions redesign, supersedes docs/adr/0021/0025's phase-boundary
// framing). One choke point at construction time beats patching every RPC
// call site that appends (coreserver's SendMessage/AskUserQuestion/
// SetPermissionMode/ReportPodEvents, dashboard's Discuss/RespondToPermission/
// AnswerQuestion/Stop/...).
//
// Deliberately untyped by entry type — every append counts, not just
// human-authored ones. During active autonomous work the worker relays
// every SDK message as its own entry (tool_use, tool_result, assistant
// text), so a session that's genuinely busy keeps touching this on its
// own; only a session with nothing happening at all — agent idle, human
// silent — actually goes stale. Splitting "human activity" from "agent
// activity" here would make an actively-coding pod look idle just because
// the human hasn't typed anything in 20 minutes, which is exactly wrong.
type activityTrackingStore struct {
	transcript.Store
	tasks *tasks.Store
}

func newActivityTrackingStore(store transcript.Store, taskStore *tasks.Store) activityTrackingStore {
	return activityTrackingStore{Store: store, tasks: taskStore}
}

// touch is best-effort and fire-and-forget — a transcript append must
// never be slowed down or failed by the activity-tracking side effect
// (matches tasks.Store.TouchActive's own best-effort framing).
func (s activityTrackingStore) touch(taskID string) {
	go func() {
		if err := s.tasks.TouchActive(context.Background(), taskID); err != nil {
			slog.Warn("activityTrackingStore: touch active failed", "taskId", taskID, "error", err)
		}
	}()
}

func (s activityTrackingStore) Append(ctx context.Context, taskID, from, text, msgType, idempotencyKey string) (int64, error) {
	seq, err := s.Store.Append(ctx, taskID, from, text, msgType, idempotencyKey)
	if err == nil {
		s.touch(taskID)
	}
	return seq, err
}

func (s activityTrackingStore) AppendReply(ctx context.Context, taskID, from, text, msgType, idempotencyKey string, replyToSeq int64) (int64, error) {
	seq, err := s.Store.AppendReply(ctx, taskID, from, text, msgType, idempotencyKey, replyToSeq)
	if err == nil {
		s.touch(taskID)
	}
	return seq, err
}
