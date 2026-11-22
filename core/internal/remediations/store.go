package remediations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ADR-0086 §3 bounds: one action per target per 10 min, 6 per responder per hour.
const (
	targetWindow    = "10 minutes"
	responderWindow = "1 hour"
	responderMax    = 6
)

// Row is what the executor pulls: the selection plus the definition's bounds.
type Row struct {
	ID          string `json:"id"`
	Action      string `json:"action"`
	Kind        string `json:"kind,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	Name        string `json:"name"`
	Replicas    int    `json:"replicas,omitempty"`
	MinReplicas int    `json:"minReplicas,omitempty"`
	MaxReplicas int    `json:"maxReplicas,omitempty"`
}

// Outcome is what the executor reports back.
type Outcome struct {
	Status  string   `json:"status"` // done or refused
	Detail  string   `json:"detail,omitempty"`
	Inverse []string `json:"inverse,omitempty"`
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

type params struct {
	Selection
	Bounds Bounds `json:"bounds"`
}

// Insert writes one pending row, or ErrRateLimited. The advisory lock makes the count and the insert one step, so two
// selections at once cannot both pass the limit.
func (s *Store) Insert(ctx context.Context, responder string, sel Selection, b Bounds) (string, error) {
	p, _ := json.Marshal(params{sel, b})
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('remediations'))`); err != nil {
		return "", err
	}
	if err := limited(ctx, tx, responder, sel); err != nil {
		return "", err
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO remediations (responder, target, action, params) VALUES ($1, $2, $3, $4) RETURNING id`,
		responder, sel.target(), sel.Action, p).Scan(&id); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// limited is ErrRateLimited when responder or the selection's target is over its window.
func limited(ctx context.Context, q queryer, responder string, sel Selection) error {
	var perTarget, perResponder int
	if err := q.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE target = $2 AND created_at > now() - interval '`+targetWindow+`'),
		count(*) FILTER (WHERE responder = $1 AND created_at > now() - interval '`+responderWindow+`')
		FROM remediations`, responder, sel.target()).Scan(&perTarget, &perResponder); err != nil {
		return err
	}
	if perTarget >= 1 || perResponder >= responderMax {
		return ErrRateLimited
	}
	return nil
}

// Limited previews Insert's rate limit without writing, for a check before an approval is spent.
func (s *Store) Limited(ctx context.Context, responder string, sel Selection) error {
	return limited(ctx, s.pool, responder, sel)
}

// Pull consumes the row: pending becomes pulled in one statement, so a second pull, or a Job retry, finds nothing.
func (s *Store) Pull(ctx context.Context, id string) (Row, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `UPDATE remediations SET state = 'pulled', pulled_at = now()
		WHERE id = $1 AND state = 'pending' RETURNING params`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, ErrNotFound
	}
	if err != nil {
		return Row{}, err
	}
	var p params
	if err := json.Unmarshal(raw, &p); err != nil {
		return Row{}, err
	}
	r := Row{ID: id, Action: p.Action, Kind: p.Kind, Namespace: p.Namespace, Name: p.Name, MinReplicas: p.Bounds.Min, MaxReplicas: p.Bounds.Max}
	if p.Replicas != nil {
		r.Replicas = *p.Replicas
	}
	return r, nil
}

// Report records the outcome of a pulled row, once.
func (s *Store) Report(ctx context.Context, id string, o Outcome) error {
	if o.Status != "done" && o.Status != "refused" {
		return fmt.Errorf("%w: status %q", ErrInvalid, o.Status)
	}
	b, _ := json.Marshal(o)
	tag, err := s.pool.Exec(ctx, `UPDATE remediations SET state = $2, outcome = $3, finished_at = now()
		WHERE id = $1 AND state = 'pulled'`, id, o.Status, b)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SweepLost marks rows pulled but never reported within maxAge as lost: the action may or may not have run, and the
// state says so instead of sitting in 'pulled' forever. It returns the ids.
func (s *Store) SweepLost(ctx context.Context, maxAge time.Duration) ([]string, error) {
	rows, err := s.pool.Query(ctx, `UPDATE remediations SET state = 'lost', finished_at = now(),
		outcome = '{"status":"lost","detail":"pulled, no report: outcome unknown"}'
		WHERE state = 'pulled' AND pulled_at < now() - $1::interval RETURNING id`, fmt.Sprintf("%d seconds", int(maxAge.Seconds())))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Inserter is the store's write side, so Select is testable without Postgres.
type Inserter interface {
	Insert(ctx context.Context, responder string, sel Selection, b Bounds) (string, error)
}

// Service is core's remediation API: checks, then one pending row.
type Service struct {
	Store Inserter
}

// Select checks the responder's raw selection against the schema, and the denylist, then
// writes the row (rate limits are the store's). It returns the row id.
func (s *Service) Select(ctx context.Context, responder string, raw []byte, b Bounds) (string, error) {
	sel, err := s.check(raw, b)
	if err != nil {
		return "", err
	}
	return s.Store.Insert(ctx, responder, sel, b)
}

func (s *Service) check(raw []byte, b Bounds) (Selection, error) {
	sel, err := Parse(raw)
	if err != nil {
		return sel, err
	}
	if err := sel.Validate(b); err != nil {
		return sel, err
	}
	if sel.Denied() {
		return sel, ErrDenied
	}
	return sel, nil
}

// Check is Select's refusals, and the rate limit when the store can preview it, with nothing written:
// what can fail before an approval is spent.
func (s *Service) Check(ctx context.Context, responder string, raw []byte, b Bounds) error {
	sel, err := s.check(raw, b)
	if err != nil {
		return err
	}
	if l, ok := s.Store.(interface {
		Limited(context.Context, string, Selection) error
	}); ok {
		return l.Limited(ctx, responder, sel)
	}
	return nil
}
