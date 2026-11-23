// Package cardevents publishes a decision card's state to the event bridge:
// opened, answered, expired, stale, withdrawn (docs/adr/0078 amendment). Core
// keeps the last state the bridge accepted per card (decisions.bridge_state)
// and publishes again whenever the card's state differs, on a tick:
// current state, like announce, not a queue (docs/adr/0048 §2). A card
// that opens and closes between two ticks publishes only its last state.
//
// The event carries no free text: never the ask, the option labels or the
// owner's own words.
package cardevents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/MohammadBnei/agent-fleet/core/internal/bridgeclient"
	"github.com/MohammadBnei/agent-fleet/core/internal/metrics"
	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
	bridgev1 "github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1"
	"github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1/bridgev1connect"
)

// TypePrefix is what the bridge-core row's publish_types must allow.
const TypePrefix = "dev.bnei.fleet.card."

// Tunables; tests shrink them.
var (
	PublishTimeout = 10 * time.Second
	BackoffMin     = 30 * time.Second
	BackoffMax     = 5 * time.Minute
	Batch          = 100
)

// Publisher is the bridge (bridgeclient.Client); tests fake it.
type Publisher interface {
	Publish(ctx context.Context, e *bridgev1.CloudEvent) error
}

type Events struct {
	pool    *pgxpool.Pool
	pub     Publisher
	now     func() time.Time
	backoff time.Duration
	retryAt time.Time
	note    string // the last "not configured" reason logged, so it is logged once
}

// New: enabled says whether the bridge client is configured (BRIDGE_URL set),
// for the enabled metric.
func New(pool *pgxpool.Pool, pub Publisher, enabled bool) *Events {
	if enabled {
		metrics.CardEventsEnabled.Set(1)
	}
	metrics.CardEventsLastSuccess.SetToCurrentTime()
	return &Events{pool: pool, pub: pub, now: time.Now}
}

// Run publishes every interval (no nudge: appends are too frequent for one). After a failure it backs
// off, up to BackoffMax, and never stops.
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
			slog.Info("cardevents: no card events", "reason", n)
			e.note = n
		}
	default:
		e.backoff = min(max(2*e.backoff, BackoffMin), BackoffMax)
		e.retryAt = e.now().Add(e.backoff)
		slog.Warn("cardevents: publish failed, backing off", "retryIn", e.backoff, "error", err)
	}
}

type row struct {
	id, session, state, subject string
	version                     int64
	createdAt                   time.Time
	answeredAt, expiresAt       *time.Time
	optionID, payload           string
	definition                  *string
}

// Tick publishes, in version order, up to Batch cards whose state differs
// from the one the bridge accepted, and refreshes the pending count. A failure
// that concerns every card (the bridge unreachable, not configured) stops it.
func (e *Events) Tick(ctx context.Context) error {
	var pending int64
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM decisions WHERE kind = 'card' AND state IS DISTINCT FROM bridge_state`).Scan(&pending); err != nil {
		return err
	}
	metrics.CardEventsPending.Set(float64(pending))
	rows, err := e.pool.Query(ctx, `
		SELECT d.id::text, d.session_id::text, d.state, COALESCE(d.subject, ''), d.version, d.created_at, d.answered_at, d.expires_at,
		       COALESCE(d.answer ->> 'optionId', ''), COALESCE(d.payload::text, '{}'), s.definition_name
		  FROM decisions d JOIN sessions s ON s.id = d.session_id
		 WHERE d.kind = 'card' AND d.state IS DISTINCT FROM d.bridge_state
		 ORDER BY d.version LIMIT $1`, Batch)
	if err != nil {
		return err
	}
	cards, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var c row
		err := r.Scan(&c.id, &c.session, &c.state, &c.subject, &c.version, &c.createdAt, &c.answeredAt, &c.expiresAt, &c.optionID, &c.payload, &c.definition)
		return c, err
	})
	if err != nil {
		return err
	}
	for _, c := range cards {
		if err := e.publish(ctx, c); err != nil {
			return err
		}
	}
	if len(cards) > 0 {
		return nil
	}
	return e.probe()
}

// probe: with nothing to publish, core still has to know whether the bridge
// is configured, so settle logs it and an idle fleet is not read as healthy.
// ponytail: no Publish probe, so LastSuccess ages while no card changes; the
// pending count is the stuck signal.
func (e *Events) probe() error {
	if c, ok := e.pub.(interface {
		API() (bridgev1connect.BridgeServiceClient, error)
	}); ok {
		_, err := c.API()
		return err
	}
	return nil
}

// subjectFor: the card's thread key as the bridge subject. A character card
// goes under agent/ (an existing publish prefix); every other key already
// starts with forge/ or alert/. A card with no subject falls back to its
// session.
func subjectFor(c row) string {
	switch {
	case c.subject == "":
		return "fleet/session/" + c.session
	case strings.HasPrefix(c.subject, "character/"):
		return "agent/" + c.subject
	}
	return c.subject
}

// transition is when the card reached its state, as far as the row says.
func (e *Events) transition(c row) time.Time {
	switch {
	case c.state == "open":
		return c.createdAt
	case c.state == "answered" && c.answeredAt != nil:
		return *c.answeredAt
	case c.state == "expired" && c.expiresAt != nil:
		return *c.expiresAt
	}
	return e.now() // stale and withdrawn are not stamped
}

func (e *Events) publish(ctx context.Context, c row) error {
	var card agentfleetv1.Card
	_ = protojson.Unmarshal([]byte(c.payload), &card)
	at := e.transition(c)
	data := map[string]any{
		"decision_id": c.id, "session_id": c.session, "state": c.state,
		"subject_kind": card.GetSubject().GetKind(), "subject": c.subject,
		"drafted_by": card.GetDraftedBy(), "writer_definition": card.GetWriterDefinition(),
		"created_at": c.createdAt.UTC().Format(time.RFC3339), "transitioned_at": at.UTC().Format(time.RFC3339),
	}
	if c.optionID != "" {
		data["option_id"] = c.optionID
	}
	b, _ := json.Marshal(data)
	typ := TypePrefix + c.state
	if c.state == "open" {
		typ = TypePrefix + "opened"
	}
	ev := &bridgev1.CloudEvent{
		Id: fmt.Sprintf("card/%s/%s/%d", c.id, c.state, c.version), Type: typ, Subject: subjectFor(c),
		Time: timestamppb.New(at), Data: b, Actor: "machine", ActorLogin: "core",
	}
	pctx, cancel := context.WithTimeout(ctx, PublishTimeout)
	defer cancel()
	if err := e.pub.Publish(pctx, ev); err != nil {
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			return err // bridge_state is unchanged: the next tick publishes again
		}
		// Retrying cannot help: park it, so it does not stall every other card.
		slog.Error("cardevents: the bridge refused a card event, parked", "decision", c.id, "type", typ, "error", err)
		metrics.CardEventsRefused.Inc()
	} else {
		metrics.CardEventsPublished.WithLabelValues(typ).Inc()
	}
	metrics.CardEventsLastSuccess.SetToCurrentTime()
	// A newer transition (state differs from the one published) stays pending.
	_, err := e.pool.Exec(ctx, `UPDATE decisions SET bridge_state = $2 WHERE id = $1::uuid AND state = $2`, c.id, c.state)
	return err
}
