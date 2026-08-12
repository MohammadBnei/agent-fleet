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
// AnswerQuestion/Kill/...).
//
// Deliberately untyped by entry type for last_active_at — every append
// counts, not just human-authored ones. During active autonomous work the
// worker relays every SDK message as its own entry (tool_use, tool_result,
// assistant text), so a session that's genuinely busy keeps touching this
// on its own; only a session with nothing happening at all — agent idle,
// human silent — actually goes stale. Splitting "human activity" from
// "agent activity" here would make an actively-coding pod look idle just
// because the human hasn't typed anything in 20 minutes, which is exactly
// wrong.
//
// tasks.awaiting_human is the one exception that IS typed by entry type
// (see awaitHuman below) — unlike last_active_at, it must reflect a real
// pending-decision state, not "something happened recently".
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
func (s activityTrackingStore) touch(taskID, from, msgType string) {
	go func() {
		if err := s.tasks.TouchActive(context.Background(), taskID, from, msgType); err != nil {
			slog.Warn("activityTrackingStore: touch active failed", "taskId", taskID, "error", err)
		}
	}()
}

// awaitHuman sets/clears tasks.awaiting_human based on msgType — a
// permission_request/question opens the wait, its permission_response/
// answer resolves it, and abort/interrupt clear it too (worker/src/
// session.ts's resolveAllPendingDeny denies every pending permission
// in-memory the moment either arrives, but — confirmed live against a real
// kind-local worker — never posts a real permission_response row for it,
// so nothing stays waiting on a human who already moved the session on).
// Every other msgType is a no-op: not every transcript append is a
// decision point, unlike last_active_at above.
func (s activityTrackingStore) awaitHuman(taskID, msgType string) {
	var awaiting bool
	switch msgType {
	case "permission_request", "question":
		awaiting = true
	case "permission_response", "answer", "abort", "interrupt":
		awaiting = false
	default:
		return
	}
	go func() {
		if err := s.tasks.SetAwaitingHuman(context.Background(), taskID, awaiting); err != nil {
			slog.Warn("activityTrackingStore: set awaiting human failed", "taskId", taskID, "error", err)
		}
	}()
}

func (s activityTrackingStore) Append(ctx context.Context, taskID, from, text, msgType, idempotencyKey string) (int64, error) {
	seq, err := s.Store.Append(ctx, taskID, from, text, msgType, idempotencyKey)
	if err == nil {
		s.touch(taskID, from, msgType)
		s.awaitHuman(taskID, msgType)
	}
	return seq, err
}

func (s activityTrackingStore) AppendReply(ctx context.Context, taskID, from, text, msgType, idempotencyKey string, replyToSeq int64) (int64, error) {
	seq, err := s.Store.AppendReply(ctx, taskID, from, text, msgType, idempotencyKey, replyToSeq)
	if err == nil {
		s.touch(taskID, from, msgType)
		s.awaitHuman(taskID, msgType)
	}
	return seq, err
}
