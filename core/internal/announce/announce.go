// Package announce publishes, for the sessions of clients that opted in
// (AgentClient.spec.announceSessions), when a session starts and stops waiting
// on a person (docs/adr/0078 §2-3). An edge of the pending-decision count:
// waiting when it goes from 0 to more, resumed when it returns to 0 (or the
// session is archived). Core keeps the last edge the bridge accepted per
// session and publishes again whenever the current state differs, on a nudge
// from the transcript decorator or on a tick; that is current state, like
// pod_phase, not a queue (docs/adr/0048 §2).
package announce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/MohammadBnei/agent-fleet/core/internal/bridgeclient"
	bridgev1 "github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1"
)

const (
	TypeWaiting = "dev.bnei.fleet.session.waiting"
	TypeResumed = "dev.bnei.fleet.session.resumed"
)

// Tunables; tests shrink them.
var (
	PublishTimeout = 10 * time.Second
	BackoffMin     = 30 * time.Second
	BackoffMax     = 5 * time.Minute
)

// Publisher is the bridge (bridgeclient.Client); tests fake it.
type Publisher interface {
	Publish(ctx context.Context, e *bridgev1.CloudEvent) error
}

type Announcer struct {
	pool    *pgxpool.Pool
	pub     Publisher
	clients func() []string // the clients with announceSessions; nil or empty: nothing is announced
	nudge   chan string
	now     func() time.Time

	// Owned by the Run goroutine (and by tests calling Check/Tick directly).
	backoff  time.Duration
	retryAt  time.Time
	lastNote string // the last "not configured" reason logged, so it is logged once
}

func New(pool *pgxpool.Pool, pub Publisher, clients func() []string) *Announcer {
	return &Announcer{pool: pool, pub: pub, clients: clients, nudge: make(chan string, 64), now: time.Now}
}

// Nudge asks for one session to be checked soon. It never blocks an append:
// a full buffer drops it, and the tick catches up. With no client opted in it
// does nothing at all.
func (a *Announcer) Nudge(sessionID string) {
	if len(a.optedIn()) == 0 {
		return
	}
	select {
	case a.nudge <- sessionID:
	default:
	}
}

// Run checks nudged sessions as they come and every opted-in session each
// interval (which catches a stale close in ReserveSlot and anything else that
// bypasses the decorator). After a failed publish it backs off, up to
// BackoffMax, and skips nudges until then: the tick that follows catches up.
func (a *Announcer) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-a.nudge:
			if a.now().Before(a.retryAt) {
				continue
			}
			a.settle(a.Check(ctx, id))
		case <-t.C:
			if a.now().Before(a.retryAt) {
				continue
			}
			a.settle(a.Tick(ctx))
		}
	}
}

// settle records how the last attempt went: a failure doubles the backoff and
// logs one WARN per step; not being configured logs one Info per reason.
func (a *Announcer) settle(err error) {
	switch {
	case err == nil:
		a.backoff, a.retryAt = 0, time.Time{}
		a.lastNote = ""
	case errors.Is(err, context.Canceled):
	case errors.Is(err, bridgeclient.ErrNotConfigured):
		if note := err.Error(); note != a.lastNote {
			slog.Info("announce: no session announcements", "reason", note)
			a.lastNote = note
		}
	default:
		a.backoff = min(max(2*a.backoff, BackoffMin), BackoffMax)
		a.retryAt = a.now().Add(a.backoff)
		slog.Warn("announce: publish failed, backing off", "retryIn", a.backoff, "error", err)
	}
}

func (a *Announcer) optedIn() []string {
	if a.clients == nil {
		return nil
	}
	return a.clients()
}

// Tick checks every session whose current state differs from its stored edge,
// found in one query, and stops at the first failure (the backoff follows).
func (a *Announcer) Tick(ctx context.Context) error {
	names := a.optedIn()
	if len(names) == 0 {
		return nil
	}
	rows, err := a.pool.Query(ctx, `
		SELECT s.id FROM sessions s
		  LEFT JOIN session_announcements n ON n.session_id = s.id
		 WHERE s.invoker = ANY($1)
		   AND (s.archived_at IS NULL OR n.waiting)
		   AND (s.archived_at IS NULL AND EXISTS (
		          SELECT 1 FROM transcript q
		           WHERE q.session_id = s.id AND q.type IN ('question', 'permission_request')
		             AND NOT EXISTS (SELECT 1 FROM transcript r WHERE r.session_id = q.session_id AND r.reply_to_seq = q.seq)))
		       IS DISTINCT FROM coalesce(n.waiting, false)`, names)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := a.Check(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

type pendingEntry struct {
	typ, text string
}

type state struct {
	invoker    *string
	archived   bool
	position   int64
	pending    []pendingEntry
	known      bool // an edge was published before
	lastWait   bool
	lastPosNum int64
}

// read takes everything Check needs from one snapshot, so an append between
// two reads cannot pair an old position with a newer pending set.
func (a *Announcer) read(ctx context.Context, id string) (*state, error) {
	tx, err := a.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var st state
	err = tx.QueryRow(ctx, `
		SELECT s.invoker, s.archived_at IS NOT NULL,
		       coalesce((SELECT max(t.seq) FROM transcript t
		                  WHERE t.session_id = s.id
		                    AND (t.type IN ('question', 'permission_request') OR t.reply_to_seq IS NOT NULL)), 0)
		  FROM sessions s WHERE s.id = $1`, id).Scan(&st.invoker, &st.archived, &st.position)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT q.type, q.text FROM transcript q
		 WHERE q.session_id = $1 AND q.type IN ('question', 'permission_request')
		   AND NOT EXISTS (SELECT 1 FROM transcript r WHERE r.session_id = q.session_id AND r.reply_to_seq = q.seq)
		 ORDER BY q.seq`, id)
	if err != nil {
		return nil, err
	}
	st.pending, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (pendingEntry, error) {
		var p pendingEntry
		err := r.Scan(&p.typ, &p.text)
		return p, err
	})
	if err != nil {
		return nil, err
	}
	err = tx.QueryRow(ctx, `SELECT waiting, position FROM session_announcements WHERE session_id = $1`, id).Scan(&st.lastWait, &st.lastPosNum)
	st.known = err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return &st, nil
}

// Check publishes session id's edge if it differs from the last one the
// bridge accepted. A session no opted-in client opened is never announced.
func (a *Announcer) Check(ctx context.Context, id string) error {
	names := a.optedIn()
	if len(names) == 0 {
		return nil
	}
	st, err := a.read(ctx, id)
	if err != nil || st == nil {
		return err
	}
	if st.invoker == nil || !slices.Contains(names, *st.invoker) {
		return nil
	}
	waiting := len(st.pending) > 0 && !st.archived // an archived session waits on nobody
	if (st.known && st.lastWait == waiting) || (!st.known && !waiting) {
		return nil // no edge: a session never seen waiting has nothing to resume from
	}

	edge, typ := "resumed", TypeResumed
	if waiting {
		edge, typ = "waiting", TypeWaiting
	}
	data, _ := json.Marshal(map[string]any{
		"thread_id": id, "pending": len(st.pending), "reasons": reasons(st.pending), "position": st.position,
	})
	// The edge is part of the id: an archived session's resume shares the
	// position of the wait it ends, and the bridge dedups on (source, id).
	e := &bridgev1.CloudEvent{
		Id: fmt.Sprintf("%s:%d:%s", id, st.position, edge), Type: typ, Subject: "fleet/client/" + *st.invoker + "/" + id,
		Time: timestamppb.New(a.now()), Data: data,
		Actor: "machine", ActorLogin: "core", Trusted: false,
	}
	pctx, cancel := context.WithTimeout(ctx, PublishTimeout)
	defer cancel()
	if err := a.pub.Publish(pctx, e); err != nil {
		return err // the stored edge is unchanged: the next nudge or tick publishes again
	}
	_, err = a.pool.Exec(ctx, `
		INSERT INTO session_announcements (session_id, waiting, position) VALUES ($1, $2, $3)
		ON CONFLICT (session_id) DO UPDATE SET waiting = excluded.waiting, position = excluded.position, published_at = now()
		 WHERE session_announcements.position <= excluded.position`, id, waiting, st.position)
	return err
}

// reasons names what the pending entries wait for, without their text: a plan
// to approve is a permission request for ExitPlanMode.
func reasons(pending []pendingEntry) []string {
	var out []string
	add := func(r string) {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	for _, p := range pending {
		if p.typ == "question" {
			add("question")
			continue
		}
		var req struct {
			Tool string `json:"tool"`
		}
		_ = json.Unmarshal([]byte(p.text), &req)
		if req.Tool == "ExitPlanMode" {
			add("plan")
		} else {
			add("permission")
		}
	}
	return out
}
