package sessions

import "time"

// LiveState is a session's liveness — orthogonal to Session.Status, which is a
// workflow status (proposed/pending/claimed/running/done/...). A session can
// be `running` and blocked, or `running` and stalled; those are different
// questions and conflating them is why "is this one stuck?" previously had
// no answer short of opening each task and reading its transcript.
//
// Borrowed from herdr, which solves the same problem for terminal panes:
// every pane is marked working, blocked or idle, so a human never has to
// hunt for the one that stopped.
type LiveState string

const (
	// No live pod. The workflow status governs; there is nothing to be
	// live about.
	LiveStateNone LiveState = ""
	// A human decision is outstanding (a permission request, a plan or a
	// question) and the session is not working (DeriveLiveState).
	LiveStateBlocked LiveState = "blocked"
	// The pod is up but its agent has not posted anything at all yet.
	// Either it is still starting, or it never will — enforceStartupStall
	// is what decides which.
	LiveStateUnknown LiveState = "unknown"
	// Something was said to the agent and nothing has come back within
	// turnStall. Informational, never a teardown: a slow turn is not a
	// dead one, and the human can interrupt if they disagree.
	LiveStateStalled LiveState = "stalled"
	// The agent closed its turn and someone has looked since.
	LiveStateIdle LiveState = "idle"
	// The agent closed its turn while nobody was looking. herdr's
	// distinction, and the reason it exists: work that finished
	// unattended is the thing you actually want surfaced.
	LiveStateDone LiveState = "done"
	// Actively working.
	LiveStateWorking LiveState = "working"
)

// humanOriginatedEntry reports whether an entry type is one that hands the
// turn back to the agent — i.e. the agent now owes a response. Mirrors the
// set the dashboard's isCogitating heuristic used, which this replaces.
func humanOriginatedEntry(entryType, from string) bool {
	switch entryType {
	case "answer", "permission_response":
		return true
	case "discussion":
		// "session" is another session's prompt (docs/adr/0041) — the
		// target owes a reply to it exactly as it would to a human.
		return from == "human" || from == "session"
	}
	return false
}

// DeriveLiveState computes a session's liveness from one tasks row. Pure by
// design: every input is already maintained by the activityTrackingStore
// decorator on the same write that appends a transcript entry, so this
// cannot go stale the way a cached live_state column would, and it needs no
// second write path to keep correct.
//
// `now` is a parameter rather than a time.Now() call so the truth table in
// liveness_test.go is a table and not a set of sleeps.
func DeriveLiveState(t *Session, now time.Time, turnStall time.Duration) LiveState {
	if !IsPodPhaseLive(t.PodPhase) {
		return LiveStateNone
	}
	state := deriveTurnState(t, now, turnStall)
	// Blocked never overrides work (docs/adr/0082 §5). Nothing blocks any
	// more: past DECISION_WAIT_MS the agent hears "pending" and carries on,
	// so an open decision says the session is blocked only where it would
	// otherwise read as finished or stuck, or while its last word is the
	// request itself, which is still waiting on its answer (an older
	// worker's request waits there until answered).
	//
	// A count, not the old awaiting_human boolean. That flag was set and
	// cleared by ANY permission_response, but permissions are a list —
	// parallel tool calls each get their own seq — so answering one decision
	// reported the session unblocked while the others were still waiting,
	// and it sat there stalled with nobody notified. Counting the unanswered
	// ones cannot go wrong that way (docs/adr/0048).
	if t.PendingDecisions > 0 {
		switch state {
		case LiveStateIdle, LiveStateDone, LiveStateStalled:
			return LiveStateBlocked
		case LiveStateWorking:
			if t.LastEntryType != nil && (*t.LastEntryType == "permission_request" || *t.LastEntryType == "question") {
				return LiveStateBlocked
			}
		}
	}
	return state
}

// deriveTurnState is DeriveLiveState for a live pod, decisions aside.
func deriveTurnState(t *Session, now time.Time, turnStall time.Duration) LiveState {
	if !t.ActivitySeen {
		return LiveStateUnknown
	}

	lastType, lastFrom := "", ""
	if t.LastEntryType != nil {
		lastType = *t.LastEntryType
	}
	if t.LastEntryFrom != nil {
		lastFrom = *t.LastEntryFrom
	}

	if lastType == "result" {
		// Finished. Whether that counts as `done` depends only on whether
		// a human has opened it since it went quiet.
		if t.SeenAt == nil || (t.LastActiveAt != nil && t.SeenAt.Before(*t.LastActiveAt)) {
			return LiveStateDone
		}
		return LiveStateIdle
	}

	// A turn is in flight. It is stalled only if the agent owes a response
	// and hasn't produced one — an agent mid-work keeps appending entries
	// (tool_use, tool_result, its own text), so last_active_at moves on
	// its own and this never fires for a session that is genuinely busy.
	if humanOriginatedEntry(lastType, lastFrom) && t.LastActiveAt != nil && now.Sub(*t.LastActiveAt) > turnStall {
		return LiveStateStalled
	}
	return LiveStateWorking
}
