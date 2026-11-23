// Package cardevents publishes a decision card's state to the event bridge:
// opened, answered, expired, stale, withdrawn (docs/adr/0078 amendment). Core
// keeps the last state the bridge accepted per card (decisions.bridge_state)
// and publishes again whenever the card's state differs, on a tick:
// current state, like announce, not a queue (docs/adr/0048 §2). A card
// that opens and closes between two ticks publishes only its last state, and
// a session's hard delete cascades its cards away with no terminal event; the
// data carries expires_at so a consumer can close an orphaned open card.
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
	"regexp"
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
	definition, bridgeState     *string
}

// Tick publishes, in version order, up to Batch cards whose state differs
// from the one the bridge accepted, and refreshes the pending and parked
// counts. A refusal that retrying cannot fix (InvalidArgument, PermissionDenied)
// parks the card; any other error (Unauthenticated, Unavailable, DeadlineExceeded)
// stops the tick, so every later card waits behind it; CardEventsStuck fires.
func (e *Events) Tick(ctx context.Context) error {
	rows, err := e.pool.Query(ctx, `
		SELECT d.id::text, d.session_id::text, d.state, COALESCE(d.subject, ''), d.version, d.created_at, d.answered_at, d.expires_at,
		       COALESCE(d.answer ->> 'optionId', ''), COALESCE(d.payload::text, '{}'), s.definition_name, d.bridge_state
		  FROM decisions d JOIN sessions s ON s.id = d.session_id
		 WHERE d.kind = 'card' AND NOT d.bridge_parked AND d.state IS DISTINCT FROM d.bridge_state
		 ORDER BY d.version LIMIT $1`, Batch)
	if err != nil {
		return err
	}
	cards, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var c row
		err := r.Scan(&c.id, &c.session, &c.state, &c.subject, &c.version, &c.createdAt, &c.answeredAt, &c.expiresAt, &c.optionID, &c.payload, &c.definition, &c.bridgeState)
		return c, err
	})
	if err != nil {
		return err
	}
	var perr error
	for _, c := range cards {
		if perr = e.publish(ctx, c); perr != nil {
			break
		}
	}
	// Counted after the drain (also after a failure), so steady churn does not read as stuck.
	var pending, parked int64
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE NOT bridge_parked AND state IS DISTINCT FROM bridge_state),
		count(*) FILTER (WHERE bridge_parked) FROM decisions WHERE kind = 'card'`).Scan(&pending, &parked); err != nil {
		return err
	}
	metrics.CardEventsPending.Set(float64(pending))
	metrics.CardEventsParked.Set(float64(parked))
	if perr != nil {
		return perr
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

// transition is when the card reached state, as far as the row says.
func (e *Events) transition(c row, state string) time.Time {
	switch {
	case state == "open":
		return c.createdAt
	case state == "answered" && c.answeredAt != nil:
		return *c.answeredAt
	case state == "expired" && c.expiresAt != nil:
		return *c.expiresAt
	}
	return e.now() // stale and withdrawn are not stamped
}

// optionRe: the card's option ids are "1".."3"; anything else is not forwarded.
var optionRe = regexp.MustCompile(`^[1-3]$`)

// publish sends the card's state. A card first seen already closed publishes
// `opened` first (at created_at), so a consumer sees the whole life. Each
// state has one ce_id, card/<id>/<state>: no writer re-enters open, so a
// republish is a duplicate the bridge drops.
func (e *Events) publish(ctx context.Context, c row) error {
	if c.state != "open" && c.bridgeState == nil {
		if parked, err := e.send(ctx, c, "open"); err != nil || parked {
			return e.finish(ctx, c, parked, err)
		}
	}
	parked, err := e.send(ctx, c, c.state)
	return e.finish(ctx, c, parked, err)
}

// finish records the outcome: parked rows leave the pick, published ones move bridge_state.
func (e *Events) finish(ctx context.Context, c row, parked bool, err error) error {
	if err != nil {
		return err // bridge_state is unchanged: the next tick publishes again
	}
	metrics.CardEventsLastSuccess.SetToCurrentTime()
	if parked {
		_, err = e.pool.Exec(ctx, `UPDATE decisions SET bridge_parked = true WHERE id = $1::uuid`, c.id)
		return err
	}
	// A newer transition (state differs from the one published) stays pending.
	_, err = e.pool.Exec(ctx, `UPDATE decisions SET bridge_state = $2 WHERE id = $1::uuid AND state = $2`, c.id, c.state)
	return err
}

// send publishes one event for state; parked reports a refusal retrying cannot fix.
func (e *Events) send(ctx context.Context, c row, state string) (parked bool, err error) {
	var card agentfleetv1.Card
	_ = protojson.Unmarshal([]byte(c.payload), &card)
	at := e.transition(c, state)
	data := map[string]any{
		"decision_id": c.id, "session_id": c.session, "state": state,
		"subject_kind": card.GetSubject().GetKind(), "subject": c.subject,
		"drafted_by": card.GetDraftedBy(), "writer_definition": card.GetWriterDefinition(),
		"created_at": c.createdAt.UTC().Format(time.RFC3339), "transitioned_at": at.UTC().Format(time.RFC3339),
	}
	if state == "answered" && optionRe.MatchString(c.optionID) {
		data["option_id"] = c.optionID
	}
	if c.expiresAt != nil { // lets a consumer close an orphaned open card (a deleted session emits no terminal event)
		data["expires_at"] = c.expiresAt.UTC().Format(time.RFC3339)
	}
	b, _ := json.Marshal(data)
	typ := TypePrefix + state
	if state == "open" {
		typ = TypePrefix + "opened"
	}
	ev := &bridgev1.CloudEvent{
		Id: fmt.Sprintf("card/%s/%s", c.id, state), Type: typ, Subject: subjectFor(c),
		Time: timestamppb.New(at), Data: b, Actor: "machine", ActorLogin: "core",
	}
	pctx, cancel := context.WithTimeout(ctx, PublishTimeout)
	defer cancel()
	if err := e.pub.Publish(pctx, ev); err != nil {
		if code := connect.CodeOf(err); code != connect.CodeInvalidArgument && code != connect.CodePermissionDenied {
			return false, err
		}
		slog.Error("cardevents: the bridge refused a card event, parked", "decision", c.id, "type", typ, "error", err)
		metrics.CardEventsRefused.Inc()
		return true, nil
	}
	metrics.CardEventsPublished.WithLabelValues(typ).Inc()
	return false, nil
}
