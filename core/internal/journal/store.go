// Package journal owns writes to the append-only knowledge_journal table.
// As of docs/adr/0020, core is the fleet's sole Postgres-credential
// holder — this used to be worker/src/db.ts's appendJournal, called
// directly over SQL; now it's a CoreService RPC the sidecar calls on the
// worker's behalf, plus core's own ReportPodEvents ingestion writing here
// too (docs/adr/0020 point 3).
package journal

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Append inserts one event. payloadJSON must be a valid JSON document (or
// empty, which stores '{}' — matches the column's own DEFAULT).
func (s *Store) Append(ctx context.Context, repo, actor, eventType, payloadJSON string) error {
	if payloadJSON == "" {
		payloadJSON = "{}"
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO knowledge_journal (repo, actor, event_type, payload)
		VALUES ($1, $2, $3, $4)
	`, repo, actor, eventType, payloadJSON)
	if err != nil {
		return fmt.Errorf("append journal: %w", err)
	}
	return nil
}
