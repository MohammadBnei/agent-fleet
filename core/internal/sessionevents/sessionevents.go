// Package sessionevents publishes a session's life to the event bridge:
// opened, turn_ended, ended. It follows core/internal/cardevents: the state
// the bridge accepted lives on the sessions row (bridge_state, bridge_turn_seq,
// bridge_parked), a tick publishes what differs, in order, and a retry
// carries the same ce_id so the bridge drops the duplicate.
//
// The events carry ids, kind, repo, definition, title, thread key and timings.
// Never a prompt, a reply, a result's text or a file path. Sessions and
// result entries that exist when migration 000043 runs are marked published.
//
// ended is published when a session is archived. A hard delete removes the
// row with no ended event; it is counted (agentfleet_session_events_lost_total).
package sessionevents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/MohammadBnei/agent-fleet/core/internal/bridgeclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/cardevents"
	"github.com/MohammadBnei/agent-fleet/core/internal/metrics"
	bridgev1 "github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1"
	"github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1/bridgev1connect"
)

// TypePrefix is what the bridge-core row's publish_types must allow.
const TypePrefix = "dev.bnei.fleet.session."

const turnsPerTick = 100 // results published per session per tick

type Events struct {
	pool    *pgxpool.Pool
	pub     cardevents.Publisher
	now     func() time.Time
	backoff time.Duration
	retryAt time.Time
	note    string
}

func New(pool *pgxpool.Pool, pub cardevents.Publisher, enabled bool) *Events {
	if enabled {
		metrics.SessionEventsEnabled.Set(1)
	}
	metrics.SessionEventsLastSuccess.SetToCurrentTime()
	return &Events{pool: pool, pub: pub, now: time.Now}
}

// Run ticks every interval; after a failure it backs off and never stops.
func (e *Events) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if e.now().Add(time.Second).Before(e.retryAt) {
			continue
		}
		e.settle(e.Tick(ctx))
	}
}

func (e *Events) settle(err error) {
	switch {
	case err == nil:
		e.backoff, e.retryAt, e.note = 0, time.Time{}, ""
	case errors.Is(err, context.Canceled):
	case errors.Is(err, bridgeclient.ErrNotConfigured):
		if n := err.Error(); n != e.note {
			slog.Info("sessionevents: no session events", "reason", n)
			e.note = n
		}
	default:
		e.backoff = min(max(2*e.backoff, cardevents.BackoffMin), cardevents.BackoffMax)
		e.retryAt = e.now().Add(e.backoff)
		slog.Warn("sessionevents: publish failed, backing off", "retryIn", e.backoff, "error", err)
	}
}

type session struct {
	id, repo, title, definition, lifecycle, threadKey string
	createdAt                                         time.Time
	archivedAt                                        *time.Time
	state                                             *string
	turnSeq                                           int64
}

type turn struct {
	seq       int64
	threadKey string
	at        time.Time
	text      string
}

// Tick publishes, per session in creation order: opened, its unpublished
// results in seq order, then ended. A refusal retrying cannot fix
// (InvalidArgument, PermissionDenied) parks the session; any other error
// stops the tick.
func (e *Events) Tick(ctx context.Context) error {
	rows, err := e.pool.Query(ctx, `
		SELECT s.id::text, s.repo, COALESCE(s.title, ''), COALESCE(s.definition_name, ''), COALESCE(s.lifecycle, ''),
		       COALESCE(s.thread_key, ''), s.created_at, s.archived_at, s.bridge_state, s.bridge_turn_seq
		  FROM sessions s
		 WHERE NOT s.bridge_parked AND (`+differs+` OR `+unsentTurns+`)
		 ORDER BY s.created_at, s.id LIMIT $1`, cardevents.Batch)
	if err != nil {
		return err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (session, error) {
		var s session
		err := r.Scan(&s.id, &s.repo, &s.title, &s.definition, &s.lifecycle, &s.threadKey, &s.createdAt, &s.archivedAt, &s.state, &s.turnSeq)
		return s, err
	})
	if err != nil {
		return err
	}
	var perr error
	for _, s := range list {
		if perr = e.publish(ctx, s); perr != nil {
			break
		}
	}
	// ponytail: counts sessions, not events; one session with 50 unsent turns is 1.
	var pending, parked int64
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE NOT s.bridge_parked AND (`+differs+` OR `+unsentTurns+`)),
		count(*) FILTER (WHERE s.bridge_parked) FROM sessions s`).Scan(&pending, &parked); err != nil {
		return err
	}
	metrics.SessionEventsPending.Set(float64(pending))
	metrics.SessionEventsParked.Set(float64(parked))
	if perr != nil {
		return perr
	}
	if len(list) > 0 {
		return nil
	}
	// Nothing to publish: still learn whether the bridge is configured.
	if c, ok := e.pub.(interface {
		API() (bridgev1connect.BridgeServiceClient, error)
	}); ok {
		_, err := c.API()
		return err
	}
	return nil
}

const (
	differs = `s.bridge_state IS DISTINCT FROM CASE WHEN s.archived_at IS NULL THEN 'opened' ELSE 'ended' END`
	// An empty result is not a turn.
	unsentTurns = `EXISTS (SELECT 1 FROM transcript t WHERE t.session_id = s.id AND t.type = 'result' AND t.text <> '' AND t.seq > s.bridge_turn_seq)`
)

func (e *Events) publish(ctx context.Context, s session) error {
	if s.state == nil {
		if parked, err := e.send(ctx, openedEvent(s)); err != nil || parked {
			return e.finish(ctx, s, parked, err)
		}
		if _, err := e.pool.Exec(ctx, `UPDATE sessions SET bridge_state = 'opened' WHERE id = $1::uuid AND bridge_state IS NULL`, s.id); err != nil {
			return err
		}
	}
	rows, err := e.pool.Query(ctx, `SELECT seq, thread_key, created_at, text FROM transcript
		WHERE session_id = $1::uuid AND type = 'result' AND text <> '' AND seq > $2 ORDER BY seq LIMIT $3`, s.id, s.turnSeq, turnsPerTick)
	if err != nil {
		return err
	}
	turns, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (turn, error) {
		var t turn
		return t, r.Scan(&t.seq, &t.threadKey, &t.at, &t.text)
	})
	if err != nil {
		return err
	}
	for _, t := range turns {
		if parked, err := e.send(ctx, turnEvent(s, t)); err != nil || parked {
			return e.finish(ctx, s, parked, err)
		}
		if _, err := e.pool.Exec(ctx, `UPDATE sessions SET bridge_turn_seq = $2 WHERE id = $1::uuid AND bridge_turn_seq < $2`, s.id, t.seq); err != nil {
			return err
		}
	}
	switch {
	case s.archivedAt != nil && len(turns) < turnsPerTick:
		parked, err := e.send(ctx, endedEvent(s))
		if err != nil || parked {
			return e.finish(ctx, s, parked, err)
		}
		_, err = e.pool.Exec(ctx, `UPDATE sessions SET bridge_state = 'ended' WHERE id = $1::uuid AND archived_at IS NOT NULL`, s.id)
		return err
	case s.archivedAt == nil && s.state != nil && *s.state == "ended":
		// Unarchived again: back to opened, no event (a later archive has its own ce_id).
		_, err = e.pool.Exec(ctx, `UPDATE sessions SET bridge_state = 'opened' WHERE id = $1::uuid AND archived_at IS NULL`, s.id)
		return err
	}
	return nil
}

func (e *Events) finish(ctx context.Context, s session, parked bool, err error) error {
	if err != nil {
		return err
	}
	_, err = e.pool.Exec(ctx, `UPDATE sessions SET bridge_parked = true WHERE id = $1::uuid`, s.id)
	return err
}

func (e *Events) send(ctx context.Context, ev *bridgev1.CloudEvent) (parked bool, err error) {
	pctx, cancel := context.WithTimeout(ctx, cardevents.PublishTimeout)
	defer cancel()
	if err := e.pub.Publish(pctx, ev); err != nil {
		if code := connect.CodeOf(err); code != connect.CodeInvalidArgument && code != connect.CodePermissionDenied {
			return false, err
		}
		slog.Error("sessionevents: the bridge refused a session event, parked", "subject", ev.Subject, "type", ev.Type, "error", err)
		metrics.SessionEventsRefused.Inc()
		return true, nil
	}
	metrics.SessionEventsLastSuccess.SetToCurrentTime()
	metrics.SessionEventsPublished.WithLabelValues(ev.Type).Inc()
	return false, nil
}

// kind: character (a standing session), definition, else repo.
func kind(s session) string {
	switch {
	case s.lifecycle == "standing":
		return "character"
	case s.definition != "":
		return "definition"
	}
	return "repo"
}

// base is the data every event shares. Ids, labels and times only.
func base(s session, at time.Time) map[string]any {
	d := map[string]any{
		"session_id": s.id, "kind": kind(s), "repo": s.repo,
		"occurred_at": at.UTC().Format(time.RFC3339),
	}
	if s.definition != "" {
		d["definition"] = s.definition
	}
	if s.title != "" {
		d["title"] = s.title
	}
	return d
}

func event(s session, typ, id string, at time.Time, data map[string]any) *bridgev1.CloudEvent {
	b, _ := json.Marshal(data)
	return &bridgev1.CloudEvent{
		Id: id, Type: TypePrefix + typ, Subject: "fleet/session/" + s.id,
		Time: timestamppb.New(at), Data: b, Actor: "machine", ActorLogin: "core",
	}
}

func openedEvent(s session) *bridgev1.CloudEvent {
	d := base(s, s.createdAt)
	if s.threadKey != "" {
		d["thread_key"] = s.threadKey
	}
	return event(s, "opened", "session/"+s.id+"/opened", s.createdAt, d)
}

// turnEvent reads two numbers from the result's JSON and nothing else.
func turnEvent(s session, t turn) *bridgev1.CloudEvent {
	d := base(s, t.at)
	d["thread_key"], d["seq"] = t.threadKey, t.seq
	var r struct {
		DurationMs *int64 `json:"durationMs"`
		IsError    *bool  `json:"isError"`
	}
	if json.Unmarshal([]byte(t.text), &r) == nil {
		if r.DurationMs != nil {
			d["duration_ms"] = *r.DurationMs
		}
		if r.IsError != nil {
			d["is_error"] = *r.IsError
		}
	}
	return event(s, "turn_ended", fmt.Sprintf("session/%s/turn/%d", s.id, t.seq), t.at, d)
}

func endedEvent(s session) *bridgev1.CloudEvent {
	at := *s.archivedAt
	return event(s, "ended", fmt.Sprintf("session/%s/ended/%d", s.id, at.Unix()), at, base(s, at))
}
