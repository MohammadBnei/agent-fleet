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

// GetByIDs used to live here. It resolved the operator's selected snippet ids
// into the hidden `guidance` column CreateTask wrote at creation time.
//
// Both ends of that are gone: there is no `guidance` column (docs/adr/0048),
// and `snippet_ids` is a reserved field on CreateSessionRequest. Snippets
// prefill the dashboard's message composer instead, so their text reaches the
// model as part of a message a human sent and could edit — which means the
// server never sees an id to resolve. It had no non-test caller left.

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
