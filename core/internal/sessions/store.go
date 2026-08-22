// Package sessions owns every read/write against the `sessions` table —
// core is the fleet's sole Postgres-credential holder (docs/adr/0020 point
// 1), so nothing else in the fleet reaches this data except through core.
//
// Renamed from `tasks` in docs/adr/0048, completing the rename docs/adr/0029
// deferred. That is not only a rename: the claim/lease/retry machinery this
// package used to own is gone with it. There is no queue, so there is
// nothing to claim; there is no reclaim, so there is nothing to retry; and
// there is no status, because a polymorphic session has no completion a
// machine can compute — which is why the old enum's 'done' value never
// acquired a writer in its entire life.
//
// What survives is the part that was always doing real work: pod-phase
// liveness, the activity clock the sweeps read, and the lease that stops a
// dying pod overwriting its replacement's resume identity.
package sessions

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// capLockKey serializes the read-decide-provision sequence in ReserveSlot.
//
// Inherited from the deleted ClaimNextTask, along with the incident that
// justified it: under READ COMMITTED every concurrent caller's count(*) sees
// the snapshot from before any of the others committed, so N simultaneous
// callers all read the same low count, all decide there is room, and all
// provision. CI caught exactly that — 4 tasks claimed with maxInFlight=2.
//
// The cap is the fleet's blast-radius bound: how many pods, each running an
// agent, can exist at once. It has to be a real ceiling, not a
// usually-right heuristic. Cheap to hold, since it is taken only on the
// human-or-agent action that boots a pod, never in a hot loop.
const capLockKey = int64(0x61676E74) // "agnt"

// Session is one durable conversation on a repo. A pod is ephemeral compute
// attached to it on demand; this row, the session's directory and its SDK
// state all outlive the pod, which is what makes a session resumable.
type Session struct {
	ID   string `json:"id"`
	Repo string `json:"repo"`
	// Title and Description are human-facing labels. They are deliberately
	// never part of the prompt: a session's actual instruction is its first
	// transcript entry, and a description the agent cannot see is one that
	// silently drifts from what it was really asked to do.
	Title       *string `json:"title,omitempty"`
	Description string  `json:"description"`

	PodPhase   *string `json:"podPhase,omitempty"`
	PodMessage *string `json:"podMessage,omitempty"`
	LastError  *string `json:"lastError,omitempty"`

	// LeaseID is minted per pod. Teardown is not synchronous and the
	// workspace is shared, so a torn-down pod still finishing its shutdown
	// must not be able to overwrite the resume identity of the pod that
	// replaced it. See SaveAgentSessionID.
	LeaseID string `json:"-"`

	// AgentSessionID is the Agent SDK's own session id, for `resume:`. Named
	// so rather than SessionID now that the row itself is a session.
	AgentSessionID *string `json:"agentSessionId,omitempty"`
	Model          *string `json:"model,omitempty"`
	PermissionMode *string `json:"permissionMode,omitempty"`

	// Liveness inputs (docs/adr/0040), maintained by the activity-tracking
	// decorator in cmd/core, so DeriveLiveState can be a pure function of
	// this row rather than a cached column that drifts from the transcript.
	LastEntryType *string    `json:"lastEntryType,omitempty"`
	LastEntryFrom *string    `json:"lastEntryFrom,omitempty"`
	ActivitySeen  bool       `json:"activitySeen"`
	SeenAt        *time.Time `json:"seenAt,omitempty"`
	LastActiveAt  *time.Time `json:"lastActiveAt,omitempty"`

	SweptAt    *time.Time `json:"sweptAt,omitempty"`
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`

	// PendingDecisions is how many unanswered questions and pending
	// permission requests this session has right now. Populated by a join,
	// not stored.
	//
	// It replaces an `awaiting_human` boolean that could not be correct:
	// permissions are a LIST — parallel tool calls each get their own seq —
	// but any single permission_response set and cleared the one flag. So
	// answering one decision marked the session unblocked while others were
	// still waiting, and it sat stalled with nobody notified.
	PendingDecisions int `json:"pendingDecisions"`

	// WorkerImage/SidecarImage are the images this session's pod actually got,
	// written by SetPodImages from CreateWorkerPodResponse. Empty for a session
	// that has never had a pod — and deliberately NOT refreshed from the
	// provisioner's current defaults on read: a session warmed before a fleet
	// upgrade is still running the older build until its next warm.
	WorkerImage  string `json:"workerImage"`
	SidecarImage string `json:"sidecarImage"`
}

// ErrAtCapacity is returned by ReserveSlot when the fleet already has
// maxLive pods. Callers surface it as a FailedPrecondition — it is a real
// answer ("stop one first"), not an internal error.
var ErrAtCapacity = fmt.Errorf("fleet is at its live-session cap")

// ErrNotFound distinguishes "no such session" from a database failure, so
// callers can map it to NotFound rather than Internal.
var ErrNotFound = fmt.Errorf("session not found")

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// selectCols is the column list every read shares, kept in one place so a
// new column cannot be added to one query and forgotten in another. The
// pending-decision count is a correlated subquery rather than a stored
// column for the reason given on Session.PendingDecisions.
// description is COALESCEd because the column is nullable and Session.Description
// is a plain string: one row with a NULL description fails the scan, and that
// takes down the whole of ListSessions rather than that one row. Create() writes
// '' rather than NULL, so nothing in the app produces one today — but
// docs/adr/0048 made description a vestigial label that new sessions leave
// empty, so "nobody writes NULL" is a convention, and one row is enough to blank
// the console. Title stays a pointer; it is optional on the wire too.
const selectCols = `
	s.id, s.repo, s.title, COALESCE(s.description, ''),
	s.pod_phase, s.pod_message, s.last_error, s.lease_id,
	s.agent_session_id, s.model, s.permission_mode,
	s.last_entry_type, s.last_entry_from, s.activity_seen, s.seen_at,
	s.last_active_at, s.swept_at, s.archived_at,
	s.worker_image, s.sidecar_image,
	(SELECT count(*) FROM transcript q
	   WHERE q.session_id = s.id
	     AND q.type IN ('question', 'permission_request')
	     AND NOT EXISTS (
	       SELECT 1 FROM transcript r
	        WHERE r.session_id = q.session_id AND r.reply_to_seq = q.seq
	     )) AS pending_decisions`

func scanSession(row pgx.Row) (*Session, error) {
	var s Session
	var leaseID *string
	err := row.Scan(
		&s.ID, &s.Repo, &s.Title, &s.Description,
		&s.PodPhase, &s.PodMessage, &s.LastError, &leaseID,
		&s.AgentSessionID, &s.Model, &s.PermissionMode,
		&s.LastEntryType, &s.LastEntryFrom, &s.ActivitySeen, &s.SeenAt,
		&s.LastActiveAt, &s.SweptAt, &s.ArchivedAt,
		&s.WorkerImage, &s.SidecarImage,
		&s.PendingDecisions,
	)
	if err != nil {
		return nil, err
	}
	if leaseID != nil {
		s.LeaseID = *leaseID
	}
	return &s, nil
}

// Create makes a session row and nothing else — no pod, no directory.
//
// That split is the human gate (docs/adr/0048): the first message is what
// provisions, and nothing machine-initiated produces a message. It also
// makes an empty session a valid resting state, which matters because the
// Agent SDK's streaming-input generator is never entered until an input
// arrives — a pod booted with nothing to do would never emit a session id,
// would be unresumable, and would be swept as stalled with nothing logged
// to explain it.
// CreateParams is a struct rather than positional arguments because every
// field is a string: repo, title, description, model and permission mode are
// five interchangeable values at a call site, and this repo has already
// shipped one silent same-typed transposition (see
// agent-fleet/docs/verification-traps.md — task_id/session_id compiled,
// linted, passed every mocked test, and made every resume start a brand-new
// conversation). repos.Create already takes a struct for the same reason.
type CreateParams struct {
	Repo        string
	Title       string
	Description string
	Model       string
	// PermissionMode is the mode the session LAUNCHES in. Empty keeps the
	// column default ('default'), which is what every caller wanted before
	// the dashboard could offer a choice. Validate against
	// dashboard.validPermissionModes before calling: the value reaches a real
	// SDK call by way of worker/src/session.ts's launchMode.
	PermissionMode string
}

func (s *Store) Create(ctx context.Context, p CreateParams) (string, error) {
	var id string
	var titlePtr, modelPtr, modePtr *string
	if p.Title != "" {
		titlePtr = &p.Title
	}
	if p.Model != "" {
		modelPtr = &p.Model
	}
	if p.PermissionMode != "" {
		modePtr = &p.PermissionMode
	}
	// COALESCE rather than a conditional INSERT: permission_mode is NOT NULL
	// DEFAULT 'default', so passing NULL must fall back to that default
	// rather than violate the constraint.
	err := s.pool.QueryRow(ctx, `
		INSERT INTO sessions (repo, title, description, model, permission_mode)
		VALUES ($1, $2, $3, $4, COALESCE($5, 'default'))
		RETURNING id
	`, p.Repo, titlePtr, p.Description, modelPtr, modePtr).Scan(&id)
	if err != nil {
		slog.Error("sessions Create", "repo", p.Repo, "error", err)
		return "", fmt.Errorf("create session: %w", err)
	}
	slog.Info("sessions Create", "sessionId", id, "repo", p.Repo, "permissionMode", p.PermissionMode)
	return id, nil
}

// Get returns ErrNotFound when no such session exists.
func (s *Store) Get(ctx context.Context, id string) (*Session, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+selectCols+` FROM sessions s WHERE s.id = $1`, id)
	sess, err := scanSession(row)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		slog.Error("sessions Get", "sessionId", id, "error", err)
		return nil, fmt.Errorf("get session: %w", err)
	}
	return sess, nil
}

// List returns the most recent sessions, newest first. Archived sessions
// are included — the dashboard buckets them rather than hiding them, since
// "what did I finish" is a question a human asks.
//
// A non-empty query is Postgres full-text search over the session's own
// labels (repo/title/description) OR any of its transcript rows' text, ranked
// by relevance (websearch_to_tsquery = quotes/OR/`-` support, ADR-0033's
// journal pattern, no new pg extension). Empty query keeps the created_at
// listing order untouched.
func (s *Store) List(ctx context.Context, limit int, query string) ([]Session, error) {
	if limit <= 0 {
		limit = 100
	}
	sql := `SELECT ` + selectCols + ` FROM sessions s ORDER BY s.created_at DESC LIMIT $1`
	args := []any{limit}
	if query != "" {
		// tsq is reused three times (label match, transcript EXISTS, rank), so
		// bind it once as $2. The label vector and the transcript vector are
		// ranked together; a transcript-only hit still surfaces its session.
		sql = `SELECT ` + selectCols + `
			FROM sessions s
			WHERE to_tsvector('english', s.repo || ' ' || coalesce(s.title,'') || ' ' || coalesce(s.description,'')) @@ websearch_to_tsquery('english', $2)
			   OR EXISTS (
			     SELECT 1 FROM transcript t
			      WHERE t.session_id = s.id
			        AND to_tsvector('english', t.text) @@ websearch_to_tsquery('english', $2)
			   )
			ORDER BY ts_rank(to_tsvector('english', s.repo || ' ' || coalesce(s.title,'') || ' ' || coalesce(s.description,'')), websearch_to_tsquery('english', $2)) DESC,
			         s.created_at DESC
			LIMIT $1`
		args = append(args, query)
	}
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		slog.Error("sessions List", "error", err)
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	// []Session{}, not var out []Session — a nil slice marshals to JSON
	// `null`, and this goes straight out through the dashboard's API.
	out := []Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			slog.Error("sessions List: scan", "error", err)
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, *sess)
	}
	return out, rows.Err()
}

// ReserveSlot atomically checks the live-pod cap and mints a fresh lease,
// returning the lease id the caller must pass to the provisioner.
//
// This is the whole of what replaces ClaimNextTask. The claim is gone —
// there is no queue and nothing to pick from — but the two things that
// query was really doing survive here: bounding concurrency, and resetting
// the per-pod state so a sweep cannot act on the previous pod's history.
//
// Returns ErrAtCapacity when the fleet is full, and ErrNotFound for an
// unknown or archived session.
func (s *Store) ReserveSlot(ctx context.Context, id string, maxLive int) (leaseID string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("reserve slot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// See capLockKey: without this, concurrent callers all read the same
	// pre-commit count and all decide there is room.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, capLockKey); err != nil {
		return "", fmt.Errorf("reserve slot: lock: %w", err)
	}

	var live int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM sessions
		WHERE pod_phase = ANY($1) AND archived_at IS NULL
	`, livePhases).Scan(&live); err != nil {
		return "", fmt.Errorf("reserve slot: count: %w", err)
	}
	if live >= maxLive {
		return "", ErrAtCapacity
	}

	err = tx.QueryRow(ctx, `
		UPDATE sessions
		SET lease_id = gen_random_uuid(),
		    last_active_at = now(),
		    stop_requested_at = NULL,
		    -- A fresh pod is about to be provisioned; nothing it says has
		    -- been heard yet. Reset alongside stop_requested_at for the same
		    -- reason that one is reset: both describe the PREVIOUS pod, and
		    -- leaving either behind makes a sweep act on the new one
		    -- (docs/adr/0040).
		    --
		    -- This is also why activity_seen cannot simply be derived from
		    -- the transcript: a resumed session already has agent-authored
		    -- entries, so a derived version is permanently true and the
		    -- startup-stall sweep would never fire again for a warmed
		    -- session.
		    activity_seen = false,
		    -- The reservation itself, and the reason the cap above is a real
		    -- ceiling rather than a usually-right heuristic.
		    --
		    -- The count reads pod_phase. If this transaction did not WRITE
		    -- pod_phase, the advisory lock would serialize callers around a
		    -- number none of them changed: the phase is otherwise only set
		    -- asynchronously, once CreateWorkerPod reaches the provisioner and
		    -- its event round-trips back through ReportPodEvents. Every warm
		    -- landing inside that window would read the same stale count and
		    -- every one of them would pass.
		    --
		    -- A reservation that then fails to provision is not stuck: the
		    -- reconcile loop finds a row claiming a pod with no Job and clears
		    -- it once its Job has been missing for STARTUP_STALL_MS. That is
		    -- exactly what reconciliation is for. The grace is not optional and
		    -- the claim was false without it — this phase is written BEFORE
		    -- the clone and the pod create, so "no Job" is the normal answer
		    -- for the first tens of seconds.
		    pod_phase = 'POD_PHASE_PROVISIONING',
		    updated_at = now()
		WHERE id = $1 AND archived_at IS NULL
		RETURNING lease_id::text
	`, id).Scan(&leaseID)
	if err == pgx.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reserve slot: mint lease: %w", err)
	}

	// Close out every PERMISSION request the PREVIOUS pod was waiting on, for
	// the same reason stop_requested_at and activity_seen are reset above:
	// they all describe a pod that no longer exists.
	//
	// Nothing else does this. The worker denies its pending permissions on
	// Stop, but with interrupt:true, which deliberately records no resolution;
	// a crashed or idle-swept pod does not even get that far. The requests
	// therefore stay unanswered forever, and pending_decisions is a live
	// count — so the session renders as blocked, offering allow/deny buttons
	// that reach no pod, and keeps doing so after a warm, since the new pod
	// streams from a cursor above them and never sees them at all.
	//
	// Questions are DELIBERATELY excluded. A permission's allow/deny buttons
	// are bound to a specific live pod's canUseTool promise, so once that pod
	// is gone the buttons are dead and the request must be closed. A blocking
	// question is not: its answer is a durable transcript row keyed by the
	// question's own seq, delivered to the NEXT pod on warm
	// (StreamHumanMessages replays the answer above RESUME_FROM_SEQ, and
	// worker/src/session.ts feeds it in as an ordinary turn). So a question
	// can outlive its pod and be answered days later on the same card —
	// stale-closing it here would auto-answer it "restarted before answered"
	// the instant the pod warms, destroying the exact answer the human was
	// about to give.
	if _, err := tx.Exec(ctx, `
		INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key, reply_to_seq)
		SELECT q.session_id,
		       (SELECT coalesce(max(seq), 0) FROM transcript WHERE session_id = q.session_id)
		         + row_number() OVER (ORDER BY q.seq),
		       'system',
		       '{"behavior":"deny","message":"The session was restarted before this was answered."}',
		       'permission_response',
		       'stale-decision-' || q.session_id || '-' || q.seq,
		       q.seq
		  FROM transcript q
		 WHERE q.session_id = $1
		   AND q.type = 'permission_request'
		   AND NOT EXISTS (
		         SELECT 1 FROM transcript r
		          WHERE r.session_id = q.session_id AND r.reply_to_seq = q.seq
		       )
		ON CONFLICT DO NOTHING
	`, id); err != nil {
		return "", fmt.Errorf("reserve slot: resolve stale decisions: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("reserve slot: commit: %w", err)
	}
	slog.Info("sessions ReserveSlot", "sessionId", id, "live", live+1, "cap", maxLive)
	return leaseID, nil
}

// livePhases is the set of pod phases that mean "a pod exists and is
// consuming a slot". Shared by ReserveSlot, CountLivePods and the sweeps so
// they cannot disagree about what "live" means.
var livePhases = []string{
	"POD_PHASE_PROVISIONING",
	"POD_PHASE_CREATED",
	"POD_PHASE_SCHEDULED",
	"POD_PHASE_RUNNING",
}

// IsPodPhaseLive mirrors livePhases for callers holding a single value.
func IsPodPhaseLive(phase *string) bool {
	if phase == nil {
		return false
	}
	for _, p := range livePhases {
		if *phase == p {
			return true
		}
	}
	return false
}

func (s *Store) CountLivePods(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM sessions WHERE pod_phase = ANY($1) AND archived_at IS NULL
	`, livePhases).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count live pods: %w", err)
	}
	return n, nil
}

// SetPodPhase records what Kubernetes says about a session's pod. With
// `status` gone this is the fleet's only liveness signal, written by
// ReportPodEvents and by core's reconcile loop.
func (s *Store) SetPodPhase(ctx context.Context, id, phase, message string) error {
	var msg *string
	if message != "" {
		msg = &message
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions SET pod_phase = $2, pod_message = $3, updated_at = now() WHERE id = $1
	`, id, phase, msg)
	if err != nil {
		slog.Error("sessions SetPodPhase", "sessionId", id, "phase", phase, "error", err)
		return fmt.Errorf("set pod phase: %w", err)
	}
	return nil
}

// SetPodImages records the images the provisioner actually stamped onto this
// session's pod, straight from CreateWorkerPodResponse.
//
// Not lease-scoped, unlike SaveAgentSessionID: this is written synchronously by
// the same call that created the pod, before that pod can report anything, so
// there is no losing writer to guard against.
func (s *Store) SetPodImages(ctx context.Context, id, workerImage, sidecarImage string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions SET worker_image = $2, sidecar_image = $3, updated_at = now() WHERE id = $1
	`, id, workerImage, sidecarImage)
	if err != nil {
		slog.Error("sessions SetPodImages", "sessionId", id, "error", err)
		return fmt.Errorf("set pod images: %w", err)
	}
	return nil
}

// SaveAgentSessionID persists the Agent SDK's own session id for `resume:`.
//
// Lease-scoped by design: this column is what the next resume reads, so a
// torn-down pod still finishing its shutdown must not be able to overwrite
// the identity of the pod that replaced it. Returns false when the caller's
// lease is stale, which is information, not an error.
func (s *Store) SaveAgentSessionID(ctx context.Context, id, agentSessionID, model, leaseID string) (bool, error) {
	var modelPtr *string
	if model != "" {
		modelPtr = &model
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE sessions
		SET agent_session_id = $2, model = COALESCE($3, model), updated_at = now()
		WHERE id = $1 AND lease_id::text = $4
	`, id, agentSessionID, modelPtr, leaseID)
	if err != nil {
		slog.Error("sessions SaveAgentSessionID", "sessionId", id, "error", err)
		return false, fmt.Errorf("save agent session id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		slog.Warn("sessions SaveAgentSessionID: stale lease, ignored", "sessionId", id, "leaseId", leaseID)
		return false, nil
	}
	return true, nil
}

// VerifyLease reports whether leaseID is the CURRENT lease of session id. It
// is the authentication check behind every CoreService call (see
// coreserver/auth.go): a worker pod holds its lease in an env var and presents
// it, and core recomputes nothing — the database is the authority.
//
// A stale lease returns false, not an error. That is the point: ReserveSlot
// mints a fresh lease_id on every warm, so a pod that was torn down and
// replaced stops being able to act as its session the moment the new pod
// starts, with no revocation list to maintain.
//
// ponytail: one indexed primary-key lookup per RPC, no cache. At this fleet's
// size (MAX_LIVE_SESSIONS defaults to 5) that is not worth a cache and its
// invalidation bug; add one if this ever shows up in the RPC durations
// AccessLogInterceptor already logs.
func (s *Store) VerifyLease(ctx context.Context, id, leaseID string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM sessions WHERE id = $1 AND lease_id::text = $2
		)
	`, id, leaseID).Scan(&ok)
	if err != nil {
		slog.Error("sessions VerifyLease", "sessionId", id, "error", err)
		return false, fmt.Errorf("verify lease: %w", err)
	}
	return ok, nil
}

// ClearLease revokes a session's credential at teardown.
//
// Without it the lease stays valid until the NEXT warm rotates it, so a
// deleted pod keeps a working credential for as long as the session then sits
// idle — which is most of the time. Clearing here is what makes the lease a
// revocable credential rather than merely a rotating one.
func (s *Store) ClearLease(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET lease_id = NULL WHERE id = $1`, id)
	if err != nil {
		slog.Error("sessions ClearLease", "sessionId", id, "error", err)
		return fmt.Errorf("clear lease: %w", err)
	}
	return nil
}

func (s *Store) SetPermissionMode(ctx context.Context, id, mode string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET permission_mode = $2, updated_at = now() WHERE id = $1`, id, mode)
	if err != nil {
		slog.Error("sessions SetPermissionMode", "sessionId", id, "error", err)
		return fmt.Errorf("set permission mode: %w", err)
	}
	return nil
}

// UpdateMeta relabels a session's human-facing title/description. A nil
// pointer leaves that column untouched (COALESCE falls back to the current
// value), so a title can be set without clobbering a description. Like
// SetPermissionMode, a plain column write with no side effects.
func (s *Store) UpdateMeta(ctx context.Context, id string, title, description *string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions
		    SET title = COALESCE($2, title),
		        description = COALESCE($3, description),
		        updated_at = now()
		  WHERE id = $1`, id, title, description)
	if err != nil {
		slog.Error("sessions UpdateMeta", "sessionId", id, "error", err)
		return fmt.Errorf("update session meta: %w", err)
	}
	return nil
}

// TouchActive maintains the activity clock on every transcript append. It
// is the sole writer of last_active_at, last_entry_type, last_entry_from
// and activity_seen — i.e. the clock BOTH the idle sweep and the retention
// GC read. Deleting it would leave last_active_at NULL forever, and
// `NULL < now() - interval` is NULL, so the GC would never fire for
// anything.
//
// last_active_at and activity_seen deliberately count DIFFERENT things, and
// conflating them made the startup-stall sweep (docs/adr/0040) dead code on
// the only path that creates pods:
//
//   - last_active_at means "anything happened", human or pod. A human typing
//     is a reason not to call a session idle, so every append bumps it.
//   - activity_seen means "the POD said something" — it answers "did this
//     pod ever come up?", which is a question about the pod alone.
//
// Setting activity_seen on human appends too made it unsettable-by-anything
// -but-a-human, in the wrong direction: ReserveSlot clears it per pod, then
// PostMessage warms and appends (that order is load-bearing, docs/adr/0048),
// so the human's own provisioning message set it true milliseconds later.
// `WHERE NOT activity_seen` in ListStartupStalledIDs then matched nothing,
// ever, and a pod that never started — a bad repos.image, an unschedulable
// node, an ImagePullBackOff — held its slot until the 30-minute idle sweep
// instead of the 3-minute stall sweep written for exactly that case.
//
// OR'd rather than assigned, so a later human message cannot un-see a pod
// that has already spoken.
func (s *Store) TouchActive(ctx context.Context, id, from, entryType string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions
		SET last_active_at = now(),
		    last_entry_from = $2,
		    last_entry_type = $3,
		    activity_seen = activity_seen OR $2 <> 'human',
		    updated_at = now()
		WHERE id = $1
	`, id, from, entryType)
	if err != nil {
		slog.Error("sessions TouchActive", "sessionId", id, "error", err)
		return fmt.Errorf("touch active: %w", err)
	}
	return nil
}

// MarkSeen records that a human opened this session's detail view — what
// turns a `done` live_state (finished while nobody was looking) back into
// `idle`.
func (s *Store) MarkSeen(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET seen_at = now(), updated_at = now() WHERE id = $1`, id)
	if err != nil {
		slog.Error("sessions MarkSeen", "sessionId", id, "error", err)
		return fmt.Errorf("mark seen: %w", err)
	}
	return nil
}

func (s *Store) SetLastError(ctx context.Context, id, message string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET last_error = $2, updated_at = now() WHERE id = $1`, id, message)
	if err != nil {
		slog.Error("sessions SetLastError", "sessionId", id, "error", err)
		return fmt.Errorf("set last error: %w", err)
	}
	return nil
}

// MarkStopRequested records when a human first asked to stop. Stop posts a
// cooperative abort; the grace sweep uses this to force-tear-down a pod
// that never honored it.
//
// This is the only thing that can kill a runaway agent, and that is why it
// survives: a runaway keeps appending transcript entries, so last_active_at
// keeps moving and the idle sweep never fires for it.
func (s *Store) MarkStopRequested(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions SET stop_requested_at = COALESCE(stop_requested_at, now()), updated_at = now()
		WHERE id = $1
	`, id)
	if err != nil {
		slog.Error("sessions MarkStopRequested", "sessionId", id, "error", err)
		return fmt.Errorf("mark stop requested: %w", err)
	}
	return nil
}

// ListOverdueStopIDs finds sessions whose cooperative stop went unhonored
// past the grace period and still have a live pod to tear down.
func (s *Store) ListOverdueStopIDs(ctx context.Context, grace time.Duration) ([]string, error) {
	return s.listIDs(ctx, `
		SELECT id FROM sessions
		WHERE stop_requested_at IS NOT NULL
		  AND stop_requested_at < now() - $1::interval
		  AND pod_phase = ANY($2)
	`, grace, livePhases)
}

// ListStartupStalledIDs finds sessions whose pod came up and never said
// anything (docs/adr/0040). Gated on activity_seen, which ReserveSlot
// resets per pod — see the comment there for why it cannot be derived.
func (s *Store) ListStartupStalledIDs(ctx context.Context, startupStall time.Duration) ([]string, error) {
	return s.listIDs(ctx, `
		SELECT id FROM sessions
		WHERE NOT activity_seen
		  AND pod_phase = ANY($2)
		  AND updated_at < now() - $1::interval
	`, startupStall, livePhases)
}

// ListIdleSessionIDs finds sessions with a live pod nobody is talking to.
func (s *Store) ListIdleSessionIDs(ctx context.Context, idleTimeout time.Duration) ([]string, error) {
	// A session holding an unanswered PERMISSION is not idle — and unlike a
	// question, its pod cannot be replaced.
	//
	// The asymmetry is the one docs/adr/0050 turns on, and ReserveSlot states
	// the same thing from the other side (see its stale-close below): a
	// permission's allow/deny is bound to one live pod's canUseTool promise, so
	// reaping that pod destroys the agent's whole turn and the human's later
	// click lands in a row nothing will ever read. Holding a slot is the
	// cheaper loss. A question is durable and pod-independent — its answer is a
	// transcript row keyed by the question's own seq, delivered to whichever
	// pod comes next — so its pod IS free to die, which is what 0050 decided
	// and what AnswerQuestion's warm now makes true.
	//
	// So: permissions only. Excluding every pending decision would let one
	// forgotten subsidiary question hold 20% of a five-pod fleet forever.
	//
	// Written out rather than sharing selectCols' pending_decisions fragment:
	// that one is a count over both types, this is a filter over one, and
	// gluing SQL strings together to save a duplicate NOT EXISTS is how a
	// WHERE clause ends up applying to the wrong query.
	return s.listIDs(ctx, `
		SELECT id FROM sessions s
		WHERE s.pod_phase = ANY($2)
		  AND s.last_active_at IS NOT NULL
		  AND s.last_active_at < now() - $1::interval
		  AND NOT EXISTS (
		    SELECT 1 FROM transcript q
		     WHERE q.session_id = s.id
		       AND q.type = 'permission_request'
		       AND NOT EXISTS (
		         SELECT 1 FROM transcript r
		          WHERE r.session_id = q.session_id AND r.reply_to_seq = q.seq
		       )
		  )
	`, idleTimeout, livePhases)
}

// ListRetentionExpired finds sessions whose disk the GC should reclaim:
// idle past the retention window, with no live pod, not already swept.
func (s *Store) ListRetentionExpired(ctx context.Context, retention time.Duration) ([]string, error) {
	return s.listIDs(ctx, `
		SELECT id FROM sessions
		WHERE swept_at IS NULL
		  AND NOT (pod_phase = ANY($2))
		  AND COALESCE(last_active_at, created_at) < now() - $1::interval
	`, retention, livePhases)
}

func (s *Store) listIDs(ctx context.Context, query string, d time.Duration, phases []string) ([]string, error) {
	rows, err := s.pool.Query(ctx, query, d.String(), phases)
	if err != nil {
		return nil, fmt.Errorf("list session ids: %w", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan session id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MarkSwept records that the retention GC reclaimed this session's
// directory and SDK state. Core is the single writer of this fact, which is
// what lets the dashboard trust it: a session with swept_at set is readable
// but no longer resumable, so the Warm button must not appear.
//
// Under the old design this claim would have been false — the provisioner's
// own branch sweep deleted worktrees on an independent ticker without
// telling anyone, so a session could lose its disk with nobody recording
// it. That loop is deleted in docs/adr/0048; core now owns every sweep
// decision and writes this in the same pass.
func (s *Store) MarkSwept(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sessions SET swept_at = now(), updated_at = now() WHERE id = $1`, id)
	if err != nil {
		slog.Error("sessions MarkSwept", "sessionId", id, "error", err)
		return fmt.Errorf("mark swept: %w", err)
	}
	return nil
}

// Archive marks a session finished by a human — the only terminal state in
// the fleet, because it is the only one a machine cannot compute.
func (s *Store) Archive(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sessions SET archived_at = COALESCE(archived_at, now()), updated_at = now()
		WHERE id = $1
	`, id)
	if err != nil {
		slog.Error("sessions Archive", "sessionId", id, "error", err)
		return fmt.Errorf("archive session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	slog.Info("sessions Archive", "sessionId", id)
	return nil
}

// Delete removes the row and its transcript for real.
//
// A hard DELETE, where the old schema could only soft-delete: transcript
// REFERENCEs sessions ON DELETE CASCADE and proposals ON DELETE SET NULL,
// so a session with history can actually be removed. That FK pair was the
// entire reason deleted_at existed.
func (s *Store) Delete(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	if err != nil {
		slog.Error("sessions Delete", "sessionId", id, "error", err)
		return fmt.Errorf("delete session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	slog.Info("sessions Delete", "sessionId", id)
	return nil
}

// CountByRepoLiveState backs the Prometheus gauge that used to count
// sessions per status (docs/adr/0047). With the status enum gone, live
// state is what there is to count — and it is the more useful axis anyway,
// since "blocked" and "stalled" are the two a human actually acts on.
func (s *Store) CountByRepoLiveState(ctx context.Context, turnStall time.Duration) (map[[2]string]int, error) {
	list, err := s.List(ctx, 500, "")
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := map[[2]string]int{}
	for _, sess := range list {
		if sess.ArchivedAt != nil {
			continue
		}
		state := DeriveLiveState(&sess, now, turnStall)
		if state == "" {
			state = "idle"
		}
		out[[2]string{sess.Repo, string(state)}]++
	}
	return out, nil
}
