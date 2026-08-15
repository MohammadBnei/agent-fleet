// Package promptsnippets owns the dashboard-editable `prompt_snippets`
// table — reusable guidance text an operator optionally attaches to a task
// at creation time, replacing worker/src/session.ts's old unconditional,
// hardcoded workflow prompt. Structurally a clone of core/internal/repos'
// Store: same CRUD shape, same ErrExists/onChange pattern.
package promptsnippets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrExists is returned by Create when a snippet with the same name
// already exists — callers map it to connect.CodeAlreadyExists.
var ErrExists = errors.New("prompt snippet already exists")

type Snippet struct {
	ID                      string
	Name                    string
	Text                    string
	SuggestedPermissionMode *string
}

type Store struct {
	pool     *pgxpool.Pool
	onChange func() // optional; set via SetOnChange
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// SetOnChange wires a callback fired after every successful Create/Update/
// Delete — same pattern as repos.Store.SetOnChange.
func (s *Store) SetOnChange(onChange func()) {
	s.onChange = onChange
}

func (s *Store) List(ctx context.Context) ([]Snippet, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, text, suggested_permission_mode FROM prompt_snippets ORDER BY name`)
	if err != nil {
		slog.Error("promptsnippets List", "error", err)
		return nil, fmt.Errorf("list prompt snippets: %w", err)
	}
	defer rows.Close()

	// []Snippet{}, not var snippets []Snippet — a nil slice marshals to
	// JSON `null` (same fix as repos.Store.List), and this is exposed
	// directly through the dashboard's ListPromptSnippets RPC.
	result := []Snippet{}
	for rows.Next() {
		var sn Snippet
		if err := rows.Scan(&sn.ID, &sn.Name, &sn.Text, &sn.SuggestedPermissionMode); err != nil {
			slog.Error("promptsnippets List: scan", "error", err)
			return nil, fmt.Errorf("scan prompt snippet: %w", err)
		}
		result = append(result, sn)
	}
	return result, rows.Err()
}

// GetByIDs resolves a set of snippet ids to their text, in name order —
// used by DashboardService.CreateTask to join the operator's selected
// snippets into a task's guidance column at creation time. Unknown ids are
// silently skipped rather than erroring: a snippet deleted between the
// dashboard fetching its list and the task being submitted shouldn't fail
// task creation.
func (s *Store) GetByIDs(ctx context.Context, ids []string) ([]Snippet, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, name, text, suggested_permission_mode FROM prompt_snippets WHERE id = ANY($1) ORDER BY name`, ids)
	if err != nil {
		slog.Error("promptsnippets GetByIDs", "error", err)
		return nil, fmt.Errorf("get prompt snippets: %w", err)
	}
	defer rows.Close()

	result := []Snippet{}
	for rows.Next() {
		var sn Snippet
		if err := rows.Scan(&sn.ID, &sn.Name, &sn.Text, &sn.SuggestedPermissionMode); err != nil {
			slog.Error("promptsnippets GetByIDs: scan", "error", err)
			return nil, fmt.Errorf("scan prompt snippet: %w", err)
		}
		result = append(result, sn)
	}
	return result, rows.Err()
}

func (s *Store) Create(ctx context.Context, sn Snippet) (Snippet, error) {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO prompt_snippets (name, text) VALUES ($1, $2) RETURNING id
	`, sn.Name, sn.Text).Scan(&sn.ID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Snippet{}, ErrExists
		}
		slog.Error("promptsnippets Create", "name", sn.Name, "error", err)
		return Snippet{}, fmt.Errorf("create prompt snippet: %w", err)
	}
	slog.Info("promptsnippets Create", "id", sn.ID, "name", sn.Name)
	if s.onChange != nil {
		s.onChange()
	}
	return sn, nil
}

// Update returns pgx.ErrNoRows if no snippet with this id exists.
func (s *Store) Update(ctx context.Context, sn Snippet) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE prompt_snippets SET name = $2, text = $3, updated_at = now() WHERE id = $1
	`, sn.ID, sn.Name, sn.Text)
	if err != nil {
		slog.Error("promptsnippets Update", "id", sn.ID, "error", err)
		return fmt.Errorf("update prompt snippet: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	slog.Info("promptsnippets Update", "id", sn.ID, "name", sn.Name)
	if s.onChange != nil {
		s.onChange()
	}
	return nil
}

// Delete returns pgx.ErrNoRows if no snippet with this id exists.
func (s *Store) Delete(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM prompt_snippets WHERE id = $1`, id)
	if err != nil {
		slog.Error("promptsnippets Delete", "id", id, "error", err)
		return fmt.Errorf("delete prompt snippet: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	slog.Info("promptsnippets Delete", "id", id)
	if s.onChange != nil {
		s.onChange()
	}
	return nil
}
