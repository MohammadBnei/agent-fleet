package grants

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("grant not found")

// Grant is one live definition grant, as the console lists it.
type Grant struct {
	ID         int64
	Definition string
	Rule       string
	GrantedBy  string
	GrantedAt  time.Time
}

// Store is session_grants and definition_grants (migration 000015).
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Add records rule for the session, or for its definition when scope is
// ScopeDefinition. A rule already granted is a no-op. The returned undo
// removes the row this call inserted, and nothing when it inserted none, so
// it never takes back someone else's earlier grant.
func (s *Store) Add(ctx context.Context, sessionID, definition, scope, rule, by string) (undo func(), err error) {
	var remove string
	var args []any
	switch scope {
	case ScopeSession:
		var inserted bool
		err = s.pool.QueryRow(ctx, `
			INSERT INTO session_grants (session_id, rule, granted_by) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING RETURNING true`, sessionID, rule, by).Scan(&inserted)
		remove, args = `DELETE FROM session_grants WHERE session_id = $1 AND rule = $2`, []any{sessionID, rule}
	case ScopeDefinition:
		var id int64
		err = s.pool.QueryRow(ctx, `
			INSERT INTO definition_grants (definition_name, rule, granted_by) VALUES ($1, $2, $3)
			ON CONFLICT (definition_name, rule) WHERE revoked_at IS NULL DO NOTHING RETURNING id`, definition, rule, by).Scan(&id)
		remove, args = `DELETE FROM definition_grants WHERE id = $1`, []any{id}
	default:
		return nil, fmt.Errorf("%w: scope %q is not remembered", ErrInvalid, scope)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return func() {}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("add grant: %w", err)
	}
	slog.Info("grants Add", "session", sessionID, "definition", definition, "scope", scope, "rule", rule, "by", by)
	return func() {
		// A fresh context: the request's may be why the append failed.
		if _, err := s.pool.Exec(context.Background(), remove, args...); err != nil {
			slog.Error("grants Add undo", "rule", rule, "error", err)
		}
	}, nil
}

// Answered reports whether the prompt at seq already has its reply.
func (s *Store) Answered(ctx context.Context, sessionID string, seq int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM transcript WHERE session_id = $1 AND reply_to_seq = $2
		                 AND type IN ('answer', 'permission_response'))`, sessionID, seq).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("answered: %w", err)
	}
	return ok, nil
}

// Rules is every rule a session's worker applies on warm: its own grants and
// its definition's live ones (docs/adr/0076 §3, Delivery).
func (s *Store) Rules(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT rule FROM session_grants WHERE session_id = $1
		UNION
		SELECT g.rule FROM definition_grants g JOIN sessions s ON s.definition_name = g.definition_name
		 WHERE s.id = $1 AND g.revoked_at IS NULL
		ORDER BY 1`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("grant rules: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("grant rules: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// List is every live definition grant, by definition then rule.
func (s *Store) List(ctx context.Context) ([]Grant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, definition_name, rule, granted_by, granted_at FROM definition_grants
		 WHERE revoked_at IS NULL ORDER BY definition_name, rule`)
	if err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ID, &g.Definition, &g.Rule, &g.GrantedBy, &g.GrantedAt); err != nil {
			return nil, fmt.Errorf("list grants: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Revoke ends a definition grant. Running sessions keep it until their next
// warm (docs/adr/0076 §3, accepted).
func (s *Store) Revoke(ctx context.Context, id int64, by string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE definition_grants SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("revoke grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	slog.Info("grants Revoke", "id", id, "by", by)
	return nil
}
