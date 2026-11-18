package transcript

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// MainThread is the owner's admin thread, and the thread of every entry of a row that is not a standing character.
const MainThread = "main"

// ErrBadThread is an append naming a malformed thread key, or a thread other than main on a row that is not a
// standing character (docs/adr/0084 §4).
var ErrBadThread = errors.New("bad thread")

var threadKey = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidThread is the key's shape (the session_threads CHECK).
func ValidThread(key string) bool { return threadKey.MatchString(key) }

type threadCtx struct{}

// WithThread names the thread the appends under ctx belong to. A context naming none appends to main, so the
// ~12 call sites that never see a thread keep their signature.
func WithThread(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, threadCtx{}, key)
}

type contextOnlyCtx struct{}

// WithContextOnly makes the appends under ctx context-only: stored and shown, never delivered to the agent as input
// (docs/adr/0067 §3).
func WithContextOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, contextOnlyCtx{}, true)
}

// ThreadFrom is the thread WithThread named, else main.
func ThreadFrom(ctx context.Context) string {
	k, _ := threadOf(ctx)
	return k
}

func threadOf(ctx context.Context) (key string, named bool) {
	if k, ok := ctx.Value(threadCtx{}).(string); ok && k != "" {
		return k, true
	}
	return MainThread, false
}

// noteThread keeps session_threads in step with one appended entry, inside the append's transaction:
//   - an owner or peer message opens its thread (or reopens a parked one, from this entry, keeping its SDK
//     session id: docs/adr/0084 amendment A4), and stamps the thread active (§4);
//   - a result advances the thread's cursor past its consumedSeq, so a re-warm resumes from the last result
//     and never from LatestSeq (§4). A crash result written by core does the same.
//
// A thread other than main on a row that is not standing is refused. Main on such a row touches nothing: the
// UPDATE below matches no row.
func noteThread(ctx context.Context, tx pgx.Tx, sessionID string, e Entry) error {
	if e.ThreadKey == MainThread {
		// A standing row always has its main thread, whatever made the row: one an older core created after the
		// migration's backfill gets it from its first entry. StreamHumanMessages drops entries of a thread it
		// has no row for.
		if _, err := tx.Exec(ctx, `INSERT INTO session_threads (session_id, thread_key, cursor)
			SELECT id, $2, $3 FROM sessions WHERE id = $1 AND lifecycle = 'standing' ON CONFLICT DO NOTHING`,
			sessionID, MainThread, e.Seq); err != nil {
			return fmt.Errorf("main thread: %w", err)
		}
	}
	if e.ThreadKey != MainThread {
		var standing bool
		if err := tx.QueryRow(ctx, `SELECT lifecycle IS NOT DISTINCT FROM 'standing' FROM sessions WHERE id = $1`, sessionID).Scan(&standing); err != nil {
			return fmt.Errorf("thread check: %w", err)
		}
		if !standing {
			return ErrBadThread
		}
	}
	switch {
	case e.Type == "result":
		var r struct {
			ConsumedSeq *float64 `json:"consumedSeq"`
		}
		if json.Unmarshal([]byte(e.Text), &r) != nil || r.ConsumedSeq == nil {
			return nil
		}
		_, err := tx.Exec(ctx, `UPDATE session_threads SET cursor = GREATEST(cursor, $3) WHERE session_id = $1 AND thread_key = $2`,
			sessionID, e.ThreadKey, int64(*r.ConsumedSeq)+1)
		return err
	case e.ThreadKey != MainThread && (e.From == "human" || e.From == "session") && (e.Type == "" || e.Type == "discussion"):
		_, err := tx.Exec(ctx, `
			INSERT INTO session_threads (session_id, thread_key, cursor) VALUES ($1, $2, $3)
			ON CONFLICT (session_id, thread_key) DO UPDATE SET
			  last_active_at = now(),
			  cursor = CASE WHEN session_threads.closed_at IS NULL THEN session_threads.cursor ELSE EXCLUDED.cursor END,
			  effect_seq = CASE WHEN session_threads.closed_at IS NULL THEN session_threads.effect_seq END,
			  crash_seq = CASE WHEN session_threads.closed_at IS NULL THEN session_threads.crash_seq END,
			  crashes = CASE WHEN session_threads.closed_at IS NULL THEN session_threads.crashes ELSE 0 END,
			  crash_agent_seq = CASE WHEN session_threads.closed_at IS NULL THEN session_threads.crash_agent_seq END,
			  closed_at = NULL`, sessionID, e.ThreadKey, e.Seq)
		return err
	}
	return nil
}
