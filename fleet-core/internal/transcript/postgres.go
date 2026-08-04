package transcript

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the Store implementation backing planning_transcript
// (db/schema.sql). Concurrent appends to the same task (proposer + critic
// posting near-simultaneously) are serialized via a per-task advisory lock
// so seq assignment can't race.
type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) Append(ctx context.Context, taskID, from, text, msgType, idempotencyKey string) (int64, error) {
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
		`SELECT seq FROM planning_transcript WHERE task_id = $1 AND idempotency_key = $2`,
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
		INSERT INTO planning_transcript (task_id, seq, "from", text, type, idempotency_key)
		SELECT $1, COALESCE(MAX(seq), -1) + 1, $2, $3, $4, $5
		FROM planning_transcript WHERE task_id = $1
		RETURNING seq
	`, taskID, from, text, msgTypePtr, idempotencyKey).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("insert: %w", err)
	}

	return seq, tx.Commit(ctx)
}

func (s *PostgresStore) ReadSince(ctx context.Context, taskID string, sinceSeq int64, limit int) ([]Entry, int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT seq, "from", text, COALESCE(type, '')
		FROM planning_transcript
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
		if err := rows.Scan(&e.Seq, &e.From, &e.Text, &e.Type); err != nil {
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
