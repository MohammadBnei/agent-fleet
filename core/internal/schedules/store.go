// Package schedules owns the dashboard-editable `schedules` table and the
// loop that fires it (docs/adr/0035, generalized). A schedule is "run this
// prompt against this repo on this cadence"; firing files a proposal, which a
// human opens.
//
// Replaces internal/scheduledaudits + internal/audits, which were the same
// thing with the repo hardcoded to infra-bootstrap and the cadence limited to
// a fixed interval.
//
// Deliberately modeled on internal/repos: same CRUD shape, same SetOnChange
// live-refresh hook, so editing a schedule takes effect without a core
// redeploy or waiting out the current interval.
package schedules

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

// ErrExists is returned by Create when (repo, name) is already taken —
// callers map it to connect.CodeAlreadyExists.
var ErrExists = errors.New("schedule already exists")

type Schedule struct {
	ID     string
	Name   string
	Repo   string
	Prompt string

	// Exactly one of Cron / IntervalSeconds is set, or neither for a one-shot.
	// Cron is a plain string read through COALESCE: the column is nullable
	// (the CHECK needs a real NULL to tell "unset" from "set"), and a nullable
	// column scanned into a plain string fails the whole query rather than the
	// row — that is how one NULL description once emptied the session list.
	// The reverse direction is handled on write with NULLIF.
	Cron            string
	IntervalSeconds *int32

	// RunNow is a human's out-of-band "fire this once", cleared by the claim.
	RunNow bool

	Enabled    bool
	NextRunAt  time.Time
	LastRunAt  *time.Time
	LastStatus string
}

// DueAt reports whether the cadence itself is due — as opposed to the row
// having been listed only because a human pressed "run now".
func (s Schedule) DueAt(now time.Time) bool { return s.Enabled && !s.NextRunAt.After(now) }

// OneShot reports whether this schedule fires once and then disables itself.
func (s Schedule) OneShot() bool { return s.Cron == "" && s.IntervalSeconds == nil }

type Store struct {
	pool     *pgxpool.Pool
	onChange func()
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// SetOnChange fires after every successful mutation — wired in
// cmd/core/run.go to nudge the loop, so a newly created or shortened schedule
// is picked up immediately instead of on the next tick.
func (s *Store) SetOnChange(onChange func()) { s.onChange = onChange }

func (s *Store) changed() {
	if s.onChange != nil {
		s.onChange()
	}
}

const columns = `id, name, repo, prompt, COALESCE(cron, ''), interval_seconds,
	run_now, enabled, next_run_at, last_run_at, COALESCE(last_status, '')`

func scan(row pgx.Row) (Schedule, error) {
	var s Schedule
	err := row.Scan(&s.ID, &s.Name, &s.Repo, &s.Prompt, &s.Cron, &s.IntervalSeconds,
		&s.RunNow, &s.Enabled, &s.NextRunAt, &s.LastRunAt, &s.LastStatus)
	return s, err
}

func collect(rows pgx.Rows) ([]Schedule, error) {
	defer rows.Close()
	// []Schedule{} not nil — a nil slice marshals to JSON `null` and this is
	// returned straight through a dashboard RPC (same fix repos.List has).
	result := []Schedule{}
	for rows.Next() {
		s, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan schedule: %w", err)
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

func (s *Store) List(ctx context.Context) ([]Schedule, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM schedules ORDER BY repo, name`)
	if err != nil {
		slog.Error("schedules List", "error", err)
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	return collect(rows)
}

// Create inserts a schedule. runAt is only meaningful for a one-shot (no cron,
// no interval); the other two compute their own first run from now.
func (s *Store) Create(ctx context.Context, in Schedule, runAt time.Time) (Schedule, error) {
	next := runAt
	if !in.OneShot() {
		var err error
		if next, _, err = nextRun(in, time.Now()); err != nil {
			return Schedule{}, err
		}
	}
	sc, err := scan(s.pool.QueryRow(ctx, `
		INSERT INTO schedules (name, repo, prompt, cron, interval_seconds, next_run_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6)
		RETURNING `+columns, in.Name, in.Repo, in.Prompt, in.Cron, in.IntervalSeconds, next))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Schedule{}, ErrExists
		}
		slog.Error("schedules Create", "error", err)
		return Schedule{}, fmt.Errorf("create schedule: %w", err)
	}
	s.changed()
	return sc, nil
}

// Update rewrites the editable fields and re-anchors the cursor, so shortening
// an interval takes effect immediately rather than after the old (possibly
// much longer) one elapses.
//
// next_run_at is computed here in Go, not in SQL: a cron schedule has a NULL
// interval_seconds, and the old `now() + ($n * interval '1 second')` yields
// NULL for it, which the NOT NULL column rejects — every edit of a cron
// schedule would error. Pause/resume routes through this same method.
func (s *Store) Update(ctx context.Context, in Schedule, runAt time.Time) (Schedule, error) {
	next := runAt
	if !in.OneShot() {
		var err error
		if next, _, err = nextRun(in, time.Now()); err != nil {
			return Schedule{}, err
		}
	}
	sc, err := scan(s.pool.QueryRow(ctx, `
		UPDATE schedules
		SET name = $2, repo = $3, prompt = $4, cron = NULLIF($5, ''),
		    interval_seconds = $6, enabled = $7, next_run_at = $8, updated_at = now()
		WHERE id = $1
		RETURNING `+columns,
		in.ID, in.Name, in.Repo, in.Prompt, in.Cron, in.IntervalSeconds, in.Enabled, next))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Schedule{}, pgx.ErrNoRows
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Schedule{}, ErrExists
		}
		slog.Error("schedules Update", "error", err)
		return Schedule{}, fmt.Errorf("update schedule: %w", err)
	}
	s.changed()
	return sc, nil
}

// RunNow raises a flag rather than moving next_run_at to now(). Moving the
// cursor would consume a cron occurrence — run-now on a Sunday for
// `0 9 * * MON` means Monday 09:00 never happens — and for a schedule whose
// whole point is a fixed wall-clock slot, that is the feature failing at its
// main use.
//
// changed() nudges the loop, whose ListDue then picks this row up on the spot,
// so a manual run takes the identical path a scheduled one does. Deliberately
// ignores `enabled`, and ListDue honours run_now regardless: "run now" on a
// paused schedule is a one-off, and pausing is about the cadence.
func (s *Store) RunNow(ctx context.Context, id string) (Schedule, error) {
	sc, err := scan(s.pool.QueryRow(ctx, `
		UPDATE schedules SET run_now = true, updated_at = now()
		WHERE id = $1
		RETURNING `+columns, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Schedule{}, pgx.ErrNoRows
		}
		slog.Error("schedules RunNow", "error", err)
		return Schedule{}, fmt.Errorf("run schedule now: %w", err)
	}
	s.changed()
	return sc, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM schedules WHERE id = $1`, id)
	if err != nil {
		slog.Error("schedules Delete", "error", err)
		return fmt.Errorf("delete schedule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	s.changed()
	return nil
}

// ListDue returns the schedules that should fire, plus POSTGRES's now().
//
// The clock comes back with the rows because the due-check is SQL and the
// next-run computation is Go (cron cannot be a make_interval). Anchoring the
// Go side on time.Now() instead would let clock skew produce a next-run that
// is already past, leaving the row due on every tick — and two core replicas
// would compute different cursors for the same schedule.
//
// ORDER BY matters with the LIMIT: without it, a schedule can be permanently
// shadowed once more rows are due than the limit.
func (s *Store) ListDue(ctx context.Context, limit int) ([]Schedule, time.Time, error) {
	var now time.Time
	if err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return nil, time.Time{}, fmt.Errorf("read database clock: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+columns+` FROM schedules
		WHERE (enabled AND next_run_at <= now()) OR run_now
		ORDER BY next_run_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("list due schedules: %w", err)
	}
	due, err := collect(rows)
	return due, now, err
}

// Claim advances a schedule's cursor, returning false if this caller lost the
// race. It is what makes firing exactly-once now that the cursor is computed
// outside the database: `next_run_at = prev` is a compare-and-set, so two
// ticks, a ticker racing a nudge, or two core replicas cannot both win.
//
// `AND enabled` is load-bearing twice over. A human pausing a runaway schedule
// between ListDue and Claim must not have it fire anyway — and without the
// guard the `enabled` write below would put the pause back. It also covers the
// spent one-shot, whose next_run_at does not change: the enabled flip is the
// state change, which is why spent rows keep their last intended time instead
// of being parked at a sentinel the UI would render as "739000d ago".
//
// GREATEST pins the result to the database's clock: a next-run in the past is
// how a schedule ends up firing every tick forever.
func (s *Store) Claim(ctx context.Context, id string, prev, next time.Time, spent bool) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE schedules
		SET next_run_at = GREATEST(now() + interval '1 second', $3),
		    last_run_at = now(),
		    run_now = false,
		    enabled = enabled AND NOT $4
		WHERE id = $1 AND next_run_at = $2 AND enabled`, id, prev, next, spent)
	if err != nil {
		return false, fmt.Errorf("claim schedule: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimRunNow consumes a human's manual trigger, leaving the cadence's own
// cursor untouched — that is the whole point of the flag, so a run-now does
// not swallow the next cron occurrence. Clearing the flag IS the
// compare-and-set here, so two ticks racing still fire exactly once, and it
// works on a paused schedule (a manual run is a one-off; pausing is about the
// cadence).
func (s *Store) ClaimRunNow(ctx context.Context, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE schedules SET run_now = false, last_run_at = now()
		WHERE id = $1 AND run_now`, id)
	if err != nil {
		return false, fmt.Errorf("claim run-now schedule: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) RecordStatus(ctx context.Context, id, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE schedules SET last_status = $2 WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("record schedule status: %w", err)
	}
	return nil
}
