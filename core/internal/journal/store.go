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
	"strconv"
	"strings"
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

// SearchOpts is journal.Store.Search's parameter set. It is a struct rather
// than positional params because Since/Until are two adjacent same-typed
// values: CLAUDE.md's own trap list has a silent swap of exactly that shape
// (SaveAgentSessionId passing the same string for two different string
// fields, which compiled and broke every resume).
type SearchOpts struct {
	Repo  string    // "" matches every repo
	Query string    // "" drops the full-text predicate entirely
	Since time.Time // zero = unbounded; inclusive
	Until time.Time // zero = unbounded; exclusive
	Limit int
}

// Search returns journal entries newest-first.
//
// A non-empty Query ranks by relevance via Postgres full-text search
// (to_tsvector/ts_rank against the same expression knowledge_journal_fts_idx
// covers, db/migrations/000001_init.up.sql) — no embedding model or vector
// store. An empty Query drops the predicate and returns the window in plain
// reverse-chronological order, which is the only way to ask "what happened
// last week" about entries you have not read yet: relevance ranking surfaces
// what matches your guess, not what happened. Same conditional-SQL shape as
// sessions.Store.List. Same repo == "" matches-all semantics as List.
//
// Newest-first, not oldest-first (issue #198 asked for ascending): with a
// LIMIT, ascending truncates a seven-day window to its oldest rows and drops
// the recent ones the caller came for. Reverse client-side for narrative
// order.
func (s *Store) Search(ctx context.Context, o SearchOpts) ([]Entry, error) {
	if o.Limit <= 0 {
		o.Limit = 50
	}
	// args[0] is always repo; every other bind is appended as it applies, so
	// the placeholder numbers follow len(args) rather than a fixed layout.
	args := []any{o.Repo}
	where := []string{`($1 = '' OR repo = $1)`}
	// id DESC is a real tie-break, not decoration: ts_rank ties were
	// previously returned in whatever order the plan happened to produce.
	order := `created_at DESC, id DESC`

	if o.Query != "" {
		args = append(args, o.Query)
		q := fmt.Sprintf("$%d", len(args))
		where = append(where, `to_tsvector('english', event_type || ' ' || payload::text) @@ plainto_tsquery('english', `+q+`)`)
		order = `ts_rank(to_tsvector('english', event_type || ' ' || payload::text), plainto_tsquery('english', ` + q + `)) DESC, ` + order
	}
	if !o.Since.IsZero() {
		args = append(args, o.Since)
		where = append(where, fmt.Sprintf(`created_at >= $%d`, len(args)))
	}
	if !o.Until.IsZero() {
		args = append(args, o.Until)
		where = append(where, fmt.Sprintf(`created_at < $%d`, len(args)))
	}
	args = append(args, o.Limit)

	sql := `
		SELECT id, COALESCE(repo, ''), actor, event_type, payload::text, created_at
		FROM knowledge_journal
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY ` + order + `
		LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		slog.Error("journal Search", "repo", o.Repo, "query", o.Query, "error", err)
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
