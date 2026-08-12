// Package scheduledaudits owns the dashboard-editable `scheduled_audits`
// table (docs/adr/0035) — periodic cluster checks thot runs on its own.
// Deliberately modeled on internal/repos: same CRUD shape, same
// SetOnChange live-refresh hook, so editing a schedule takes effect
// without a core redeploy or waiting out the current interval.
package scheduledaudits

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrExists is returned by Create when the name is already taken —
// callers map it to connect.CodeAlreadyExists.
var ErrExists = errors.New("scheduled audit already exists")

type Audit struct {
	ID              string
	Name            string
	Prompt          string
	IntervalSeconds int32
	Enabled         bool
	NextRunAt       time.Time
	LastRunAt       *time.Time
	LastStatus      string
}

type Store struct {
	pool     *pgxpool.Pool
	onChange func()
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// SetOnChange fires after every successful mutation — wired in
// cmd/core/run.go to nudge the audit loop, so a newly created or
// shortened schedule is picked up immediately instead of on the next tick.
func (s *Store) SetOnChange(onChange func()) { s.onChange = onChange }

func (s *Store) changed() {
	if s.onChange != nil {
		s.onChange()
	}
}

const columns = `id, name, prompt, interval_seconds, enabled, next_run_at, last_run_at, COALESCE(last_status, '')`

func scan(row pgx.Row) (Audit, error) {
	var a Audit
	err := row.Scan(&a.ID, &a.Name, &a.Prompt, &a.IntervalSeconds, &a.Enabled, &a.NextRunAt, &a.LastRunAt, &a.LastStatus)
	return a, err
}

func (s *Store) List(ctx context.Context) ([]Audit, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM scheduled_audits ORDER BY name`)
	if err != nil {
		slog.Error("scheduledaudits List", "error", err)
		return nil, fmt.Errorf("list scheduled audits: %w", err)
	}
	defer rows.Close()

	// []Audit{} not nil — a nil slice marshals to JSON `null` and this is
	// returned straight through a dashboard RPC (same fix repos.List has).
	result := []Audit{}
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan scheduled audit: %w", err)
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) Create(ctx context.Context, name, prompt string, intervalSeconds int32) (Audit, error) {
	a, err := scan(s.pool.QueryRow(ctx, `
		INSERT INTO scheduled_audits (name, prompt, interval_seconds)
		VALUES ($1, $2, $3)
		RETURNING `+columns, name, prompt, intervalSeconds))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Audit{}, ErrExists
		}
		slog.Error("scheduledaudits Create", "error", err)
		return Audit{}, fmt.Errorf("create scheduled audit: %w", err)
	}
	s.changed()
	return a, nil
}

// Update rewrites the editable fields. next_run_at is deliberately
// recomputed from now(): shortening an interval should take effect
// immediately, not after the old (possibly much longer) one elapses.
// RunNow brings an audit's next run forward instead of dispatching one
// directly. changed() nudges the loop, whose ClaimDue then picks this row up on
// the spot — so a manual run takes the identical path a scheduled one does,
// including landing as `proposed` and including the dedup that skips it while a
// previous run is still open. Nothing here knows how to create a task.
//
// Deliberately ignores `enabled`: "run now" on a paused audit is a one-off, and
// pausing is about the schedule, not about forbidding manual runs.
func (s *Store) RunNow(ctx context.Context, id string) (Audit, error) {
	a, err := scan(s.pool.QueryRow(ctx, `
		UPDATE scheduled_audits
		SET next_run_at = now(), updated_at = now()
		WHERE id = $1
		RETURNING `+columns, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Audit{}, pgx.ErrNoRows
		}
		slog.Error("scheduledaudits RunNow", "error", err)
		return Audit{}, fmt.Errorf("run scheduled audit now: %w", err)
	}
	s.changed()
	return a, nil
}

func (s *Store) Update(ctx context.Context, id, name, prompt string, intervalSeconds int32, enabled bool) (Audit, error) {
	a, err := scan(s.pool.QueryRow(ctx, `
		UPDATE scheduled_audits
		SET name = $2, prompt = $3, interval_seconds = $4, enabled = $5,
		    -- $6, not $4, even though it's the same value: reusing $4 here
		    -- makes Postgres deduce two different types for one parameter
		    -- (INT for the column assignment above, numeric/double for the
		    -- interval arithmetic) and fail with "inconsistent types
		    -- deduced". A second binding of the same value is the cheap fix.
		    next_run_at = now() + ($6 * interval '1 second'),
		    updated_at = now()
		WHERE id = $1
		RETURNING `+columns, id, name, prompt, intervalSeconds, enabled, intervalSeconds))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Audit{}, pgx.ErrNoRows
		}
		slog.Error("scheduledaudits Update", "error", err)
		return Audit{}, fmt.Errorf("update scheduled audit: %w", err)
	}
	s.changed()
	return a, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM scheduled_audits WHERE id = $1`, id)
	if err != nil {
		slog.Error("scheduledaudits Delete", "error", err)
		return fmt.Errorf("delete scheduled audit: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	s.changed()
	return nil
}

// ClaimDue atomically hands back the audits that are due and marks them
// as scheduled for their next run in the same statement.
//
// The UPDATE ... RETURNING is what makes this safe: reading due rows and
// then advancing them in a second query would let two ticks (or two core
// instances) both see the same row and fire it twice.
func (s *Store) ClaimDue(ctx context.Context, limit int) ([]Audit, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE scheduled_audits
		SET next_run_at = now() + make_interval(secs => interval_seconds),
		    last_run_at = now()
		WHERE id IN (
		  SELECT id FROM scheduled_audits
		  WHERE enabled AND next_run_at <= now()
		  ORDER BY next_run_at
		  LIMIT $1
		  FOR UPDATE SKIP LOCKED
		)
		RETURNING `+columns, limit)
	if err != nil {
		return nil, fmt.Errorf("claim due audits: %w", err)
	}
	defer rows.Close()

	result := []Audit{}
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan due audit: %w", err)
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) RecordStatus(ctx context.Context, id, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE scheduled_audits SET last_status = $2 WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("record audit status: %w", err)
	}
	return nil
}
