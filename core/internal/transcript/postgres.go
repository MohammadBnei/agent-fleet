package transcript

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the Store implementation backing transcript
// (db/migrations/). Concurrent appends to the same task (a human's Discord
// reply landing alongside the agent's own send_message call) are
// serialized via a per-task advisory lock so seq assignment can't race.
type PostgresStore struct {
	pool  *pgxpool.Pool
	nudge func() // optional; set via SetNudge
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// SetNudge wires an immediate-relay trigger (reliability-findings.md #5) —
// called once at startup rather than threaded through Append's ~6 call
// sites individually. A nil nudge (the zero value, before SetNudge is
// called) is a valid no-op.
func (s *PostgresStore) SetNudge(nudge func()) {
	s.nudge = nudge
}

func (s *PostgresStore) Append(ctx context.Context, taskID, from, text, msgType, idempotencyKey string) (int64, error) {
	return s.appendInternal(ctx, taskID, from, text, msgType, idempotencyKey, nil)
}

// AppendReply is Append plus reply-to-seq correlation (reliability-
// findings.md #0) — see Store interface's own doc comment for why this is
// a second method, not a signature change to Append.
func (s *PostgresStore) AppendReply(ctx context.Context, taskID, from, text, msgType, idempotencyKey string, replyToSeq int64) (int64, error) {
	return s.appendInternal(ctx, taskID, from, text, msgType, idempotencyKey, &replyToSeq)
}

func (s *PostgresStore) appendInternal(ctx context.Context, taskID, from, text, msgType, idempotencyKey string, replyToSeq *int64) (int64, error) {
	if idempotencyKey == "" {
		// The dedup guarantee lives here, not in each caller — an empty key
		// must never reach the query below as a literal value: every
		// concurrent caller passing "" would collide on the same row and
		// silently share one seq (caught by
		// TestPostgresStore_ConcurrentAppendsAreGaplessAndOrdered). A fresh
		// UUID per call means "no key supplied" behaves as "never
		// deduplicated", which is the correct behavior for a caller that
		// didn't ask for idempotency.
		idempotencyKey = uuid.NewString()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, taskID); err != nil {
		return 0, fmt.Errorf("advisory lock: %w", err)
	}

	// Idempotency: a retried append with the same key returns the
	// already-assigned seq instead of appending twice.
	var existingSeq int64
	err = tx.QueryRow(ctx,
		`SELECT seq FROM transcript WHERE task_id = $1 AND idempotency_key = $2`,
		taskID, idempotencyKey,
	).Scan(&existingSeq)
	if err == nil {
		return existingSeq, tx.Commit(ctx)
	}
	if err != pgx.ErrNoRows {
		return 0, fmt.Errorf("check idempotency: %w", err)
	}

	var seq int64
	var msgTypePtr *string
	if msgType != "" {
		msgTypePtr = &msgType
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO transcript (task_id, seq, "from", text, type, idempotency_key, reply_to_seq)
		SELECT $1, COALESCE(MAX(seq), -1) + 1, $2, $3, $4, $5, $6
		FROM transcript WHERE task_id = $1
		RETURNING seq
	`, taskID, from, text, msgTypePtr, idempotencyKey, replyToSeq).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("insert: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	if s.nudge != nil {
		s.nudge()
	}
	return seq, nil
}

func (s *PostgresStore) ReadSince(ctx context.Context, taskID string, sinceSeq int64, limit int) ([]Entry, int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT seq, "from", text, COALESCE(type, ''), reply_to_seq, created_at
		FROM transcript
		WHERE task_id = $1 AND seq >= $2
		ORDER BY seq
		LIMIT $3
	`, taskID, sinceSeq, limit)
	if err != nil {
		return nil, sinceSeq, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	// []Entry{}, not var entries []Entry — see tasks.Store.ListRecentTasks's
	// identical fix: a nil slice marshals to JSON `null`, which the
	// dashboard API now serializes directly (docs/adr/0014).
	entries := []Entry{}
	nextSeq := sinceSeq
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Seq, &e.From, &e.Text, &e.Type, &e.ReplyTo, &e.CreatedAt); err != nil {
			return nil, sinceSeq, fmt.Errorf("scan: %w", err)
		}
		entries = append(entries, e)
		nextSeq = e.Seq + 1
	}
	if err := rows.Err(); err != nil {
		return nil, sinceSeq, fmt.Errorf("rows: %w", err)
	}

	return entries, nextSeq, nil
}

func (s *PostgresStore) LatestSeq(ctx context.Context, taskID string) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(seq) + 1, 0) FROM transcript WHERE task_id = $1`, taskID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("latest seq: %w", err)
	}
	return seq, nil
}
