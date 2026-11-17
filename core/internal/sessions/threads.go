package sessions

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// MainThread is the owner's admin thread: it never closes, and keeps its SDK session id in
// sessions.agent_session_id (docs/adr/0084 §4).
const MainThread = "main"

// PoisonCrashes is how many times in a row a thread's head entry may crash its turn before core marks it done
// (docs/adr/0084 §4).
const PoisonCrashes = 3

// Thread is one thread of a standing character (migration 000031).
type Thread struct {
	Key string
	// Cursor is the first seq not yet done in the thread.
	Cursor         int64
	AgentSessionID string
	Closed         bool
}

// Threads lists every thread row of a standing session, closed ones included.
func (s *Store) Threads(ctx context.Context, sessionID string) ([]Thread, error) {
	rows, err := s.pool.Query(ctx, `SELECT thread_key, cursor, COALESCE(agent_session_id, ''), closed_at IS NOT NULL
		FROM session_threads WHERE session_id = $1 ORDER BY opened_at, thread_key`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("threads: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (t Thread, err error) {
		err = r.Scan(&t.Key, &t.Cursor, &t.AgentSessionID, &t.Closed)
		return
	})
}

// ResumeFrom is where a standing character's new pod starts streaming (docs/adr/0084 §4): the lowest
// turn-starting entry at or above its own thread's cursor, over every open thread. The cursor is each
// thread's last result, never LatestSeq, so an entry appended while the pod was down is delivered after
// the re-warm. ok is false with nothing waiting: LatestSeq stands.
func (s *Store) ResumeFrom(ctx context.Context, sessionID string) (fromSeq int64, ok bool, err error) {
	var seq *int64
	err = s.pool.QueryRow(ctx, `SELECT MIN(h.seq) FROM session_threads st
		JOIN transcript h ON h.session_id = st.session_id AND h.thread_key = st.thread_key
		WHERE st.session_id = $1 AND st.closed_at IS NULL AND h.seq >= st.cursor AND `+turnStarting, sessionID).Scan(&seq)
	if err != nil {
		return 0, false, fmt.Errorf("resume from: %w", err)
	}
	if seq == nil {
		return 0, false, nil
	}
	return *seq, true, nil
}

// CloseThread is the owner's close (docs/adr/0084 §4): the SDK session id is dropped, the transcript stays.
// False when the thread is main (never closes), unknown or already closed.
func (s *Store) CloseThread(ctx context.Context, sessionID, key string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE session_threads SET closed_at = now(), agent_session_id = NULL
		WHERE session_id = $1 AND thread_key = $2 AND thread_key <> $3 AND closed_at IS NULL`, sessionID, key, MainThread)
	if err != nil {
		return false, fmt.Errorf("close thread: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// IdleThread is one open thread IdleThreads found.
type IdleThread struct{ SessionID, Key string }

// IdleThreads lists every open non-main thread whose last owner message is older than idle: 7 days
// (docs/adr/0084 §4). The caller sends the thread's abort entry first and closes it after (CloseThread), so a
// failed append leaves the thread open for the next sweep rather than closed under a live SDK session.
func (s *Store) IdleThreads(ctx context.Context, idle time.Duration) ([]IdleThread, error) {
	rows, err := s.pool.Query(ctx, `SELECT session_id::text, thread_key FROM session_threads
		WHERE closed_at IS NULL AND thread_key <> $1 AND last_active_at < now() - make_interval(secs => $2)`, MainThread, idle.Seconds())
	if err != nil {
		return nil, fmt.Errorf("idle threads: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (c IdleThread, err error) {
		err = r.Scan(&c.SessionID, &c.Key)
		return
	})
}

// SaveThreadSessionID is SaveAgentSessionID for a thread other than main, under the same lease rule. An
// empty id clears it. False for a stale lease, or a thread that is unknown or closed.
func (s *Store) SaveThreadSessionID(ctx context.Context, id, key, agentSessionID, leaseID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE session_threads SET agent_session_id = NULLIF($4, '')
		WHERE session_id = $1 AND thread_key = $5 AND closed_at IS NULL
		AND EXISTS (SELECT 1 FROM sessions WHERE id = $1 AND `+leaseMatch+`)`, id, leaseID, livePhases, agentSessionID, key)
	if err != nil {
		return false, fmt.Errorf("save thread session id: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// NoteEffect records that an effect method ran for entry seq of a thread (docs/adr/0084 §4): after a crash
// that entry is done, never replayed.
func (s *Store) NoteEffect(ctx context.Context, id, key string, seq int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE session_threads SET effect_seq = GREATEST(COALESCE(effect_seq, -1), $3)
		WHERE session_id = $1 AND thread_key = $2`, id, key, seq)
	if err != nil {
		return fmt.Errorf("note effect: %w", err)
	}
	return nil
}

// Settled is a thread's head entry a crash made done.
type Settled struct {
	Key    string
	Seq    int64
	Reason string // "effect" or "poison"
}

// runningThreads selects the open threads of session $1 with a turn running: the head entry (the first
// turn-starting one at or above the cursor) has agent output above it and above the thread's last result. top
// is that output's highest seq.
const runningThreads = `SELECT st.thread_key, head.seq, COALESCE(st.effect_seq, -1), COALESCE(st.crash_seq, -1), st.crashes, top.seq
		FROM session_threads st
		CROSS JOIN LATERAL (SELECT MIN(h.seq) AS seq FROM transcript h WHERE h.session_id = st.session_id
		                      AND h.thread_key = st.thread_key AND h.seq >= st.cursor AND ` + turnStarting + `) head
		CROSS JOIN LATERAL (SELECT MAX(t.seq) AS seq FROM transcript t WHERE t.session_id = st.session_id
		              AND t.thread_key = st.thread_key AND t."from" = 'agent' AND t.seq > head.seq
		              AND t.seq > COALESCE((SELECT MAX(r.seq) FROM transcript r WHERE r.session_id = st.session_id
		                                      AND r.thread_key = st.thread_key AND r.type = 'result'), -1)) top
		WHERE st.session_id = $1 AND st.closed_at IS NULL AND head.seq IS NOT NULL AND top.seq IS NOT NULL`

// RunningThreads names the open threads of a standing session with a turn running (docs/adr/0084 §3): a re-pin
// drain waits for them.
func (s *Store) RunningThreads(ctx context.Context, id string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT q.thread_key FROM (`+runningThreads+`) q`, id)
	if err != nil {
		return nil, fmt.Errorf("running threads: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// SettleCrash runs once per CRASHED pod of a standing session (docs/adr/0084 §4). A thread whose head entry
// (the first turn-starting one at or above its cursor) had a turn running, shown by agent output above it and
// above the thread's last result, has crashed on that entry. A crash counts once per attempt: agent output
// above the one the last settle saw. A report with none (the same crash seen twice, a re-warm that failed
// before the pod ran) changes nothing. The second result names the threads with a turn running on this attempt, for
// their crash entries. The entry is settled, for the caller to close
// with a crash result, when an effect of it ran (replaying would run it twice) or it has now crashed
// PoisonCrashes times in a row. Any other crash leaves it for the re-warm.
func (s *Store) SettleCrash(ctx context.Context, id string) ([]Settled, []string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("settle crash: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	rows, err := tx.Query(ctx, runningThreads+` AND top.seq > COALESCE(st.crash_agent_seq, -1)`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("settle crash: %w", err)
	}
	type head struct {
		key                           string
		seq, effect, crashSeq, topSeq int64
		crashes                       int
	}
	heads, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (h head, err error) {
		err = r.Scan(&h.key, &h.seq, &h.effect, &h.crashSeq, &h.crashes, &h.topSeq)
		return
	})
	if err != nil {
		return nil, nil, fmt.Errorf("settle crash: %w", err)
	}
	var out []Settled
	var running []string
	for _, h := range heads {
		running = append(running, h.key)
		crashes := 1
		if h.crashSeq == h.seq {
			crashes = h.crashes + 1
		}
		if _, err := tx.Exec(ctx, `UPDATE session_threads SET crash_seq = $3, crashes = $4, crash_agent_seq = $5 WHERE session_id = $1 AND thread_key = $2`,
			id, h.key, h.seq, crashes, h.topSeq); err != nil {
			return nil, nil, fmt.Errorf("settle crash: %w", err)
		}
		switch {
		case h.effect >= h.seq:
			out = append(out, Settled{h.key, h.seq, "effect"})
		case crashes >= PoisonCrashes:
			out = append(out, Settled{h.key, h.seq, "poison"})
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("settle crash: %w", err)
	}
	return out, running, nil
}

// EffectKey is the idempotency key every effect method carries in a standing character's turn (docs/adr/0084
// §4): the session, the thread, the entry the turn answers and the call's ordinal within the turn.
func EffectKey(sessionID, thread string, entrySeq int64, ordinal int) string {
	return fmt.Sprintf("eff:%s:%s:%d:%d", sessionID, thread, entrySeq, ordinal)
}

// ParseEffectKey reads a key EffectKey made for sessionID: the thread and entry seq. ok is false for any
// other key, or one made for another session.
func ParseEffectKey(sessionID, key string) (thread string, entrySeq int64, ok bool) {
	rest, found := strings.CutPrefix(key, "eff:"+sessionID+":")
	if !found {
		return "", 0, false
	}
	// A thread key has no ':', so the rest is "<thread>:<seq>:<ordinal>".
	var ord int
	parts := strings.Split(rest, ":")
	if len(parts) != 3 {
		return "", 0, false
	}
	if _, err := fmt.Sscanf(parts[1]+" "+parts[2], "%d %d", &entrySeq, &ord); err != nil {
		return "", 0, false
	}
	return parts[0], entrySeq, true
}
