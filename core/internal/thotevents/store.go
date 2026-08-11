// Package thotevents is the durable store for thot's own activity stream
// (docs/adr/0035) — findings, alerts, audit runs, and the canUseTool
// permission prompt/decision pair.
//
// It deliberately mirrors internal/transcript's pull/cursor read shape
// (docs/adr/0013: never a bare streaming-watch without a resume cursor)
// rather than sharing its table — see db/migrations/000005 for why thot's
// stream can't live in the task-scoped `transcript`.
package thotevents

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event kinds. Kept as constants rather than bare strings at call sites so
// a typo is a compile error instead of a CHECK-constraint violation at
// runtime.
const (
	KindPermissionRequest  = "permission_request"
	KindPermissionResponse = "permission_response"
	KindFinding            = "finding"
	KindAlert              = "alert"
	KindAuditRun           = "audit_run"
)

// DecisionAllow is the exact payload a permission_response carries when the
// human allowed the call; anything else is treated as a denial with that
// text as the reason. A sentinel rather than a bool column because the
// deny case needs to carry a message anyway, and one column beats two.
const DecisionAllow = "allow"

// Event is one entry in thot's stream. JSON tags: serialized directly by
// the dashboard API, same as transcript.Entry.
type Event struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	Actor   string `json:"actor"`
	Payload string `json:"payload"`
	// ReplyTo is non-nil only on a permission_response, pointing at the
	// permission_request it answers.
	ReplyTo *int64 `json:"replyTo,omitempty"`
	// time.Time, not string: created_at is TIMESTAMPTZ and pgx refuses to
	// scan it into a string (caught by the integration test, not review).
	// Formatting happens at the serialization boundary instead.
	CreatedAt time.Time `json:"createdAt"`
}

type Store struct {
	pool  *pgxpool.Pool
	nudge func() // optional; set via SetNudge
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// SetNudge wires an immediate-refresh trigger for whatever is watching the
// stream (the dashboard hub). Same one-shot wiring transcript.PostgresStore
// uses; a nil nudge is a valid no-op.
func (s *Store) SetNudge(nudge func()) { s.nudge = nudge }

func (s *Store) Append(ctx context.Context, kind, actor, payload, idempotencyKey string) (int64, error) {
	return s.append(ctx, kind, actor, payload, idempotencyKey, nil)
}

// AppendReply is Append plus reply-to correlation — a separate method for
// the same reason transcript.AppendReply is: the callers that never need
// it shouldn't have to pass a meaningless zero.
func (s *Store) AppendReply(ctx context.Context, kind, actor, payload, idempotencyKey string, replyTo int64) (int64, error) {
	return s.append(ctx, kind, actor, payload, idempotencyKey, &replyTo)
}

func (s *Store) append(ctx context.Context, kind, actor, payload, idempotencyKey string, replyTo *int64) (int64, error) {
	if idempotencyKey == "" {
		// Same reasoning as transcript.appendInternal: an empty key must
		// never reach the query as a literal, or every concurrent caller
		// that omitted one would collide on the single "" row. A fresh
		// UUID means "no key supplied" == "never deduplicated".
		idempotencyKey = uuid.NewString()
	}

	// No advisory lock (unlike transcript): BIGSERIAL assignment is already
	// atomic, and there's no per-scope gapless-seq contract to protect.
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO thot_events (kind, actor, payload, reply_to, idempotency_key)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (idempotency_key) DO UPDATE SET idempotency_key = EXCLUDED.idempotency_key
		RETURNING id
	`, kind, actor, payload, replyTo, idempotencyKey).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("append thot event: %w", err)
	}

	if s.nudge != nil {
		s.nudge()
	}
	return id, nil
}

// ReadSince returns every event with id >= sinceID in id order, plus the
// next cursor — the same LRANGE-ish contract transcript.ReadSince has.
func (s *Store) ReadSince(ctx context.Context, sinceID int64, limit int) ([]Event, int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, actor, payload, reply_to, created_at
		FROM thot_events
		WHERE id >= $1
		ORDER BY id
		LIMIT $2
	`, sinceID, limit)
	if err != nil {
		return nil, sinceID, fmt.Errorf("query thot events: %w", err)
	}
	defer rows.Close()

	// []Event{} not var events []Event — a nil slice marshals to JSON
	// `null`, and this is serialized straight through the dashboard API.
	events := []Event{}
	nextID := sinceID
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Kind, &e.Actor, &e.Payload, &e.ReplyTo, &e.CreatedAt); err != nil {
			return nil, sinceID, fmt.Errorf("scan thot event: %w", err)
		}
		events = append(events, e)
		nextID = e.ID + 1
	}
	if err := rows.Err(); err != nil {
		return nil, sinceID, fmt.Errorf("rows: %w", err)
	}
	return events, nextID, nil
}

// FindResponse returns the permission_response answering requestID, or nil
// if none exists yet. Used by the RequestThotPermission long-poll — a
// targeted lookup rather than rescanning the stream on every tick.
func (s *Store) FindResponse(ctx context.Context, requestID int64) (*Event, error) {
	var e Event
	err := s.pool.QueryRow(ctx, `
		SELECT id, kind, actor, payload, reply_to, created_at
		FROM thot_events
		WHERE reply_to = $1 AND kind = $2
		ORDER BY id
		LIMIT 1
	`, requestID, KindPermissionResponse).Scan(&e.ID, &e.Kind, &e.Actor, &e.Payload, &e.ReplyTo, &e.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find permission response: %w", err)
	}
	return &e, nil
}

// PendingRequests returns permission_requests with no response yet — what
// the dashboard renders as live PermissionCards on load, so a page
// refresh mid-prompt doesn't lose the pending decision.
func (s *Store) PendingRequests(ctx context.Context, limit int) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.kind, r.actor, r.payload, r.reply_to, r.created_at
		FROM thot_events r
		WHERE r.kind = $1
		  AND NOT EXISTS (
		    SELECT 1 FROM thot_events resp
		    WHERE resp.reply_to = r.id AND resp.kind = $2
		  )
		ORDER BY r.id
		LIMIT $3
	`, KindPermissionRequest, KindPermissionResponse, limit)
	if err != nil {
		return nil, fmt.Errorf("query pending thot permissions: %w", err)
	}
	defer rows.Close()

	events := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Kind, &e.Actor, &e.Payload, &e.ReplyTo, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan pending thot permission: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return events, nil
}
