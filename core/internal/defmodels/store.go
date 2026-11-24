// Package defmodels stores the console's model pick per definition
// (docs/adr/0088 amendment, migration 000043).
package defmodels

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Override is one definition's chosen model.
type Override struct {
	Definition string
	Model      string
	SetBy      string
	SetAt      time.Time
}

// Reader is what session create reads: nil, nil when the definition has none.
type Reader interface {
	Get(ctx context.Context, definition string) (*Override, error)
}

// Store is definition_model_overrides.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Get is nil, nil when the definition has no override.
func (s *Store) Get(ctx context.Context, definition string) (*Override, error) {
	var o Override
	err := s.pool.QueryRow(ctx, `
		SELECT definition_name, model, set_by, set_at FROM definition_model_overrides WHERE definition_name = $1`,
		definition).Scan(&o.Definition, &o.Model, &o.SetBy, &o.SetAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get definition model: %w", err)
	}
	return &o, nil
}

// List is every override, by definition name.
func (s *Store) List(ctx context.Context) ([]Override, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT definition_name, model, set_by, set_at FROM definition_model_overrides ORDER BY definition_name`)
	if err != nil {
		return nil, fmt.Errorf("list definition models: %w", err)
	}
	defer rows.Close()
	var out []Override
	for rows.Next() {
		var o Override
		if err := rows.Scan(&o.Definition, &o.Model, &o.SetBy, &o.SetAt); err != nil {
			return nil, fmt.Errorf("list definition models: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Set replaces the definition's override.
func (s *Store) Set(ctx context.Context, definition, model, by string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO definition_model_overrides (definition_name, model, set_by) VALUES ($1, $2, $3)
		ON CONFLICT (definition_name) DO UPDATE SET model = $2, set_by = $3, set_at = now()`,
		definition, model, by); err != nil {
		return fmt.Errorf("set definition model: %w", err)
	}
	return nil
}

// Clear removes the override; clearing none is a no-op.
func (s *Store) Clear(ctx context.Context, definition string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM definition_model_overrides WHERE definition_name = $1`, definition); err != nil {
		return fmt.Errorf("clear definition model: %w", err)
	}
	return nil
}
