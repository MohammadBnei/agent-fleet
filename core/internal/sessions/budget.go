package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
)

// BudgetOverride is how long the owner's raise_budget answer lifts a character's daily turn hold (docs/adr/0084 §9).
const BudgetOverride = 24 * time.Hour

// Turns counts a character's finished turns: the worker's result entries (not core's crash results, which core authors as 'fleet') since
// midnight Europe/Paris and in the last hour. Overridden is true while a raise_budget answer stands.
func (s *Store) Turns(ctx context.Context, id string) (today, lastHour int, overridden bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT
		count(t.seq) FILTER (WHERE t.created_at >= date_trunc('day', now() AT TIME ZONE 'Europe/Paris') AT TIME ZONE 'Europe/Paris'),
		count(t.seq) FILTER (WHERE t.created_at > now() - interval '1 hour'),
		COALESCE(max(s.budget_override_until) > now(), false)
		FROM sessions s LEFT JOIN transcript t ON t.session_id = s.id AND t.type = 'result'
		  AND t."from" <> 'fleet' AND t.created_at > now() - interval '48 hours'
		WHERE s.id = $1`, id).Scan(&today, &lastHour, &overridden)
	if err != nil {
		return 0, 0, false, fmt.Errorf("turns: %w", err)
	}
	return
}

// TurnBudget is the character's pinned limits.turnsPerDay, the default when it sets none.
func (s *Store) TurnBudget(ctx context.Context, id string) (int, error) {
	raw, _, err := s.PinnedEffective(ctx, id)
	if err != nil {
		return 0, err
	}
	var spec definitions.Spec
	if raw != nil {
		if err := json.Unmarshal(raw, &spec); err != nil {
			return 0, fmt.Errorf("turn budget: %w", err)
		}
	}
	return spec.TurnBudget(), nil
}

// BudgetHeld is whether a character's turn-starting entries that are not the owner's wait at their thread's head:
// it has finished its day's budget and no override stands (docs/adr/0084 §9).
func (s *Store) BudgetHeld(ctx context.Context, id string) (bool, error) {
	budget, err := s.TurnBudget(ctx, id)
	if err != nil {
		return false, err
	}
	today, _, over, err := s.Turns(ctx, id)
	return today >= budget && !over, err
}

// RaiseBudget lifts a character's hold for BudgetOverride.
func (s *Store) RaiseBudget(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET budget_override_until = now() + make_interval(secs => $2)
		WHERE id = $1 AND lifecycle = 'standing'`, id, BudgetOverride.Seconds())
	if err != nil {
		return fmt.Errorf("raise budget: %w", err)
	}
	return nil
}
