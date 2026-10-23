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

	"github.com/MohammadBnei/agent-fleet/core/internal/sessions"
)

type Proposal struct {
	ID     string
	Repo   string
	Source string // "alert" | "audit" (legacy rows) | "schedule" | "event"
	// DedupKey is an Alertmanager fingerprint or "schedule:<id>". See Create
	// for the window it is unique over.
	DedupKey string
	Title    string
	Body     string
	// Payload is the raw JSON the proposal was filed from — an Alertmanager
	// alert object for source "alert", "{}" for anything else. Body is a
	// lossy flattening of it written for the agent to read; this is what the
	// dashboard renders so a human can see the actual alert.
	Payload     string
	SessionID   *string // set once a human opened it
	DismissedAt *time.Time
	CreatedAt   time.Time
	// Event is set for source "event" (docs/adr/0078 §5): opening the
	// proposal opens this definition's thread, not a plain repo session.
	Event *Event
}

// Event is what an event proposal carries so that opening it opens the
// definition's thread with the event's causation and taint.
type Event struct {
	Definition  string
	ThreadKey   string
	CausationID string
	Tainted     bool
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
// SCHEDULES ARE THE ONE EXCEPTION, and only because of what a schedule's
// proposal contains. Waiting for archived/swept means a weekly cadence can
// lose a whole slot: retention is 6d and the cadence is 7d, so a run that
// finished on Tuesday and that nobody archived still holds the key the
// following Tuesday. Observed live on "Weekly Rundown", 2026-09-02.
//
// So for source='schedule' the key also re-arms once the holding session's pod
// is gone. That is the same pod_phase signal rejected above, and it is safe
// HERE for reasons that do not hold anywhere else:
//
//   - the body is regenerable. A schedule's prompt lives in schedules.prompt,
//     so retiring its proposal loses nothing. An alert's body is the only
//     record of the payload — core has no Alertmanager client to ask again —
//     which is why alerts keep the strict rule.
//   - the cadence is the authority for a recurring task. "It is Tuesday" is a
//     better reason to run than "last Tuesday's pod is gone" is a reason not
//     to.
//   - nothing is destroyed and nothing is dispatched. Re-arming files a
//     proposal; opening it is still a human click, so the cost of being wrong
//     is one inbox row, not a second agent.
//
// Event proposals (docs/adr/0078 §5) share the schedule rule. Their key is
// the thread, and once opened the body lives on in that thread's transcript,
// so retiring it loses nothing; holding it until the thread is archived would
// drop the thread's next over-capacity event, which needs a new proposal to
// reach a human.
//
// pod_phase IS NOT NULL is load-bearing: a session that has never been warmed
// has no phase, which is the window inside OpenFromProposal between the
// committed Open and ReserveSlot's write. Without it, a Create landing there
// retires a proposal a human just opened. A session being RE-warmed sits at a
// terminal phase for that same window and can be re-armed under it — accepted,
// since the cost is one duplicate inbox row.
//
// payloadJSON is the raw document the proposal came from, kept verbatim
// because body is a lossy flattening of it and nothing else records the
// original. Empty means "none" and is stored as an empty object, the same
// normalisation journal.Store.Append does — the column is NOT NULL so that no
// reader has to handle a nil.
func (s *Store) Create(ctx context.Context, repo, source, dedupKey, title, body, payloadJSON string) (id string, created bool, err error) {
	return s.create(ctx, repo, source, dedupKey, title, body, payloadJSON, nil)
}

// CreateEvent files an event proposal (source "event"), under Create's dedup
// and the schedule's re-arm rule.
func (s *Store) CreateEvent(ctx context.Context, repo, dedupKey, title, body, payloadJSON string, ev Event) (id string, created bool, err error) {
	return s.create(ctx, repo, "event", dedupKey, title, body, payloadJSON, &ev)
}

func (s *Store) create(ctx context.Context, repo, source, dedupKey, title, body, payloadJSON string, ev *Event) (id string, created bool, err error) {
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
	//
	// WithoutCancel, not ctx: the most likely reason we are unwinding is that
	// ctx just died — core shutting down mid-tick, or an Alertmanager sender
	// hanging up. Issuing ROLLBACK on a done context cannot be written, so pgx
	// destroys the pooled connection instead of returning it, and an alert
	// burst whose senders time out churns one connection per request.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

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
			       AND (
			         s.archived_at IS NOT NULL
			         OR s.swept_at IS NOT NULL
			         OR (p.source IN ('schedule', 'event')
			             AND s.pod_phase IS NOT NULL
			             AND NOT (s.pod_phase = ANY($3)))
			       )
			  )
		`, repo, dedupKey, sessions.LivePhases()); err != nil {
			slog.Error("proposals Create: reap", "repo", repo, "dedupKey", dedupKey, "error", err)
			return "", false, fmt.Errorf("create proposal: reap standing: %w", err)
		}
	}

	var def, thread, causation *string
	tainted := false
	if ev != nil {
		def, thread, causation, tainted = &ev.Definition, &ev.ThreadKey, &ev.CausationID, ev.Tainted
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO proposals (repo, source, dedup_key, title, body, payload, definition_name, thread_key, causation_id, tainted)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT DO NOTHING
		RETURNING id
	`, repo, source, keyPtr, title, body, payloadJSON, def, thread, causation, tainted).Scan(&id)
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
		SELECT id, repo, source, COALESCE(dedup_key, ''), title, body, payload::text, session_id, created_at,
		       definition_name, thread_key, COALESCE(causation_id, ''), tainted, dismissed_at
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
		if err := scanProposal(rows, &p); err != nil {
			return nil, fmt.Errorf("scan proposal: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get returns nil, nil when no such proposal exists.
func (s *Store) Get(ctx context.Context, id string) (*Proposal, error) {
	var p Proposal
	row := s.pool.QueryRow(ctx, `
		SELECT id, repo, source, COALESCE(dedup_key, ''), title, body, payload::text, session_id, created_at,
		       definition_name, thread_key, COALESCE(causation_id, ''), tainted, dismissed_at
		FROM proposals WHERE id = $1
	`, id)
	err := scanProposal(row, &p)
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

// DismissStanding retires whatever proposal currently holds (repo, dedupKey),
// whatever state its session is in.
//
// This is the "run it anyway" path, and it exists because a human pressing Run
// now was being vetoed by the dedup key. That key is there to stop CADENCE
// ticks stacking up on a run in progress; it was never meant to overrule a
// person explicitly asking for a run, and doing so left Run now silently doing
// nothing but writing a skip into last_status (observed live 2026-09-02).
//
// Unconditional on purpose, including an un-opened proposal: re-filing an
// identical one costs nothing, and any narrower rule reintroduces a state
// where the button does not work.
func (s *Store) DismissStanding(ctx context.Context, repo, dedupKey string) error {
	if dedupKey == "" {
		return nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE proposals SET dismissed_at = now()
		WHERE repo = $1 AND dedup_key = $2 AND dismissed_at IS NULL
	`, repo, dedupKey)
	if err != nil {
		slog.Error("proposals DismissStanding", "repo", repo, "dedupKey", dedupKey, "error", err)
		return fmt.Errorf("dismiss standing proposal: %w", err)
	}
	if tag.RowsAffected() > 0 {
		slog.Info("proposals DismissStanding", "repo", repo, "dedupKey", dedupKey)
	}
	return nil
}

// scanProposal reads the columns ListOpen and Get select, in their order.
func scanProposal(row pgx.Row, p *Proposal) error {
	var def, thread *string
	var causation string
	var tainted bool
	if err := row.Scan(&p.ID, &p.Repo, &p.Source, &p.DedupKey, &p.Title, &p.Body, &p.Payload, &p.SessionID, &p.CreatedAt,
		&def, &thread, &causation, &tainted, &p.DismissedAt); err != nil {
		return err
	}
	if def != nil && thread != nil {
		p.Event = &Event{Definition: *def, ThreadKey: *thread, CausationID: causation, Tainted: tainted}
	}
	return nil
}

// maxFoldedBody bounds what FoldEvent adds to a standing proposal: its body
// is the opening message, which AgentService caps at 64 KiB.
const maxFoldedBody = 60 << 10

// FoldEvent appends a later event of the same thread to the standing,
// unopened event proposal holding (repo, dedupKey), so it is not lost while
// the proposal waits (docs/adr/0078 §5). marker names the event; a body that
// already holds it is left alone, so a redelivery folds nothing twice. Past
// maxFoldedBody the event is dropped with one note. A tainted event taints
// the proposal. It reports whether a proposal was there to fold into.
func (s *Store) FoldEvent(ctx context.Context, repo, dedupKey, marker, text string, tainted bool) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE proposals SET
		       body = CASE
		         WHEN position($3 in body) > 0 THEN body
		         WHEN length(body) + length($4) + 2 <= $6 THEN body || E'\n\n' || $4
		         WHEN position('[later events were dropped]' in body) > 0 THEN body
		         ELSE body || E'\n\n[later events were dropped]'
		       END,
		       tainted = tainted OR $5
		 WHERE repo = $1 AND dedup_key = $2 AND source = 'event'
		   AND session_id IS NULL AND dismissed_at IS NULL
	`, repo, dedupKey, marker, text, tainted, maxFoldedBody)
	if err != nil {
		slog.Error("proposals FoldEvent", "repo", repo, "dedupKey", dedupKey, "error", err)
		return false, fmt.Errorf("fold event into proposal: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DismissUnopened retires the proposal holding (repo, dedupKey) if nobody
// opened it: a resolved alert needs no investigation any more (docs/adr/0078
// §5). An opened one stays with its session.
func (s *Store) DismissUnopened(ctx context.Context, repo, dedupKey string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE proposals SET dismissed_at = now()
		WHERE repo = $1 AND dedup_key = $2 AND session_id IS NULL AND dismissed_at IS NULL
	`, repo, dedupKey)
	if err != nil {
		slog.Error("proposals DismissUnopened", "repo", repo, "dedupKey", dedupKey, "error", err)
		return false, fmt.Errorf("dismiss unopened proposal: %w", err)
	}
	if tag.RowsAffected() > 0 {
		slog.Info("proposals DismissUnopened", "repo", repo, "dedupKey", dedupKey)
	}
	return tag.RowsAffected() > 0, nil
}
