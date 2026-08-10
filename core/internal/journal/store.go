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
	"log/slog"
	"time"

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
		slog.Error("journal Append", "repo", repo, "actor", actor, "eventType", eventType, "error", err)
		return fmt.Errorf("append journal: %w", err)
	}
	return nil
}

// Entry is one knowledge_journal row — the read side reliability-
// findings.md #1/#7 both call out as missing (even a journaled crash was
// invisible to anything but direct Postgres access).
type Entry struct {
	ID          int64
	Repo        string
	Actor       string
	EventType   string
	PayloadJSON string
	CreatedAt   time.Time
}

// List returns entries with id > sinceID, ascending (insertion order) —
// same pull/cursor shape as transcript.Store.ReadSince (docs/adr/0013).
// repo == "" matches every repo, not just entries whose own repo is
// literally empty — provisioner-sourced pod-lifecycle events are written
// with repo="" (ReportPodEvents has no per-repo context), so a caller
// asking for one specific repo's history would otherwise never see them
// mixed in with worker-sourced entries that do have a repo.
func (s *Store) List(ctx context.Context, repo string, sinceID int64, limit int) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, COALESCE(repo, ''), actor, event_type, payload::text, created_at
		FROM knowledge_journal
		WHERE id > $1 AND ($2 = '' OR repo = $2)
		ORDER BY id
		LIMIT $3
	`, sinceID, repo, limit)
	if err != nil {
		slog.Error("journal List", "repo", repo, "error", err)
		return nil, fmt.Errorf("list journal: %w", err)
	}
	defer rows.Close()

	// []Entry{}, not var entries []Entry — see tasks.Store.ListRecentTasks's
	// identical fix: a nil slice marshals to JSON `null`, and this is
	// exposed directly through the dashboard API.
	entries := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Repo, &e.Actor, &e.EventType, &e.PayloadJSON, &e.CreatedAt); err != nil {
			slog.Error("journal List: scan", "error", err)
			return nil, fmt.Errorf("scan journal entry: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// Search ranks entries by relevance to query via Postgres full-text search
// (to_tsvector/ts_rank against the same expression knowledge_journal_fts_idx
// covers, db/migrations/000002_journal_fts.up.sql) — no embedding model or
// vector store, backing the journal_search MCP tool (docs/adr/0032's
// deferred "feed knowledge_journal back into a session" read path). Same
// repo == "" matches-all semantics as List.
func (s *Store) Search(ctx context.Context, repo, query string, limit int) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, COALESCE(repo, ''), actor, event_type, payload::text, created_at
		FROM knowledge_journal
		WHERE ($1 = '' OR repo = $1)
		  AND to_tsvector('english', event_type || ' ' || payload::text) @@ plainto_tsquery('english', $2)
		ORDER BY ts_rank(to_tsvector('english', event_type || ' ' || payload::text), plainto_tsquery('english', $2)) DESC
		LIMIT $3
	`, repo, query, limit)
	if err != nil {
		slog.Error("journal Search", "repo", repo, "query", query, "error", err)
		return nil, fmt.Errorf("search journal: %w", err)
	}
	defer rows.Close()

	entries := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.Repo, &e.Actor, &e.EventType, &e.PayloadJSON, &e.CreatedAt); err != nil {
			slog.Error("journal Search: scan", "error", err)
			return nil, fmt.Errorf("scan journal entry: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
