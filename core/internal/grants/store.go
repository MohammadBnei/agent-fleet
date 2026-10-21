package grants

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

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
// ScopeDefinition. A rule already granted is a no-op.
func (s *Store) Add(ctx context.Context, sessionID, definition, scope, rule, by string) error {
	var err error
	switch scope {
	case ScopeSession:
		_, err = s.pool.Exec(ctx, `
			INSERT INTO session_grants (session_id, rule, granted_by) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, sessionID, rule, by)
	case ScopeDefinition:
		_, err = s.pool.Exec(ctx, `
			INSERT INTO definition_grants (definition_name, rule, granted_by) VALUES ($1, $2, $3)
			ON CONFLICT (definition_name, rule) WHERE revoked_at IS NULL DO NOTHING`, definition, rule, by)
	default:
		return fmt.Errorf("%w: scope %q is not remembered", ErrInvalid, scope)
	}
	if err != nil {
		return fmt.Errorf("add grant: %w", err)
	}
	slog.Info("grants Add", "session", sessionID, "definition", definition, "scope", scope, "rule", rule, "by", by)
	return nil
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
