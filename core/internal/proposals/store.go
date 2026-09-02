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
	DedupKey string
	Title    string
	Body     string
	// Payload is the raw JSON the proposal was filed from — an Alertmanager
	// alert object for source "alert", "{}" for anything else. Body is a
	// lossy flattening of it written for the agent to read; this is what the
	// dashboard renders so a human can see the actual alert.
	Payload   string
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
// proposal, so an hourly cadence whose session runs 3 hours would file three
// proposals for the same thing.
//
// What re-arms the key is therefore the run ENDING, and there are two ways:
// a human archives the session (ArchiveSession -> DismissForSession), or the
// retention GC sweeps it. The sweep is the one this reap adds. Without it a
// swept session — no longer resumable, its disk gone — held its key forever,
// and the schedule that filed it logged "previous run still open" on every
// tick until someone noticed, which nothing prompted them to do.
//
// The predicate stops at archived/swept ON PURPOSE. Three richer signals look
// like "the run finished" and are all wrong here:
//
//   - last_entry_type = 'result' is written only when the worker sees the
//     SDK's own result message. An OOM-killed, evicted or node-lost pod never
//     gets there, and a human Stop ends on 'interrupt' — so the most common
//     bad endings would wedge exactly as before.
//   - activity_seen is per-POD state, reset by ReserveSlot on every warm. Read
//     as per-session state, a failed re-warm looks finished and re-arms a
//     session that is mid-work with a full transcript.
//   - pod_phase is not the run. The idle sweep reaps the pod after 30 minutes
//     and the worker pod is explicitly paused between rounds, so every opened
//     session sits at a non-live phase in steady state. Worse, the reconcile
//     loop writes TERMINATED in a way it explicitly tolerates being wrong
//     about ("repaired on the next pass") and the provisioner writes CRASHED
//     for transient clone failures — keying on either turns a recoverable
//     phase write into an irreversible dismissed_at.
//
// Archived and swept are the only two endings a machine can compute, which is
// the same reason the sessions table carries no status column at all.
//
// A finished-but-unarchived run therefore still holds its key until the
// retention sweep. That is accepted rather than inferred away: the schedule
// loop names the holding session in last_status (see StandingFor), so it is a
// visible one-click Archive instead of a silent stall.
//
// payloadJSON is the raw document the proposal came from, kept verbatim
// because body is a lossy flattening of it and nothing else records the
// original. Empty means "none" and is stored as an empty object, the same
// normalisation journal.Store.Append does — the column is NOT NULL so that no
// reader has to handle a nil.
func (s *Store) Create(ctx context.Context, repo, source, dedupKey, title, body, payloadJSON string) (id string, created bool, err error) {
	var keyPtr *string
	if dedupKey != "" {
		keyPtr = &dedupKey
	}
	if payloadJSON == "" {
		payloadJSON = "{}"
	}

	// One transaction around reap-then-insert. If the UPDATE committed and the
	// INSERT then failed — context deadline, pool exhaustion, the
	// proposals_source_check CHECK — the key would be freed with nothing filed,
	// and the row would carry a dismissed_at for a reason that never happened,
	// which also silently turns a later DismissForSession into a no-op.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		slog.Error("proposals Create: begin", "repo", repo, "source", source, "error", err)
		return "", false, fmt.Errorf("create proposal: %w", err)
	}
	// Rollback on every path that does not explicitly Commit. A committed tx
	// makes this a no-op.
	defer func() { _ = tx.Rollback(ctx) }()

	if keyPtr != nil {
		// Re-arm the key if the run its standing proposal opened has ended.
		//
		// Written in positive form deliberately: `NOT EXISTS (... archived_at
		// IS NULL ...)` reads as if it also covers a missing session row, which
		// the ON DELETE SET NULL foreign key makes unreachable while session_id
		// is non-NULL.
		//
		// session_id IS NOT NULL leaves two rows alone on purpose: an un-opened
		// proposal, which is sitting in a human's inbox and holds its own key,
		// and one detached by a session delete, which reappears in ListOpen for
		// the same reason.
		//
		// This is the only place outside the sessions package that reads the
		// sessions table. It is here because Create is the single choke point
		// both proposal sources route through, which is what makes the fix
		// self-healing for rows already stuck; and it reads two terminal-state
		// columns, never the pod path this package deliberately cannot touch.
		if _, err = tx.Exec(ctx, `
			UPDATE proposals p SET dismissed_at = now()
			WHERE p.repo = $1 AND p.dedup_key = $2
			  AND p.dismissed_at IS NULL
			  AND p.session_id IS NOT NULL
			  AND EXISTS (
			    SELECT 1 FROM sessions s
			     WHERE s.id = p.session_id
			       AND (s.archived_at IS NOT NULL OR s.swept_at IS NOT NULL)
			  )
		`, repo, dedupKey); err != nil {
			slog.Error("proposals Create: reap", "repo", repo, "dedupKey", dedupKey, "error", err)
			return "", false, fmt.Errorf("create proposal: reap standing: %w", err)
		}
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO proposals (repo, source, dedup_key, title, body, payload)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING
		RETURNING id
	`, repo, source, keyPtr, title, body, payloadJSON).Scan(&id)
	if err == pgx.ErrNoRows {
		// The partial unique index rejected it: one is already standing.
		//
		// Returning WITHOUT committing is deliberate. A conflict here means
		// another transaction inserted a standing row for this key while we
		// were running, so keeping our dismissal would retire the older
		// proposal in favour of one we did not file.
		return "", false, nil
	}
	if err != nil {
		slog.Error("proposals Create", "repo", repo, "source", source, "error", err)
		return "", false, fmt.Errorf("create proposal: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		slog.Error("proposals Create: commit", "repo", repo, "source", source, "error", err)
		return "", false, fmt.Errorf("create proposal: commit: %w", err)
	}
	slog.Info("proposals Create", "proposalId", id, "repo", repo, "source", source)
	return id, true, nil
}

// StandingFor returns the session id of the proposal currently holding
// (repo, dedupKey), or "" when none stands or the one that does has not been
// opened yet.
//
// It exists so a caller that got created=false can say WHICH session is
// holding the key rather than only that something is. A schedule whose run
// finished but was never archived is otherwise indistinguishable from one
// mid-run, and that ambiguity is what let a permanently-stalled schedule sit
// unnoticed behind a green status dot.
func (s *Store) StandingFor(ctx context.Context, repo, dedupKey string) (string, error) {
	if dedupKey == "" {
		return "", nil
	}
	var sessionID *string
	err := s.pool.QueryRow(ctx, `
		SELECT session_id FROM proposals
		WHERE repo = $1 AND dedup_key = $2 AND dismissed_at IS NULL
	`, repo, dedupKey).Scan(&sessionID)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		slog.Error("proposals StandingFor", "repo", repo, "dedupKey", dedupKey, "error", err)
		return "", fmt.Errorf("standing proposal for %q: %w", dedupKey, err)
	}
	if sessionID == nil {
		return "", nil
	}
	return *sessionID, nil
}

// ListOpen returns proposals that are neither opened nor dismissed — the
// only ones a human can still act on.
func (s *Store) ListOpen(ctx context.Context) ([]Proposal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, repo, source, COALESCE(dedup_key, ''), title, body, payload::text, session_id, created_at
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
		if err := rows.Scan(&p.ID, &p.Repo, &p.Source, &p.DedupKey, &p.Title, &p.Body, &p.Payload, &p.SessionID, &p.CreatedAt); err != nil {
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
		SELECT id, repo, source, COALESCE(dedup_key, ''), title, body, payload::text, session_id, created_at
		FROM proposals WHERE id = $1
	`, id).Scan(&p.ID, &p.Repo, &p.Source, &p.DedupKey, &p.Title, &p.Body, &p.Payload, &p.SessionID, &p.CreatedAt)
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
