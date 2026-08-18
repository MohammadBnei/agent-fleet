// Package proposals owns the `proposals` table — machine-initiated
// suggestions from the Alertmanager webhook and the schedule loop.
//
// A separate table rather than a status on `sessions`, and that is the whole
// point (docs/adr/0048): a proposal has no pod path at all. Under the old
// design the guarantee was "dispatch never SELECTs status='proposed'", which
// held only as long as every future query remembered to exclude it. Here
// there is no query to remember — nothing in this package can create a pod,
// because sessions are the only thing that can, and turning a proposal into
// one is an explicit human action behind the dashboard's auth.
//
// That matters most for infra-bootstrap, whose sessions carry cluster access
// (docs/adr/0037): the row that says "an alert fired" and the row that can
// run kubectl are now different tables.
package proposals

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Proposal struct {
	ID     string
	Repo   string
	Source string // "alert" | "audit" (legacy rows) | "schedule"
	// DedupKey is an Alertmanager fingerprint or "schedule:<id>". See Create
	// for the window it is unique over.
	DedupKey  string
	Title     string
	Body      string
	SessionID *string // set once a human opened it
	CreatedAt time.Time
}

// ErrNotOpen is returned by Open and Dismiss when the proposal is already
// opened or already dismissed. Callers map it to FailedPrecondition: from
// the caller's side those are the same fact — there is no open proposal here
// to act on.
var ErrNotOpen = fmt.Errorf("proposal is not open")

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Create files a proposal, returning created=false when one with the same
// (repo, dedupKey) is already standing.
//
// The dedup window is "not dismissed" — deliberately NOT "not yet opened".
// Keying it on un-opened would free the key the moment a human opens the
// proposal, so an hourly cadence whose session runs 3 hours would file
// three proposals for the same thing. Archiving a session dismisses its
// proposal, and that is what re-arms the key.
func (s *Store) Create(ctx context.Context, repo, source, dedupKey, title, body string) (id string, created bool, err error) {
	var keyPtr *string
	if dedupKey != "" {
		keyPtr = &dedupKey
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO proposals (repo, source, dedup_key, title, body)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING
		RETURNING id
	`, repo, source, keyPtr, title, body).Scan(&id)
	if err == pgx.ErrNoRows {
		// The partial unique index rejected it: one is already standing.
		return "", false, nil
	}
	if err != nil {
		slog.Error("proposals Create", "repo", repo, "source", source, "error", err)
		return "", false, fmt.Errorf("create proposal: %w", err)
	}
	slog.Info("proposals Create", "proposalId", id, "repo", repo, "source", source)
	return id, true, nil
}

// ListOpen returns proposals that are neither opened nor dismissed — the
// only ones a human can still act on.
func (s *Store) ListOpen(ctx context.Context) ([]Proposal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, repo, source, COALESCE(dedup_key, ''), title, body, session_id, created_at
		FROM proposals
		WHERE session_id IS NULL AND dismissed_at IS NULL
		ORDER BY created_at DESC
	`)
	if err != nil {
		slog.Error("proposals ListOpen", "error", err)
		return nil, fmt.Errorf("list proposals: %w", err)
	}
	defer rows.Close()

	// []Proposal{}, not nil — a nil slice marshals to JSON `null` and this
	// goes straight out through the dashboard API.
	out := []Proposal{}
	for rows.Next() {
		var p Proposal
		if err := rows.Scan(&p.ID, &p.Repo, &p.Source, &p.DedupKey, &p.Title, &p.Body, &p.SessionID, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan proposal: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get returns nil, nil when no such proposal exists.
func (s *Store) Get(ctx context.Context, id string) (*Proposal, error) {
	var p Proposal
	err := s.pool.QueryRow(ctx, `
		SELECT id, repo, source, COALESCE(dedup_key, ''), title, body, session_id, created_at
		FROM proposals WHERE id = $1
	`, id).Scan(&p.ID, &p.Repo, &p.Source, &p.DedupKey, &p.Title, &p.Body, &p.SessionID, &p.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		slog.Error("proposals Get", "proposalId", id, "error", err)
		return nil, fmt.Errorf("get proposal: %w", err)
	}
	return &p, nil
}

// Open links a proposal to the session a human created from it.
//
// Guarded inside the UPDATE rather than read-then-write, inheriting the
// reasoning from the ApproveProposal it replaces: this is the write that
// hands a cluster-access agent a session, so two clicks, two humans, or one
// stale browser tab must not open two sessions from one proposal. Returns
// ErrNotOpen when someone else won the race.
func (s *Store) Open(ctx context.Context, id, sessionID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE proposals SET session_id = $2
		WHERE id = $1 AND session_id IS NULL AND dismissed_at IS NULL
	`, id, sessionID)
	if err != nil {
		slog.Error("proposals Open", "proposalId", id, "error", err)
		return fmt.Errorf("open proposal: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotOpen
	}
	slog.Info("proposals Open", "proposalId", id, "sessionId", sessionID)
	return nil
}

// Dismiss declines a proposal, which re-arms its dedup key: a still-firing
// alert is proposed again on its next fire. Dismissing means "not now", not
// "never" — permanent suppression is an Alertmanager silence, which is where
// that decision belongs.
func (s *Store) Dismiss(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE proposals SET dismissed_at = now()
		WHERE id = $1 AND dismissed_at IS NULL
	`, id)
	if err != nil {
		slog.Error("proposals Dismiss", "proposalId", id, "error", err)
		return fmt.Errorf("dismiss proposal: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotOpen
	}
	slog.Info("proposals Dismiss", "proposalId", id)
	return nil
}

// DismissForSession re-arms the dedup key when a session is archived, so the
// next schedule tick or alert fire can propose the same work again. Without
// this the key would stay held by a proposal whose session is long finished.
func (s *Store) DismissForSession(ctx context.Context, sessionID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE proposals SET dismissed_at = now()
		WHERE session_id = $1 AND dismissed_at IS NULL
	`, sessionID)
	if err != nil {
		slog.Error("proposals DismissForSession", "sessionId", sessionID, "error", err)
		return fmt.Errorf("dismiss proposals for session: %w", err)
	}
	return nil
}

// Unclaim reverses Open when the session it was opened into could not be made
// usable — the fleet was at capacity, the repo is unknown, the first message
// never landed. Without it, a failure after Open leaves the proposal consumed
// (gone from ListOpen, and a second click gets ErrNotOpen), an orphan session
// with no pod and an empty transcript, and the instruction lost: it lived in
// the proposal body, which nothing else records.
//
// Scoped to the session that claimed it, so a rollback racing someone else's
// successful open cannot steal their claim.
func (s *Store) Unclaim(ctx context.Context, id, sessionID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE proposals SET session_id = NULL
		WHERE id = $1 AND session_id = $2
	`, id, sessionID)
	if err != nil {
		slog.Error("proposals Unclaim", "proposalId", id, "sessionId", sessionID, "error", err)
		return fmt.Errorf("unclaim proposal: %w", err)
	}
	slog.Info("proposals Unclaim", "proposalId", id, "sessionId", sessionID)
	return nil
}
