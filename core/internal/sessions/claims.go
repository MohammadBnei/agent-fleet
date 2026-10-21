package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Client requests (docs/adr/0066 §7): every Open and Invoke claims its
// idempotency key before any warm, and nothing is ever deleted.

// ErrBusy: another request holds the same key or thread. Never waited on:
// the caller answers Aborted and the client retries.
var ErrBusy = errors.New("request in progress, retry")

// Lock holds session-level advisory locks on one pooled connection until
// Release. A core crash drops the connection, which releases them.
type Lock struct {
	conn *pgxpool.Conn
	keys []int64
}

// lockKey hashes in Go: a key may hold any byte, NUL included, which a
// Postgres text parameter cannot.
func lockKey(k string) int64 {
	sum := sha256.Sum256([]byte(k))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// TryLock takes a pg_try_advisory_lock on each key, or none of them.
func (s *Store) TryLock(ctx context.Context, keys ...string) (*Lock, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim lock: %w", err)
	}
	l := &Lock{conn: conn}
	for _, name := range keys {
		k := lockKey(name)
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, k).Scan(&ok); err != nil || !ok {
			l.Release()
			if err != nil {
				return nil, fmt.Errorf("claim lock: %w", err)
			}
			return nil, ErrBusy
		}
		l.keys = append(l.keys, k)
	}
	return l, nil
}

// Release unlocks with a fresh context; if that fails the connection is
// closed rather than returned to the pool still holding a lock.
func (l *Lock) Release() {
	ctx := context.Background()
	for _, k := range l.keys {
		if _, err := l.conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, k); err != nil {
			slog.Error("claim unlock failed, closing the connection", "error", err)
			_ = l.conn.Conn().Close(ctx)
			break
		}
	}
	l.conn.Release()
}

// Claim is one client_requests row.
type Claim struct {
	SessionID  string // "" until a session is resolved
	RequestSHA string
	Done       bool
}

// ClaimRequest reads the claim, inserting it pending if new. Call it under
// the key's Lock.
func (s *Store) ClaimRequest(ctx context.Context, invoker, key, requestSHA string) (Claim, error) {
	var c Claim
	var sid *string
	var state string
	err := s.pool.QueryRow(ctx, `
		WITH ins AS (
		  INSERT INTO client_requests (invoker, idempotency_key, request_sha) VALUES ($1, $2, $3)
		  ON CONFLICT DO NOTHING RETURNING session_id, request_sha, state
		)
		SELECT session_id::text, request_sha, state FROM ins
		UNION ALL
		SELECT session_id::text, request_sha, state FROM client_requests
		 WHERE invoker = $1 AND idempotency_key = $2 AND NOT EXISTS (SELECT 1 FROM ins)`,
		invoker, key, requestSHA).Scan(&sid, &c.RequestSHA, &state)
	if err != nil {
		return Claim{}, fmt.Errorf("claim request: %w", err)
	}
	if sid != nil {
		c.SessionID = *sid
	}
	c.Done = state == "done"
	return c, nil
}

// PointClaim records the session a pending claim resolved to.
func (s *Store) PointClaim(ctx context.Context, invoker, key, sessionID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE client_requests SET session_id = $3 WHERE invoker = $1 AND idempotency_key = $2`, invoker, key, sessionID)
	if err != nil {
		return fmt.Errorf("point claim: %w", err)
	}
	return nil
}

// FinishClaim marks a claim done when its session already holds the entry
// with this key: an earlier attempt delivered, then crashed before marking
// it. It returns that entry's seq, or found=false.
func (s *Store) FinishClaim(ctx context.Context, invoker, key, sessionID string) (seq int64, found bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT seq FROM transcript WHERE session_id = $1 AND idempotency_key = $2`, sessionID, key).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("finish claim: %w", err)
	}
	_, err = s.pool.Exec(ctx, `UPDATE client_requests SET state = 'done' WHERE invoker = $1 AND idempotency_key = $2`, invoker, key)
	return seq, err == nil, err
}

// OpenThread is the open session of (invoker, definition, threadKey), or nil.
func (s *Store) OpenThread(ctx context.Context, invoker, definition, threadKey string) (*Session, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+selectCols+` FROM sessions s
		WHERE s.invoker = $1 AND s.definition_name = $2 AND s.thread_key = $3 AND s.archived_at IS NULL`,
		invoker, definition, threadKey)
	t, err := scanSession(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// ClaimedEntry is one client message, appended with the claim's key.
type ClaimedEntry struct {
	SessionID   string
	Text        string
	AuthorSub   string
	AuthorName  string
	Actor       string
	CausationID string
	ContextOnly bool
	Untrusted   bool
}

// AppendClaimed appends e with the claim's key as its transcript
// idempotency key and marks the claim done in the same transaction, so
// "delivered" and "claim done" never disagree (docs/adr/0066 §7 step 5).
// A key the session already holds returns its seq.
func (s *Store) AppendClaimed(ctx context.Context, invoker, key string, e ClaimedEntry) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("append claimed: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The transcript's own per-session lock (transcript.PostgresStore).
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, e.SessionID); err != nil {
		return 0, fmt.Errorf("append claimed: lock: %w", err)
	}
	var seq int64
	err = tx.QueryRow(ctx, `SELECT seq FROM transcript WHERE session_id = $1 AND idempotency_key = $2`, e.SessionID, key).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `
			INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key,
			                        author_sub, author_name, actor, causation_id, context_only, untrusted)
			SELECT $1, COALESCE(MAX(seq), -1) + 1, 'human', $2, 'discussion', $3,
			       NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''), $8, $9
			  FROM transcript WHERE session_id = $1
			RETURNING seq`,
			e.SessionID, e.Text, key, e.AuthorSub, e.AuthorName, e.Actor, e.CausationID, e.ContextOnly, e.Untrusted).Scan(&seq)
	}
	if err != nil {
		return 0, fmt.Errorf("append claimed: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE client_requests SET state = 'done', session_id = $3 WHERE invoker = $1 AND idempotency_key = $2`, invoker, key, e.SessionID); err != nil {
		return 0, fmt.Errorf("append claimed: mark done: %w", err)
	}
	return seq, tx.Commit(ctx)
}
