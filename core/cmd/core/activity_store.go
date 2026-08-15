package main

import (
	"context"
	"log/slog"

	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
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
	tasks *sessions.Store
}

func newActivityTrackingStore(store transcript.Store, sessionStore *sessions.Store) activityTrackingStore {
	return activityTrackingStore{Store: store, tasks: sessionStore}
}

// touch is best-effort and fire-and-forget — a transcript append must
// never be slowed down or failed by the activity-tracking side effect
// (matches sessions.Store.TouchActive's own best-effort framing).
func (s activityTrackingStore) touch(taskID, from, msgType string) {
	go func() {
		if err := s.tasks.TouchActive(context.Background(), taskID, from, msgType); err != nil {
			slog.Warn("activityTrackingStore: touch active failed", "sessionId", taskID, "error", err)
		}
	}()
}

// awaitHuman sets/clears tasks.awaiting_human based on msgType — a
// permission_request/question opens the wait, its permission_response/
// answer resolves it, and abort/interrupt clear it too.
//
// abort/interrupt stay in the clearing list because those two really do
// deny everything pending at that moment and leave their own entry instead
// of a permission_response. The other case they used to cover — a plain
// human reply, which worker/src/session.ts's resolveAllPendingDeny also
// treats as a denial — no longer needs covering here: that sweep now posts
// a real permission_response row (session.ts's recordResolution), which
// lands on AppendReply below and clears the wait through the normal path.
// Before it did, that resolution existed only in the worker's memory, so
// the request stayed "pending" on every dashboard surface and the plan card
// could only be dismissed by approving it.
//
// Every other msgType is a no-op: not every transcript append is a
// decision point, unlike last_active_at above.
// awaitHuman used to flip an awaiting_human boolean on every decision-shaped
// append. It is deleted in docs/adr/0048 because the column could not be
// correct: permissions are a LIST — parallel tool calls each get their own
// seq — but any single permission_response set and cleared the one flag, so
// answering one decision marked the session unblocked while others were still
// waiting, and it sat stalled with nobody notified.
//
// Replaced by a counted subquery over unanswered questions and permission
// requests (sessions.selectCols), which cannot go wrong that way and needs no
// second write path to keep in sync.

func (s activityTrackingStore) Append(ctx context.Context, taskID, from, text, msgType, idempotencyKey string) (int64, error) {
	seq, err := s.Store.Append(ctx, taskID, from, text, msgType, idempotencyKey)
	if err == nil {
		s.touch(taskID, from, msgType)
	}
	return seq, err
}

func (s activityTrackingStore) AppendReply(ctx context.Context, taskID, from, text, msgType, idempotencyKey string, replyToSeq int64) (int64, error) {
	seq, err := s.Store.AppendReply(ctx, taskID, from, text, msgType, idempotencyKey, replyToSeq)
	if err == nil {
		s.touch(taskID, from, msgType)
	}
	return seq, err
}
