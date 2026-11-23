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
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/core/internal/definitions"
	"github.com/MohammadBnei/agent-fleet/core/internal/metrics"
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
	// replaced it. See SaveAgentSessionID. lease_id only: a definition
	// session's lease (profile_lease_id, docs/adr/0064) is never read back.
	LeaseID string `json:"-"`

	// AgentSessionID is the Agent SDK's own session id, for `resume:`. Named
	// so rather than SessionID now that the row itself is a session.
	AgentSessionID *string `json:"agentSessionId,omitempty"`
	Model          *string `json:"model,omitempty"`
	PermissionMode *string `json:"permissionMode,omitempty"`
	// Effort is the repo session's chosen effort level; NULL = Claude Code's
	// default (docs/adr/0077).
	Effort *string `json:"effort,omitempty"`
	// TrustSet is the Rule of Two letters (docs/adr/0086 §2), stored at create. Read it, never recompute it.
	TrustSet string `json:"trustSet"`

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
	// permission requests this session has right now: its open rows in the
	// decisions table (docs/adr/0082 §5), counted, not stored.
	//
	// It replaces an `awaiting_human` boolean that could not be correct:
	// permissions are a LIST — parallel tool calls each get their own seq —
	// but any single permission_response set and cleared the one flag. So
	// answering one decision marked the session unblocked while others were
	// still waiting, and it sat stalled with nobody notified.
	PendingDecisions int `json:"pendingDecisions"`
	// BlockingDecisions is the part of PendingDecisions an older worker
	// really is blocked on: open permissions and plans without the "nb": 1
	// stamp (docs/adr/0082 §5), whose canUseTool promise waits with no time
	// limit. DeriveLiveState keeps today's always-Blocked rule for these.
	BlockingDecisions int `json:"-"`
	// ParkDecisions is the part of PendingDecisions a parked run waits on:
	// open questions, permissions and plans, never cards (parkedWhere's kinds,
	// docs/adr/0070 §6).
	ParkDecisions int `json:"-"`

	// WorkerImage/SidecarImage are the images this session's pod actually got,
	// written by SetPodImages from CreateWorkerPodResponse. Empty for a session
	// that has never had a pod — and deliberately NOT refreshed from the
	// provisioner's current defaults on read: a session warmed before a fleet
	// upgrade is still running the older build until its next warm.
	WorkerImage  string `json:"workerImage"`
	SidecarImage string `json:"sidecarImage"`
	// FleetVersion is the release of the provisioner that built that pod,
	// written alongside the images. Empty for a pod built before the
	// provisioner reported it.
	FleetVersion string `json:"fleetVersion"`

	// DefinitionName/DefinitionSHA pin a session created from a defined agent
	// to that version (docs/adr/0062). Both nil for a repo session. Pointers:
	// the columns are nullable, and a NULL scanned into a string fails the row.
	DefinitionName *string `json:"definitionName,omitempty"`
	DefinitionSHA  *string `json:"definitionSha,omitempty"`

	// docs/adr/0067 §1, AgentService sessions only. A nil Owner (every
	// console-created session) reads as admin-only; a nil Lifecycle reads
	// as resident. The interaction column is no longer read or written
	// (docs/adr/0070 §2).
	Owner      *string   `json:"owner,omitempty"`
	Visibility *string   `json:"visibility,omitempty"`
	Invoker    *string   `json:"invoker,omitempty"`
	Lifecycle  *string   `json:"lifecycle,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	// StartedBy and StoppedAt belong to a standing row (docs/adr/0084 §2): the admin who started it, and when it was
	// stopped (nil while it runs).
	StartedBy *string    `json:"startedBy,omitempty"`
	StoppedAt *time.Time `json:"stoppedAt,omitempty"`
}

// Caps is the pair of ceilings ReserveSlot enforces. A struct, not two ints:
// two adjacent same-typed arguments are exactly the transposition trap
// docs/verification-traps.md records.
type Caps struct {
	// MaxLive is the fleet's live-pod cap (MAX_IN_FLIGHT_TASKS).
	MaxLive int
	// MaxDefinition is the definition pool INSIDE MaxLive
	// (MAX_DEFINITION_SESSIONS, docs/adr/0062). Repo sessions never count
	// against it, so a full pool still leaves MaxLive-MaxDefinition slots.
	MaxDefinition int
	// MaxExternal is the pool INSIDE MaxDefinition for sessions a member or
	// client owns (docs/adr/0066 §8), and MaxPerMember a member's own share
	// of it. Owner-less (console) definition sessions draw only on
	// MaxDefinition, so members and clients cannot take it from an admin.
	MaxExternal  int
	MaxPerMember int
	// MaxStanding is the pool OUTSIDE MaxLive for standing characters (MAX_STANDING_SESSIONS, docs/adr/0084 §9):
	// standing rows count only here, and never in MaxLive, MaxDefinition or a definition's concurrency.
	MaxStanding int
}

// ErrAtCapacity is returned by ReserveSlot when the fleet already has
// maxLive pods. Callers surface it as a FailedPrecondition — it is a real
// answer ("stop one first"), not an internal error.
var ErrAtCapacity = fmt.Errorf("fleet is at its live-session cap")

// ErrStopped: a standing character the owner stopped takes no pod until Start (docs/adr/0084 §2).
var ErrStopped = errors.New("this character is stopped: Start it first")

// ErrNotFound distinguishes "no such session" from a database failure, so
// callers can map it to NotFound rather than Internal.
var ErrNotFound = fmt.Errorf("session not found")

// ErrModeNotAllowed: SetPermissionMode on a definition session with a mode
// outside its pinned version's mode.allow.
var ErrModeNotAllowed = errors.New("mode is outside this definition's mode.allow")

// ErrDefinitionInvalid: the session is pinned to a definition row that is no
// longer valid (a re-judge flipped it, docs/adr/0062).
var ErrDefinitionInvalid = errors.New("this session's definition version is no longer valid")

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
// ” rather than NULL, so nothing in the app produces one today — but
// docs/adr/0048 made description a vestigial label that new sessions leave
// empty, so "nobody writes NULL" is a convention, and one row is enough to blank
// the console. Title stays a pointer; it is optional on the wire too.
const selectCols = `
	s.id, s.repo, s.title, COALESCE(s.description, ''),
	s.pod_phase, s.pod_message, s.last_error, s.lease_id,
	s.agent_session_id, s.model, s.permission_mode, s.effort,
	s.last_entry_type, s.last_entry_from, s.activity_seen, s.seen_at,
	s.last_active_at, s.swept_at, s.archived_at,
	s.worker_image, s.sidecar_image, s.fleet_version,
	s.definition_name, s.definition_sha,
	s.owner, s.visibility, s.invoker, s.lifecycle, s.created_at, s.trust_set,
	s.started_by, s.stopped_at,
	(SELECT count(*) FROM decisions d
	   WHERE d.session_id = s.id AND d.state = 'open') AS pending_decisions,
	(SELECT count(*) FROM decisions d
	   WHERE d.session_id = s.id AND d.state = 'open' AND d.kind IN ('question', 'permission', 'plan')) AS park_decisions,
	(SELECT count(*) FROM decisions d
	   WHERE d.session_id = s.id AND d.state = 'open' AND d.kind IN ('permission', 'plan')
	     AND d.payload -> 'nb' IS DISTINCT FROM '1'::jsonb) AS blocking_decisions`

func scanSession(row pgx.Row) (*Session, error) {
	var s Session
	var leaseID *string
	err := row.Scan(
		&s.ID, &s.Repo, &s.Title, &s.Description,
		&s.PodPhase, &s.PodMessage, &s.LastError, &leaseID,
		&s.AgentSessionID, &s.Model, &s.PermissionMode, &s.Effort,
		&s.LastEntryType, &s.LastEntryFrom, &s.ActivitySeen, &s.SeenAt,
		&s.LastActiveAt, &s.SweptAt, &s.ArchivedAt,
		&s.WorkerImage, &s.SidecarImage, &s.FleetVersion,
		&s.DefinitionName, &s.DefinitionSHA,
		&s.Owner, &s.Visibility, &s.Invoker, &s.Lifecycle, &s.CreatedAt, &s.TrustSet,
		&s.StartedBy, &s.StoppedAt,
		&s.PendingDecisions, &s.ParkDecisions, &s.BlockingDecisions,
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
	// Effort is the SDK effort level; empty writes NULL. Validate against
	// definitions.EffortLevels before calling, as for PermissionMode.
	Effort string
	// DefinitionName/DefinitionSHA pin the session to a definition version
	// (docs/adr/0062). Both or neither; empty writes NULL.
	DefinitionName string
	DefinitionSHA  string
	// Owner, Visibility and Lifecycle are set for AgentService threads only
	// (docs/adr/0067 §1); empty writes NULL.
	Owner      string
	Visibility string
	Lifecycle  string
	// Invoker and ThreadKey name a client's thread (docs/adr/0066 §6).
	Invoker   string
	ThreadKey string
	// TrustSet is TrustSetFor's answer for the trigger; empty is BC (an owner or fleet trigger).
	TrustSet string
	// StartedBy is the admin who started a standing character; set only with Lifecycle "standing".
	StartedBy string
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
	// COALESCE rather than a conditional INSERT: permission_mode is NOT NULL,
	// so passing NULL must fall back to a default rather than violate the
	// constraint. 'auto' matches the column default (db/migrations/000007,
	// docs/adr/0061).
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	err = tx.QueryRow(ctx, `
		INSERT INTO sessions (repo, title, description, model, permission_mode, definition_name, definition_sha, effort,
		                      owner, visibility, lifecycle, invoker, thread_key, trust_set, started_by)
		VALUES ($1, $2, $3, $4, COALESCE($5, 'auto'), NULLIF($6, ''), NULLIF($7, ''), NULLIF($8, ''),
		        NULLIF($9, ''), NULLIF($10, ''), NULLIF($11, ''), NULLIF($12, ''), NULLIF($13, ''), COALESCE(NULLIF($14, ''), 'BC'), NULLIF($15, ''))
		RETURNING id
	`, p.Repo, titlePtr, p.Description, modelPtr, modePtr, p.DefinitionName, p.DefinitionSHA, p.Effort,
		p.Owner, p.Visibility, p.Lifecycle, p.Invoker, p.ThreadKey, p.TrustSet, p.StartedBy).Scan(&id)
	if err != nil {
		slog.Error("sessions Create", "repo", p.Repo, "error", err)
		return "", fmt.Errorf("create session: %w", err)
	}
	if p.Lifecycle == Standing {
		// The character's one starting thread (docs/adr/0084 §4); further threads open on the owner's messages.
		if _, err := tx.Exec(ctx, `INSERT INTO session_threads (session_id, thread_key) VALUES ($1, $2)`, id, MainThread); err != nil {
			return "", fmt.Errorf("create session: main thread: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	slog.Info("sessions Create", "sessionId", id, "repo", p.Repo, "permissionMode", p.PermissionMode, "definition", p.DefinitionName)
	return id, nil
}

// ListThreads is the open definition sessions of definitions that owner may
// see (docs/adr/0067 §2): its own, and with shared the shared ones. admin
// sees every one, owner-less console sessions included. Newest first.
//
// ponytail: no pagination; 500 rows, and CreateThread caps a member's open
// threads well below that. Add a cursor when a definition's shared threads
// outgrow it.
func (s *Store) ListThreads(ctx context.Context, definitions []string, owner string, shared, admin bool) ([]Session, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+selectCols+` FROM sessions s
		WHERE s.archived_at IS NULL AND s.definition_name = ANY($1)
		  AND ($4 OR s.owner = $2 OR ($3 AND s.visibility = 'shared'))
		ORDER BY s.created_at DESC LIMIT 500`, definitions, owner, shared, admin)
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		t, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("list threads: %w", err)
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// Answered reports whether the prompt at seq already has its reply.
func (s *Store) Answered(ctx context.Context, id string, seq int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM transcript WHERE session_id = $1 AND reply_to_seq = $2
		                 AND type IN ('answer', 'permission_response'))`, id, seq).Scan(&ok)
	return ok, err
}

// ThreadRef names an open client thread by what its thread lock is made of.
type ThreadRef struct{ ID, Owner, Definition, ThreadKey string }

// OpenOnThreadKey is every open session whose thread key is key, matched
// case-insensitively, whatever its invoker: what a card's close_threads
// closes (docs/adr/0082). GitHub's owner/name are case-insensitive, and a
// card's subject is built lowercased.
func (s *Store) OpenOnThreadKey(ctx context.Context, key string) ([]ThreadRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, COALESCE(owner, ''), COALESCE(definition_name, ''), thread_key FROM sessions
		 WHERE lower(thread_key) = lower($1) AND archived_at IS NULL ORDER BY created_at`, key)
	if err != nil {
		return nil, fmt.Errorf("open on thread key: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ThreadRef])
}

// OpenThreads counts owner's open sessions, for CreateThread's and Open's
// cap. Single-shot runs are left out: they have no thread key to close them
// by, and UnfinishedRuns bounds them instead.
func (s *Store) OpenThreads(ctx context.Context, owner string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE owner = $1 AND archived_at IS NULL
		AND lifecycle IS DISTINCT FROM 'single-shot'`, owner).Scan(&n)
	return n, err
}

// UnfinishedRuns counts owner's open single-shot sessions with no result
// entry yet, for Invoke's cap.
//
// ponytail: "has a result" stands in for finished, so a parked run (whose
// parked turn wrote one) does not hold the cap; maxParked bounds those. An
// ended run is archived within a tick of its pod's exit (endRuns), so
// counting the unarchived ones instead is the upgrade if a parked run should
// hold the cap too.
func (s *Store) UnfinishedRuns(ctx context.Context, owner string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sessions s WHERE s.owner = $1 AND s.archived_at IS NULL
		AND s.lifecycle = 'single-shot'
		AND NOT EXISTS (SELECT 1 FROM transcript r WHERE r.session_id = s.id AND r.type = 'result')`, owner).Scan(&n)
	return n, err
}

// exitResultOf selects the seq, consumedSeq and text of the latest exit
// result (the worker's last result before its pod exits, docs/adr/0070 §3)
// of the session s.id. A result entry is the worker's JSON; IS JSON guards
// the casts, inside CASE, which evaluates in order, so one unreadable row
// never fails the query. A missing consumedSeq reads as -1: every
// turn-starting entry is then above it.
const exitResultOf = `(SELECT r.seq, r.text,
	        COALESCE(CASE WHEN r.text IS JSON OBJECT THEN
	                   CASE WHEN jsonb_typeof(r.text::jsonb -> 'consumedSeq') = 'number'
	                        THEN floor((r.text::jsonb ->> 'consumedSeq')::numeric)::bigint END END, -1) AS consumed
	   FROM transcript r
	  WHERE r.session_id = s.id AND r.type = 'result'
	    AND CASE WHEN r.text IS JSON OBJECT THEN r.text::jsonb -> 'exit' = 'true'::jsonb ELSE false END
	  ORDER BY r.seq DESC LIMIT 1)`

// turnStarting is the WHERE fragment for a transcript row h that starts a
// turn: a human message, answer or permission response, or a peer session's
// relayed prompt (from 'session', docs/adr/0041), never context. It matches
// the worker's takeIn filter, so consumedSeq and this agree on what input is.
const turnStarting = `h."from" IN ('human', 'session') AND NOT h.context_only
	  AND COALESCE(h.type, '') IN ('', 'discussion', 'answer', 'permission_response')`

// inputAfterExit is "an entry that starts a turn lies above the latest exit
// result's consumedSeq" (docs/adr/0070 §3 rule 1): the pod that wrote it
// never took that entry in, so the run is not final, and the next warm starts
// just past consumedSeq (§6, the exit window). False with no exit result.
const inputAfterExit = `EXISTS (SELECT 1 FROM transcript h
	WHERE h.session_id = s.id AND ` + turnStarting + `
	  AND h.seq > (SELECT e.consumed FROM ` + exitResultOf + ` e))`

// legacyFinal selects the text of the session s.id's latest result when no
// turn-starting entry lies above its seq: the seq rule from before slice 7.
// WaitForResult judges an ended run with no exit result at all on it instead
// of NO_RESULT: a run whose pod ran an older worker at deploy, or whose
// result had no queued_turn_count (it never exits on its own).
const legacyFinal = `(SELECT r.text FROM transcript r
	  WHERE r.session_id = s.id AND r.type = 'result'
	    AND NOT EXISTS (SELECT 1 FROM transcript h
	                     WHERE h.session_id = s.id AND h.seq > r.seq AND ` + turnStarting + `)
	  ORDER BY r.seq DESC LIMIT 1)`

// endedRun is the WHERE fragment for a single-shot session s that reached a
// terminal state (docs/adr/0070 §7) and has no live pod ($1 = livePhases):
// already claimed (archived, not yet swept); or a stop was asked for; or it
// parked more than its limits.maxParks times (overParkCap: PARK_LIMIT); or,
// with no open decision, its pod crashed, or exited (TERMINATED) with no
// input above its exit result's consumedSeq: a final exit result, or a clean
// exit with none (NO_RESULT, which WaitForResult reports). An open decision
// without a stop is a parked run, and input above the exit result waits for
// its re-warm (ListRewarmRuns): neither is listed, so both keep their PVC.
var endedRun = `s.lifecycle = 'single-shot' AND s.swept_at IS NULL
	AND COALESCE(s.pod_phase, '') <> ALL($1)
	AND (s.archived_at IS NOT NULL
	     OR s.stop_requested_at IS NOT NULL
	     OR ` + overParkCap + `
	     OR (NOT EXISTS (SELECT 1 FROM decisions d WHERE d.session_id = s.id AND d.state = 'open')
	         AND (s.pod_phase = 'POD_PHASE_CRASHED'
	              OR (s.pod_phase = 'POD_PHASE_TERMINATED' AND NOT ` + inputAfterExit + `))))`

// parkCount counts the parks of the session s (docs/adr/0070 §6, "Re-park
// cap"): its exit results at whose created_at a question, permission or plan
// of s was open, created earlier and still open or answered later. A
// decision the agent withdrew (no answered_at) counts only while open,
// except on a claimed run: ClaimEndedRun withdraws the last park's decision
// and stamps no time, so on an archived s a withdrawn one counts as open
// until archived_at, and the PARK_LIMIT verdict outlives the claim.
//
// ponytail: that over-counts a decision a client withdrew before the claim
// for any exit between that withdrawal and archived_at (nothing warms after
// a withdraw, so only a human message can make one); a withdrawn_at column
// is the upgrade if that ever matters.
//
// ponytail: counted from rows each time, a handful per run; a parks counter
// on sessions (a new migration pair) is the upgrade if it ever shows slow.
const parkCount = `(SELECT count(*) FROM transcript r
	  WHERE r.session_id = s.id AND r.type = 'result'
	    AND CASE WHEN r.text IS JSON OBJECT THEN r.text::jsonb -> 'exit' = 'true'::jsonb ELSE false END
	    AND EXISTS (SELECT 1 FROM decisions d
	                 WHERE d.session_id = s.id AND d.kind IN ('question', 'permission', 'plan')
	                   AND d.created_at < r.created_at
	                   AND (d.state = 'open' OR d.answered_at > r.created_at
	                        OR (d.state = 'withdrawn' AND d.answered_at IS NULL AND s.archived_at > r.created_at))))`

// overParkCap is "the single-shot session s parked more than its
// definition's limits.maxParks times" (definitions.DefaultMaxParks when the
// pinned row sets none, or the row is gone): it ends failed, PARK_LIMIT.
var overParkCap = `(s.lifecycle = 'single-shot' AND ` + parkCount + ` > COALESCE(
	(SELECT (ad.effective -> 'limits' ->> 'maxParks')::int FROM agent_definitions ad
	  WHERE ad.name = s.definition_name AND ad.sha = s.definition_sha), ` + strconv.Itoa(definitions.DefaultMaxParks) + `))`

// ParkState reads whether the session is a parked run (parkedWhere) and
// whether it is over its park cap (overParkCap), for warm's refusals
// (docs/adr/0070 §6, "A parked session takes only its answer").
func (s *Store) ParkState(ctx context.Context, sessionID string) (parked, overCap bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT `+parkedWhere+`, `+overParkCap+` FROM sessions s WHERE s.id = $1`,
		sessionID, livePhases).Scan(&parked, &overCap)
	if err != nil {
		return false, false, fmt.Errorf("park state: %w", err)
	}
	return parked, overCap, nil
}

// ListRewarmRuns is every single-shot run in the exit window (docs/adr/0070
// §6): its pod exited cleanly, it is neither archived nor stopping, and a
// turn-starting entry lies above its exit result's consumedSeq. Every warm
// of such a run starts the next pod just past that consumedSeq (ExitCursor);
// this lists the ones no sender has warmed, for the loop to warm.
func (s *Store) ListRewarmRuns(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT s.id::text FROM sessions s
		WHERE s.lifecycle = 'single-shot' AND s.swept_at IS NULL AND s.archived_at IS NULL
		  AND s.stop_requested_at IS NULL AND s.pod_phase = 'POD_PHASE_TERMINATED'
		  AND `+inputAfterExit)
	if err != nil {
		return nil, fmt.Errorf("list rewarm runs: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list rewarm runs: %w", err)
	}
	return ids, nil
}

// ExitCursor is where a new pod of a single-shot session starts streaming
// (docs/adr/0070 §6): just past its latest exit result's consumedSeq while a
// turn-starting entry lies above it, so the entry the leaving pod never took
// in reaches the next pod, whoever warms. ok is false otherwise (no exit
// result, nothing above it, or not single-shot): LatestSeq stands.
func (s *Store) ExitCursor(ctx context.Context, sessionID string) (fromSeq int64, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT (SELECT e.consumed FROM `+exitResultOf+` e) + 1
		FROM sessions s WHERE s.id = $1 AND s.lifecycle = 'single-shot' AND `+inputAfterExit, sessionID).Scan(&fromSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("exit cursor: %w", err)
	}
	return fromSeq, true, nil
}

// ExitResult is what WaitForResult reads of a run's end (docs/adr/0070 §3).
type ExitResult struct {
	// Text is the latest exit result's JSON; Found is false with none.
	Text  string
	Found bool
	// InputAfter: a turn-starting entry lies above its consumedSeq, so it is
	// not final (rule 1).
	InputAfter bool
	// Stopped: a stop was asked for.
	Stopped bool
	// Legacy is legacyFinal's text when there is no exit result, else "":
	// what an ended run is judged on instead of NO_RESULT.
	Legacy string
	// OverParkCap: it parked more than its limits.maxParks times, so it
	// ends failed, PARK_LIMIT (overParkCap).
	OverParkCap bool
	// Expired / Moved: core ended the run waiting on a decision (ExpireRun,
	// EndMovedRun), read from the state its decisions were closed as.
	Expired, Moved bool
}

// LastExitResult reads the session's latest exit result (ExitResult).
func (s *Store) LastExitResult(ctx context.Context, sessionID string) (ExitResult, error) {
	var out ExitResult
	var text, legacy *string
	closedAs := func(state string) string {
		return `EXISTS (SELECT 1 FROM decisions d WHERE d.session_id = s.id AND d.state = '` + state + `'
			AND d.kind IN ('question', 'permission', 'plan'))`
	}
	err := s.pool.QueryRow(ctx, `SELECT (SELECT e.text FROM `+exitResultOf+` e), `+inputAfterExit+`, s.stop_requested_at IS NOT NULL, `+legacyFinal+`,
		`+closedAs("expired")+`, `+closedAs("stale")+`, `+overParkCap+`
		FROM sessions s WHERE s.id = $1`, sessionID).Scan(&text, &out.InputAfter, &out.Stopped, &legacy, &out.Expired, &out.Moved, &out.OverParkCap)
	if err != nil {
		return out, fmt.Errorf("last exit result: %w", err)
	}
	if text != nil {
		out.Text, out.Found = *text, true
	} else if legacy != nil {
		out.Legacy = *legacy
	}
	return out, nil
}

// ListEndedRuns is every session endedRun matches, oldest first.
func (s *Store) ListEndedRuns(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT s.id::text FROM sessions s WHERE `+endedRun+` ORDER BY s.created_at`, livePhases)
	if err != nil {
		return nil, fmt.Errorf("list ended runs: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list ended runs: %w", err)
	}
	return ids, nil
}

// ClaimEndedRun archives id if endedRun still holds, re-checked in the
// UPDATE itself, and withdraws its open decisions and unused grants as
// Archive does. false: something changed since ListEndedRuns (input
// arrived, a warm reserved a pod), and the run is left alone.
//
// Archiving before the teardown and sweep is what closes the window:
// ReserveSlot takes only an unarchived row and writes a live pod_phase on
// it, so a claim and a warm serialize on the row lock and exactly one wins.
func (s *Store) ClaimEndedRun(ctx context.Context, id string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("claim ended run: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `UPDATE sessions s SET archived_at = COALESCE(s.archived_at, now()), updated_at = now()
		WHERE s.id = $2 AND `+endedRun, livePhases, id)
	if err != nil {
		return false, fmt.Errorf("claim ended run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := withdrawDecisions(ctx, tx, id); err != nil {
		return false, fmt.Errorf("claim ended run: withdraw decisions: %w", err)
	}
	return true, tx.Commit(ctx)
}

// IdleRun is a single-shot run with no live pod, neither archived nor swept:
// what sessions.Loop's expireRuns judges (docs/adr/0070 §6).
type IdleRun struct {
	ID, Definition, SHA string
	// OpenFor is how long its oldest open question, permission or plan has
	// waited; nil with none.
	OpenFor *time.Duration
	// AnswerTimeout is its pinned version's limits.answerTimeout, "" unset.
	AnswerTimeout string
}

// ListIdleRuns is every IdleRun.
func (s *Store) ListIdleRuns(ctx context.Context) ([]IdleRun, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.id::text, COALESCE(s.definition_name, ''), COALESCE(s.definition_sha, ''),
		       (SELECT EXTRACT(EPOCH FROM now() - min(d.created_at))::float8 FROM decisions d
		         WHERE d.session_id = s.id AND d.state = 'open' AND d.kind IN ('question', 'permission', 'plan')),
		       COALESCE(a.effective -> 'limits' ->> 'answerTimeout', '')
		  FROM sessions s
		  LEFT JOIN agent_definitions a ON a.name = s.definition_name AND a.sha = s.definition_sha
		 WHERE s.lifecycle = 'single-shot' AND s.archived_at IS NULL AND s.swept_at IS NULL
		   AND COALESCE(s.pod_phase, '') <> ALL($1)`, livePhases)
	if err != nil {
		return nil, fmt.Errorf("list idle runs: %w", err)
	}
	defer rows.Close()
	var out []IdleRun
	for rows.Next() {
		var r IdleRun
		var openSec *float64
		if err := rows.Scan(&r.ID, &r.Definition, &r.SHA, &openSec, &r.AnswerTimeout); err != nil {
			return nil, fmt.Errorf("scan idle run: %w", err)
		}
		if openSec != nil {
			d := time.Duration(*openSec * float64(time.Second))
			r.OpenFor = &d
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ExpireRun ends the single-shot run id expired (docs/adr/0070 §6) while it
// still has no live pod and a question, permission or plan open longer than
// after: it is archived, those decisions go 'expired' (what WaitForResult
// reads), and unused grants are cleared as Archive does. endRuns then tears
// it down and sweeps it (§7). false: the run moved on (answered, warmed,
// ended) since it was listed.
func (s *Store) ExpireRun(ctx context.Context, id string, after time.Duration) (bool, error) {
	return s.endIdleRun(ctx, id, "expired", after.String())
}

// EndMovedRun ends the single-shot run id, pinned to a definition version
// that is no longer current, while it is parked (no live pod, a question,
// permission or plan open): it can never warm again, so core ends it failed
// (docs/adr/0070 §6). Its open decisions go 'stale' (superseded, as a card
// is by a newer one), which WaitForResult reads as failed,
// DEFINITION_CHANGED; endRuns then runs §7. A run on a moved pin with
// nothing open is not parked and is left as it was (§6 ends parked runs).
// ponytail: the exit-window / capacity-pending case stays on its earlier
// path; end it here too only with a reason that needs no open decision.
func (s *Store) EndMovedRun(ctx context.Context, id string) (bool, error) {
	return s.endIdleRun(ctx, id, "stale", "")
}

// endIdleRun archives id with the IdleRun condition re-checked in the UPDATE
// (so a warm and this serialize on the row, as ClaimEndedRun's claim does),
// only while it is parked (with after != "", on a decision open that long), and closes
// its open questions, permissions and plans as closeAs.
func (s *Store) endIdleRun(ctx context.Context, id, closeAs, after string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("end idle run: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `UPDATE sessions s SET archived_at = now(), updated_at = now()
		WHERE s.id = $2 AND s.lifecycle = 'single-shot' AND s.archived_at IS NULL AND s.swept_at IS NULL
		  AND COALESCE(s.pod_phase, '') <> ALL($1)
		  AND EXISTS (SELECT 1 FROM decisions d WHERE d.session_id = s.id AND d.state = 'open'
		        AND d.kind IN ('question', 'permission', 'plan') AND ($3 = '' OR d.created_at <= now() - NULLIF($3, '')::interval))`,
		livePhases, id, after)
	if err != nil {
		return false, fmt.Errorf("end idle run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE decisions SET state = $2, version = nextval('decisions_version_seq')
		WHERE session_id = $1 AND state = 'open' AND kind IN ('question', 'permission', 'plan')`, id, closeAs); err != nil {
		return false, fmt.Errorf("end idle run: close decisions: %w", err)
	}
	if err := withdrawDecisions(ctx, tx, id); err != nil {
		return false, fmt.Errorf("end idle run: withdraw decisions: %w", err)
	}
	return true, tx.Commit(ctx)
}

// ParkedRuns counts owner's parked single-shot runs (docs/adr/0070 §6, "No
// migration"): not archived, no live pod, and an open question, permission
// or plan. Invoke's maxParked reads it; it is the one SQL spelling of parked
// (build order slice 9 reuses parkedWhere for its refusals).
func (s *Store) ParkedRuns(ctx context.Context, owner string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sessions s WHERE s.owner = $1 AND s.archived_at IS NULL AND `+parkedWhere,
		owner, livePhases).Scan(&n)
	return n, err
}

// parkedWhere is "s is a parked run", with the live pod phases as $2.
const parkedWhere = `s.lifecycle = 'single-shot'
	AND NOT (COALESCE(s.pod_phase, '') = ANY($2))
	AND EXISTS (SELECT 1 FROM decisions d WHERE d.session_id = s.id AND d.state = 'open'
		AND d.kind IN ('question', 'permission', 'plan'))`

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
// Returns ErrAtCapacity when the fleet is full (wrapped with which cap for a
// definition session), ErrDefinitionInvalid for a session pinned to a revoked
// definition row, and ErrNotFound for an unknown or archived session. Every
// check runs before any write, so a refusal leaves no lease and no phase.
func (s *Store) ReserveSlot(ctx context.Context, id string, caps Caps) (leaseID string, err error) {
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
		WHERE pod_phase = ANY($1) AND archived_at IS NULL AND lifecycle IS DISTINCT FROM 'standing'
	`, livePhases).Scan(&live); err != nil {
		return "", fmt.Errorf("reserve slot: count: %w", err)
	}

	// Definition sessions also draw from the definition pool and their own
	// limits.concurrency (docs/adr/0062), counted under the same lock.
	var (
		defName, owner          *string
		invoker                 *string
		stopped, defValid       bool
		standing                bool
		concurrency             *int
		poolLive, defLive       int
		externalLive, ownerLive int
		standingLive            int
	)
	// defLive counts only external sessions of this definition when this one
	// is external (docs/adr/0066 §8): limits.concurrency bounds members and
	// clients, never an admin's console session.
	err = tx.QueryRow(ctx, `
		SELECT s.definition_name, s.owner, s.invoker, s.stopped_at IS NOT NULL, COALESCE(d.valid, false),
		       (d.effective->'limits'->>'concurrency')::int, s.lifecycle IS NOT DISTINCT FROM 'standing',
		       (SELECT count(*) FROM sessions o WHERE o.pod_phase = ANY($2) AND o.archived_at IS NULL
		                                          AND o.definition_name IS NOT NULL AND o.lifecycle IS DISTINCT FROM 'standing'),
		       (SELECT count(*) FROM sessions o WHERE o.pod_phase = ANY($2) AND o.archived_at IS NULL
		                                          AND o.definition_name = s.definition_name AND o.lifecycle IS DISTINCT FROM 'standing'
		                                          AND (s.owner IS NULL OR o.owner IS NOT NULL)),
		       (SELECT count(*) FROM sessions o WHERE o.pod_phase = ANY($2) AND o.archived_at IS NULL
		                                          AND o.definition_name IS NOT NULL AND o.owner IS NOT NULL
		                                          AND o.owner NOT LIKE 'client:standing:%'
		                                          AND o.lifecycle IS DISTINCT FROM 'standing'),
		       (SELECT count(*) FROM sessions o WHERE o.pod_phase = ANY($2) AND o.archived_at IS NULL
		                                          AND o.owner = s.owner AND o.lifecycle IS DISTINCT FROM 'standing'),
		       (SELECT count(*) FROM sessions o WHERE o.pod_phase = ANY($2) AND o.archived_at IS NULL
		                                          AND o.lifecycle = 'standing')
		  FROM sessions s
		  LEFT JOIN agent_definitions d ON d.name = s.definition_name AND d.sha = s.definition_sha
		 WHERE s.id = $1 AND s.archived_at IS NULL
	`, id, livePhases).Scan(&defName, &owner, &invoker, &stopped, &defValid, &concurrency, &standing, &poolLive, &defLive, &externalLive, &ownerLive, &standingLive)
	if err == pgx.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reserve slot: definition: %w", err)
	}
	if stopped {
		return "", ErrStopped
	}
	if standing {
		if !defValid {
			return "", ErrDefinitionInvalid
		}
		if standingLive >= caps.MaxStanding {
			return "", fmt.Errorf("%w: standing pool full (%d/%d)", ErrAtCapacity, standingLive, caps.MaxStanding)
		}
	} else if live >= caps.MaxLive {
		return "", fmt.Errorf("%w (%d live sessions) — stop one first", ErrAtCapacity, caps.MaxLive)
	}
	if defName != nil && !standing {
		// The revocation path proper is WarmIfIdle's pinned==current check;
		// this catches a re-judge landing between that check and this
		// transaction, and keeps a NULL concurrency out of the comparison.
		if !defValid || concurrency == nil {
			return "", ErrDefinitionInvalid
		}
		if poolLive >= caps.MaxDefinition {
			return "", fmt.Errorf("%w: definition pool full (%d/%d)", ErrAtCapacity, poolLive, caps.MaxDefinition)
		}
		if defLive >= *concurrency {
			return "", fmt.Errorf("%w: definition %q at its concurrency limit (%d)", ErrAtCapacity, *defName, *concurrency)
		}
		// A character's spawned run draws from the definition pool and its own sub-pool (docs/adr/0084 §6), never
		// from the clients' MaxExternal: the config check (a sync that would overfill MAX_DEFINITION_SESSIONS is refused) sizes the pool for the sub-pools, so
		// the clients' share must not eat it; client runs can still fill MaxDefinition, and then a spawn waits at the pool check.
		spawn := invoker != nil && strings.HasPrefix(*invoker, StandingPrefix)
		if spawn {
			var spawnLive, spawnCap int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM sessions o WHERE o.invoker = $1 AND o.lifecycle = 'single-shot'
				AND o.pod_phase = ANY($2) AND o.archived_at IS NULL AND o.id::text <> $3`, *invoker, livePhases, id).Scan(&spawnLive); err != nil {
				return "", fmt.Errorf("reserve slot: spawn count: %w", err)
			}
			if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT (d.effective->'limits'->'spawns'->>'concurrent')::int
				FROM sessions c JOIN agent_definitions d ON d.name = c.definition_name AND d.sha = c.definition_sha
				WHERE c.lifecycle = 'standing' AND c.archived_at IS NULL AND c.invoker = $1), 1)`, *invoker).Scan(&spawnCap); err != nil {
				return "", fmt.Errorf("reserve slot: spawn cap: %w", err)
			}
			if spawnLive >= spawnCap {
				return "", fmt.Errorf("%w: %s's spawn sub-pool full (%d/%d)", ErrAtCapacity, *invoker, spawnLive, spawnCap)
			}
		}
		if owner != nil && !spawn {
			if externalLive >= caps.MaxExternal {
				return "", fmt.Errorf("%w: member and client pool full (%d/%d)", ErrAtCapacity, externalLive, caps.MaxExternal)
			}
			// A client's sessions share MaxExternal only; a member also has
			// their own share.
			if !strings.HasPrefix(*owner, "client:") && ownerLive >= caps.MaxPerMember {
				return "", fmt.Errorf("%w: you already have %d live session(s)", ErrAtCapacity, ownerLive)
			}
		}
	}

	err = tx.QueryRow(ctx, `
		UPDATE sessions
		-- A definition session's lease goes in profile_lease_id, never
		-- lease_id, which every older core verifies (docs/adr/0064, 000009).
		SET lease_id = CASE WHEN definition_name IS NULL THEN v.lease END,
		    profile_lease_id = CASE WHEN definition_name IS NOT NULL THEN v.lease END,
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
		FROM (SELECT gen_random_uuid() AS lease) v
		WHERE id = $1 AND archived_at IS NULL
		RETURNING v.lease::text
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
	// Only an older worker's requests. One stamped "nb": 1 (docs/adr/0082 §5)
	// came from a worker that never blocks on it: its decision is the owner's
	// until answered or withdrawn, and its answer reaches whichever pod comes
	// next as a message, as a question's does. An older worker's request has
	// no decisions row stamped so, or none at all if it predates the table.
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
		   AND NOT EXISTS (
		         SELECT 1 FROM decisions d
		          WHERE d.session_id = q.session_id AND d.request_seq = q.seq AND d.payload -> 'nb' = '1'::jsonb
		       )
		ON CONFLICT DO NOTHING
	`, id); err != nil {
		return "", fmt.Errorf("reserve slot: resolve stale decisions: %w", err)
	}
	// The same requests in the decisions table (docs/adr/0082 §5), which the
	// insert above bypasses: it writes no transcript append, so no hook runs.
	// A permission or plan whose pod is gone is withdrawn, not answered.
	if _, err := tx.Exec(ctx, `
		UPDATE decisions SET state = 'withdrawn', version = nextval('decisions_version_seq')
		 WHERE session_id = $1 AND state = 'open' AND kind IN ('permission', 'plan')
		   AND payload -> 'nb' IS DISTINCT FROM '1'::jsonb
	`, id); err != nil {
		return "", fmt.Errorf("reserve slot: withdraw stale decisions: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("reserve slot: commit: %w", err)
	}
	pool, n, limit := "live", live+1, caps.MaxLive
	if standing {
		pool, n, limit = "standing", standingLive+1, caps.MaxStanding
	}
	slog.Info("sessions ReserveSlot", "sessionId", id, "pool", pool, "count", n, "cap", limit)
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

// LivePhases exposes the same set to other packages, as a copy: the slice
// backs ReserveSlot's concurrency cap and all four sweeps, and an exported var
// would let any importer mutate it out from under them.
//
// The callers outside this package are proposals.Create, which needs to know
// whether a schedule's previous run still has a pod, and the bridge's
// dispatcher, whose gate asks whether a thread waits on a person. They read
// this rather than re-spelling the enum so they cannot drift.
func LivePhases() []string {
	return slices.Clone(livePhases)
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
		UPDATE sessions SET pod_phase = $2, pod_message = $3, updated_at = now(),
		       -- docs/adr/0084 §3: every terminal path of a character stamps its re-warm clock.
		       restart_after = CASE WHEN lifecycle = 'standing' AND $2 IN ('POD_PHASE_TERMINATED', 'POD_PHASE_CRASHED')
		                            THEN now() ELSE restart_after END
		WHERE id = $1
	`, id, phase, msg)
	if err != nil {
		slog.Error("sessions SetPodPhase", "sessionId", id, "phase", phase, "error", err)
		return fmt.Errorf("set pod phase: %w", err)
	}
	return nil
}

// SetPodPhaseIf is SetPodPhase that only writes while the row still reads
// `from` (nil: no phase yet), reporting whether it did. The reconcile loop
// decides from a snapshot; without this a stale snapshot overwrites a phase a
// locked path (a model switch's TERMINATED) wrote in between.
func (s *Store) SetPodPhaseIf(ctx context.Context, id string, from *string, phase, message string) (bool, error) {
	var msg *string
	if message != "" {
		msg = &message
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE sessions SET pod_phase = $2, pod_message = $3, updated_at = now(),
		       restart_after = CASE WHEN lifecycle = 'standing' AND $2 IN ('POD_PHASE_TERMINATED', 'POD_PHASE_CRASHED')
		                            THEN now() ELSE restart_after END
		WHERE id = $1 AND pod_phase IS NOT DISTINCT FROM $4
	`, id, phase, msg, from)
	if err != nil {
		slog.Error("sessions SetPodPhaseIf", "sessionId", id, "phase", phase, "error", err)
		return false, fmt.Errorf("set pod phase: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetPodImages records the images the provisioner actually stamped onto this
// session's pod, and the release that built it, straight from
// CreateWorkerPodResponse.
//
// Not lease-scoped, unlike SaveAgentSessionID: this is written synchronously by
// the same call that created the pod, before that pod can report anything, so
// there is no losing writer to guard against.
func (s *Store) SetPodImages(ctx context.Context, id, workerImage, sidecarImage, fleetVersion string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sessions SET worker_image = $2, sidecar_image = $3, fleet_version = $4, pod_started_at = now(), updated_at = now() WHERE id = $1
	`, id, workerImage, sidecarImage, fleetVersion)
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
		SET agent_session_id = $4,
		    -- A definition session's model is its pinned model.default
		    -- (docs/adr/0062): what the pod reports never widens it.
		    model = CASE WHEN definition_name IS NULL THEN COALESCE($5, model) ELSE model END,
		    updated_at = now()
		WHERE id = $1 AND `+leaseMatch, id, leaseID, livePhases, agentSessionID, modelPtr)
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
//
// definition is the session's definition name ("" for a repo session), read in
// the same lookup: it is what narrows a definition lease to its allowlist
// (docs/adr/0064).
//
// trustSet is the session's stored Rule of Two letters, read in the same lookup (docs/adr/0086 §2).
//
// A definition lease (profile_lease_id) counts only while the row says a pod
// is live. An older core (image rollback, rollout overlap) tears a definition
// pod down with a ClearLease that nulls lease_id alone, so profile_lease_id
// outlives the pod; the phase that core writes still revokes it. A repo lease
// keeps 0057's rule: valid until cleared or rotated.
//
// leaseMatch is that rule, shared with SaveAgentSessionID so the two cannot
// drift: $2 is the lease, $3 livePhases.
const leaseMatch = `(lease_id::text = $2 OR (profile_lease_id::text = $2 AND pod_phase = ANY($3)))`

func (s *Store) VerifyLease(ctx context.Context, id, leaseID string) (ok bool, definition, trustSet string, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(definition_name, ''), trust_set FROM sessions
		WHERE id = $1 AND `+leaseMatch, id, leaseID, livePhases).Scan(&definition, &trustSet)
	if err == pgx.ErrNoRows {
		return false, "", "", nil
	}
	if err != nil {
		slog.Error("sessions VerifyLease", "sessionId", id, "error", err)
		return false, "", "", fmt.Errorf("verify lease: %w", err)
	}
	return true, definition, trustSet, nil
}

// ClearLease revokes a session's credential at teardown.
//
// Without it the lease stays valid until the NEXT warm rotates it, so a
// deleted pod keeps a working credential for as long as the session then sits
// idle — which is most of the time. Clearing here is what makes the lease a
// revocable credential rather than merely a rotating one.
func (s *Store) ClearLease(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET lease_id = NULL, profile_lease_id = NULL WHERE id = $1`, id)
	if err != nil {
		slog.Error("sessions ClearLease", "sessionId", id, "error", err)
		return fmt.Errorf("clear lease: %w", err)
	}
	return nil
}

// RestoreLease hands a running pod back the lease a warm's ReserveSlot
// replaced, when the create found that pod still running. Only while the
// minted lease is still the row's: a later warm's lease is never overwritten.
// Repo sessions only: a definition session's refused create is torn down
// (refuseDefinitionPod).
func (s *Store) RestoreLease(ctx context.Context, id, minted, prev string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET lease_id = NULLIF($3, '')::uuid WHERE id = $1 AND lease_id::text = $2`, id, minted, prev)
	if err != nil {
		return fmt.Errorf("restore lease: %w", err)
	}
	return nil
}

// PinnedEffective is the effective spec (JSON) of the definition version a
// session is pinned to (docs/adr/0067 §4), and the session row's lifecycle
// ("" when none is set, which reads as resident): nil spec for a repo session,
// ErrDefinitionInvalid when that version was revoked, ErrNotFound for an
// unknown session.
func (s *Store) PinnedEffective(ctx context.Context, id string) (eff []byte, lifecycle string, err error) {
	var pinned, valid bool
	err = s.pool.QueryRow(ctx, `
		SELECT s.definition_name IS NOT NULL, COALESCE(d.valid, false), d.effective, COALESCE(s.lifecycle, '')
		  FROM sessions s
		  LEFT JOIN agent_definitions d ON d.name = s.definition_name AND d.sha = s.definition_sha
		 WHERE s.id = $1`, id).Scan(&pinned, &valid, &eff, &lifecycle)
	switch {
	case err == pgx.ErrNoRows:
		return nil, "", ErrNotFound
	case err != nil:
		return nil, "", fmt.Errorf("pinned effective: %w", err)
	case !pinned:
		return nil, "", nil
	case !valid || eff == nil:
		return nil, "", ErrDefinitionInvalid
	}
	return eff, lifecycle, nil
}

// SetPermissionMode persists a session's mode. For a definition session the
// mode must be in its pinned, still-valid version's mode.allow
// (docs/adr/0062): checked in the same statement, so both callers (the
// console's DashboardService and the pod's CoreService) go through one guard,
// and a definition only narrows, for admins too. ErrModeNotAllowed when it is
// outside, ErrDefinitionInvalid when the pinned version was revoked,
// ErrNotFound for an unknown session.
func (s *Store) SetPermissionMode(ctx context.Context, id, mode string) error {
	var allowed, revoked bool
	err := s.pool.QueryRow(ctx, `
		WITH t AS (
		  SELECT s.id, s.definition_name IS NULL
		         OR COALESCE(d.valid AND d.effective->'mode'->'allow' ? $2, false) AS allowed,
		         COALESCE(NOT d.valid, false) AS revoked
		    FROM sessions s
		    LEFT JOIN agent_definitions d ON d.name = s.definition_name AND d.sha = s.definition_sha
		   WHERE s.id = $1
		), u AS (
		  UPDATE sessions SET permission_mode = $2, updated_at = now()
		    FROM t WHERE sessions.id = t.id AND t.allowed
		)
		SELECT allowed, revoked FROM t`, id, mode).Scan(&allowed, &revoked)
	switch {
	case err == pgx.ErrNoRows:
		return ErrNotFound
	case err != nil:
		slog.Error("sessions SetPermissionMode", "sessionId", id, "error", err)
		return fmt.Errorf("set permission mode: %w", err)
	case revoked:
		return ErrDefinitionInvalid
	case !allowed:
		return fmt.Errorf("%w: %q", ErrModeNotAllowed, mode)
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
		  -- docs/adr/0084 §3: a character is exempt, but the owner's Stop (stopped_at) still ends its pod.
		  AND (lifecycle IS DISTINCT FROM 'standing' OR stopped_at IS NOT NULL)
	`, grace, livePhases)
}

// ListStartupStalledIDs finds sessions whose pod came up and never said
// anything (docs/adr/0040). Gated on activity_seen, which ReserveSlot
// resets per pod — see the comment there for why it cannot be derived.
func (s *Store) ListStartupStalledIDs(ctx context.Context, startupStall time.Duration) ([]string, error) {
	return s.listIDs(ctx, startupStalled+` AND lifecycle IS DISTINCT FROM 'standing'`, startupStall, livePhases)
}

// ListStalledStandingIDs is ListStartupStalledIDs for the characters it skips: core alerts for these, it does not
// tear them down (docs/adr/0084 §3).
func (s *Store) ListStalledStandingIDs(ctx context.Context, startupStall time.Duration) ([]string, error) {
	return s.listIDs(ctx, startupStalled+` AND lifecycle = 'standing' AND stopped_at IS NULL`, startupStall, livePhases)
}

const startupStalled = `
		SELECT id FROM sessions
		WHERE NOT activity_seen
		  AND pod_phase = ANY($2)
		  AND updated_at < now() - $1::interval`

// ListIdleSessionIDs finds sessions with a live pod nobody is talking to:
// idle past idleTimeout, or for a definition session past its pinned
// version's limits.timeout (docs/adr/0065).
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
	// And an older worker's only. A request stamped "nb": 1 (docs/adr/0082
	// §5) holds no canUseTool promise past DECISION_WAIT_MS: its answer is a
	// message the next pod receives, so its pod is as free to die as a
	// question's.
	//
	// Written out rather than sharing selectCols' pending_decisions fragment:
	// that one is a count over every kind, this is a filter over two, and
	// gluing SQL strings together to save a duplicate NOT EXISTS is how a
	// WHERE clause ends up applying to the wrong query.
	//
	// limits.timeout is a Go duration string, compared here rather than cast
	// in SQL: one string Postgres cannot read as an interval would fail the
	// whole sweep, fleet-wide. One that does not parse falls back to
	// idleTimeout. Live pods are bounded by MaxLive, so scanning them all is
	// cheap.
	rows, err := s.pool.Query(ctx, `
		SELECT s.id, EXTRACT(EPOCH FROM now() - s.last_active_at)::float8,
		       d.effective->'limits'->>'timeout'
		  FROM sessions s
		  LEFT JOIN agent_definitions d ON d.name = s.definition_name AND d.sha = s.definition_sha
		WHERE s.pod_phase = ANY($1)
		  AND s.last_active_at IS NOT NULL
		  AND s.lifecycle IS DISTINCT FROM 'standing' -- docs/adr/0084 §3: a character is kept up, never idled out
		  AND NOT EXISTS (
		    SELECT 1 FROM decisions d
		     WHERE d.session_id = s.id AND d.state = 'open' AND d.kind IN ('permission', 'plan')
		       AND d.payload -> 'nb' IS DISTINCT FROM '1'::jsonb
		  )
	`, livePhases)
	if err != nil {
		return nil, fmt.Errorf("list idle sessions: %w", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var (
			id      string
			idleSec float64
			timeout *string
		)
		if err := rows.Scan(&id, &idleSec, &timeout); err != nil {
			return nil, fmt.Errorf("scan idle session: %w", err)
		}
		limit := idleTimeout
		if timeout != nil {
			if d, err := time.ParseDuration(*timeout); err == nil && d > 0 {
				limit = d
			}
		}
		if time.Duration(idleSec*float64(time.Second)) > limit {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// ListRetentionExpired finds sessions whose disk the GC should reclaim:
// idle past the retention window, with no live pod, not already swept. A
// parked run is skipped: limits.answerTimeout bounds it instead, and its end
// runs §7 (docs/adr/0070 §6).
func (s *Store) ListRetentionExpired(ctx context.Context, retention time.Duration) ([]string, error) {
	return s.listIDs(ctx, `
		SELECT s.id FROM sessions s
		WHERE s.swept_at IS NULL
		  AND NOT (s.pod_phase = ANY($2))
		  AND COALESCE(s.last_active_at, s.created_at) < now() - $1::interval
		  AND NOT COALESCE(s.archived_at IS NULL AND `+parkedWhere+`, false)
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
//
// A swept session cannot be resumed, so nothing it asked can be acted on: in
// the same transaction its open decisions are withdrawn and its unused
// one-time grants cleared, as Archive does (docs/adr/0082 §5).
func (s *Store) MarkSwept(ctx context.Context, id string) error {
	if err := s.markSwept(ctx, id); err != nil {
		slog.Error("sessions MarkSwept", "sessionId", id, "error", err)
		return fmt.Errorf("mark swept: %w", err)
	}
	return nil
}

func (s *Store) markSwept(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE sessions SET swept_at = now(), updated_at = now() WHERE id = $1`, id); err != nil {
		return err
	}
	if err := withdrawDecisions(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// withdrawDecisions closes a finished session's open decisions and clears
// its unused one-time grants; a spent grant keeps its record.
func withdrawDecisions(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `
		UPDATE decisions
		   SET state     = CASE WHEN state = 'open' THEN 'withdrawn' ELSE state END,
		       grantable = false,
		       version   = nextval('decisions_version_seq')
		 WHERE session_id = $1 AND (state = 'open' OR (grantable AND grant_used_at IS NULL))`, id)
	return err
}

// Archive marks a session finished by a human — the only terminal state in
// the fleet, because it is the only one a machine cannot compute.
//
// In the same transaction its open decisions are withdrawn and its unused
// one-time grants cleared (docs/adr/0082 §5): a grant lives until its
// session ends, and nothing waits on a finished session's owner.
func (s *Store) Archive(ctx context.Context, id string) error {
	if err := s.archive(ctx, id); err != nil {
		if !errors.Is(err, ErrNotFound) {
			slog.Error("sessions Archive", "sessionId", id, "error", err)
		}
		return err
	}
	slog.Info("sessions Archive", "sessionId", id)
	return nil
}

func (s *Store) archive(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("archive session: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `
		UPDATE sessions SET archived_at = COALESCE(archived_at, now()), updated_at = now()
		WHERE id = $1
	`, id)
	if err != nil {
		return fmt.Errorf("archive session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := withdrawDecisions(ctx, tx, id); err != nil {
		return fmt.Errorf("archive session: withdraw decisions: %w", err)
	}
	return tx.Commit(ctx)
}

// Delete removes the row and its transcript for real.
//
// A hard DELETE, where the old schema could only soft-delete: transcript
// REFERENCEs sessions ON DELETE CASCADE and proposals ON DELETE SET NULL,
// so a session with history can actually be removed. That FK pair was the
// entire reason deleted_at existed.
func (s *Store) Delete(ctx context.Context, id string) error {
	// An open card the bridge already has gets no terminal event once its row cascades away: count it.
	var lost int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM decisions WHERE session_id = $1 AND kind = 'card' AND state = 'open' AND bridge_state = 'open'`, id).Scan(&lost); err != nil {
		slog.Error("sessions Delete: count published open cards", "sessionId", id, "error", err)
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	if err != nil {
		slog.Error("sessions Delete", "sessionId", id, "error", err)
		return fmt.Errorf("delete session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	metrics.CardEventsLost.Add(float64(lost))
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

// Standing is the lifecycle of a character (docs/adr/0084 §1), and StandingPrefix the principal prefix its row is
// owned under.
const (
	Standing       = "standing"
	StandingPrefix = "standing:"
)

// StandingByDefinition is the one live standing row of a character, ErrNotFound when none was ever started.
func (s *Store) StandingByDefinition(ctx context.Context, name string) (*Session, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+selectCols+` FROM sessions s
		WHERE s.lifecycle = 'standing' AND s.definition_name = $1 AND s.archived_at IS NULL`, name)
	sess, err := scanSession(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("standing session: %w", err)
	}
	return sess, nil
}

// RestartStanding is Start on a stopped character (docs/adr/0084 §2, amended A4): the row is un-stopped and
// re-pinned to sha. Every thread keeps its SDK session id and its cursor, so the next warm resumes each thread with
// its context and no waiting entry is lost; a stopped in-flight turn was settled by Stop (MarkStandingStopped's
// caller). Only the crash counters restart. The transcript and the memory stay.
// False when the row is not stopped or its old pod is still live: a Stop's grace-period teardown must run first,
// else Start would disarm it (ListOverdueStopIDs) and the old pod could write its SDK session id back.
func (s *Store) RestartStanding(ctx context.Context, id, sha, by string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("restart standing: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	tag, err := tx.Exec(ctx, `
		UPDATE sessions SET stopped_at = NULL, stop_requested_at = NULL, started_by = $3, definition_sha = $2,
		       restart_after = NULL, drain_started_at = NULL, updated_at = now(),
		       restart_seq = (SELECT COALESCE(MAX(seq), -1) FROM transcript WHERE session_id = $1)
		WHERE id = $1 AND lifecycle = 'standing' AND stopped_at IS NOT NULL AND COALESCE(pod_phase, '') <> ALL($4)`,
		id, sha, by, livePhases)
	if err != nil {
		return false, fmt.Errorf("restart standing: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE session_threads SET crash_seq = NULL, crashes = 0, crash_agent_seq = NULL WHERE session_id = $1`, id); err != nil {
		return false, fmt.Errorf("restart standing: %w", err)
	}
	return true, tx.Commit(ctx)
}

// MarkStandingStopped is Stop (docs/adr/0084 §2): the cooperative stop request, plus the mark that keeps the row
// down. False when the row is not standing or is already stopped.
func (s *Store) MarkStandingStopped(ctx context.Context, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE sessions SET stopped_at = now(), stop_requested_at = COALESCE(stop_requested_at, now()), restart_after = NULL,
		       drain_started_at = NULL, updated_at = now()
		WHERE id = $1 AND lifecycle = 'standing' AND stopped_at IS NULL`, id)
	if err != nil {
		return false, fmt.Errorf("stop standing: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SpawnsSince counts the runs a character (invoker standing:<name>) started in the last d, the per-hour and per-day
// caps of limits.spawns (docs/adr/0084 §6). An archived run counts: the cap is on starts.
func (s *Store) SpawnsSince(ctx context.Context, invoker string, d time.Duration) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE invoker = $1 AND lifecycle = 'single-shot'
		AND created_at > now() - make_interval(secs => $2)`, invoker, d.Seconds()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count spawns: %w", err)
	}
	return n, nil
}

// SpawnEnd is a spawned run whose character has not yet been told it ended.
type SpawnEnd struct {
	ID        string
	Character string // the character's definition name
	Run       string // the run's definition name
}

// SpawnEnds lists the archived (ended) spawned runs of the last two days not yet reported, oldest first, up to
// limit; a run still going never sits at the head. The caller
// judges which ended (WaitForResult's rule) and reports each with MarkSpawnReported.
func (s *Store) SpawnEnds(ctx context.Context, limit int) ([]SpawnEnd, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text, substring(invoker from char_length($1) + 1), COALESCE(definition_name, '')
		FROM sessions WHERE lifecycle = 'single-shot' AND invoker LIKE $1 || '%' AND spawn_reported_at IS NULL AND archived_at IS NOT NULL
		AND created_at > now() - interval '2 days' ORDER BY created_at LIMIT $2`, StandingPrefix, limit)
	if err != nil {
		return nil, fmt.Errorf("list spawn ends: %w", err)
	}
	defer rows.Close()
	var out []SpawnEnd
	for rows.Next() {
		var e SpawnEnd
		if err := rows.Scan(&e.ID, &e.Character, &e.Run); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkSpawnReported records that the character was told run id ended.
func (s *Store) MarkSpawnReported(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx, `UPDATE sessions SET spawn_reported_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("mark spawn reported: %w", err)
	}
	return nil
}

// SetModel saves a repo session's model (SetSessionModel). A definition
// session's model is its pinned model.default, so none is written for one; the
// caller refuses those first, and false says the row was not changed.
func (s *Store) SetModel(ctx context.Context, id, model string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE sessions SET model = $2, updated_at = now() WHERE id = $1 AND definition_name IS NULL`, id, model)
	if err != nil {
		slog.Error("sessions SetModel", "sessionId", id, "error", err)
		return false, fmt.Errorf("set model: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
