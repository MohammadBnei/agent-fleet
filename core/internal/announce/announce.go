// Package announce publishes, for the sessions of clients that opted in
// (AgentClient.spec.announceSessions), when a session starts and stops waiting
// on a person (docs/adr/0078 §2-3). An edge of the pending-decision count:
// waiting when it goes from 0 to more, resumed when it returns to 0. Core keeps
// the last edge the bridge accepted per session and publishes again whenever
// the current state differs, on a nudge from the transcript decorator or on a
// tick; that is current state, like pod_phase, not a queue (docs/adr/0048 §2).
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

// Publisher is the bridge (bridgeclient.Client); tests fake it.
type Publisher interface {
	Publish(ctx context.Context, e *bridgev1.CloudEvent) error
}

type Announcer struct {
	pool    *pgxpool.Pool
	pub     Publisher
	clients func() []string // the clients with announceSessions; nil or empty: nothing is announced
	nudge   chan string
}

func New(pool *pgxpool.Pool, pub Publisher, clients func() []string) *Announcer {
	return &Announcer{pool: pool, pub: pub, clients: clients, nudge: make(chan string, 64)}
}

// Nudge asks for one session to be checked soon. It never blocks an append:
// a full buffer drops it, and the tick catches up.
func (a *Announcer) Nudge(sessionID string) {
	select {
	case a.nudge <- sessionID:
	default:
	}
}

// Run checks nudged sessions as they come and every opted-in session each
// interval (which catches a stale close in ReserveSlot and anything else that
// bypasses the decorator).
func (a *Announcer) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-a.nudge:
			if err := a.Check(ctx, id); err != nil {
				a.warn("check", id, err)
			}
		case <-t.C:
			if err := a.Tick(ctx); err != nil {
				a.warn("tick", "", err)
			}
		}
	}
}

func (a *Announcer) warn(what, id string, err error) {
	if errors.Is(err, bridgeclient.ErrNotConfigured) || errors.Is(err, context.Canceled) {
		return // not configured yet: nothing to say, the tick retries
	}
	slog.Warn("announce: "+what, "sessionId", id, "error", err)
}

// Tick checks every live session an opted-in client opened, and every session
// whose stored edge is still waiting (so one archived while waiting resumes).
func (a *Announcer) Tick(ctx context.Context) error {
	names := a.optedIn()
	if len(names) == 0 {
		return nil
	}
	rows, err := a.pool.Query(ctx, `
		SELECT s.id FROM sessions s
		 WHERE s.invoker = ANY($1)
		   AND (s.archived_at IS NULL OR EXISTS (
		        SELECT 1 FROM session_announcements n WHERE n.session_id = s.id AND n.waiting))`, names)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := a.Check(ctx, id); err != nil {
			errs = append(errs, err)
			if errors.Is(err, bridgeclient.ErrNotConfigured) {
				break
			}
		}
	}
	return errors.Join(errs...)
}

func (a *Announcer) optedIn() []string {
	if a.clients == nil {
		return nil
	}
	return a.clients()
}

type pendingEntry struct {
	typ, text string
}

// Check publishes session id's edge if it differs from the last one the
// bridge accepted. A session no opted-in client opened is never announced.
func (a *Announcer) Check(ctx context.Context, id string) error {
	var invoker *string
	var archived bool
	var position int64
	err := a.pool.QueryRow(ctx, `
		SELECT s.invoker, s.archived_at IS NOT NULL,
		       coalesce((SELECT max(t.seq) FROM transcript t
		                  WHERE t.session_id = s.id
		                    AND (t.type IN ('question', 'permission_request') OR t.reply_to_seq IS NOT NULL)), 0)
		  FROM sessions s WHERE s.id = $1`, id).Scan(&invoker, &archived, &position)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if invoker == nil || !slices.Contains(a.optedIn(), *invoker) {
		return nil
	}
	rows, err := a.pool.Query(ctx, `
		SELECT q.type, q.text FROM transcript q
		 WHERE q.session_id = $1 AND q.type IN ('question', 'permission_request')
		   AND NOT EXISTS (SELECT 1 FROM transcript r WHERE r.session_id = q.session_id AND r.reply_to_seq = q.seq)
		 ORDER BY q.seq`, id)
	if err != nil {
		return err
	}
	pending, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pendingEntry, error) {
		var p pendingEntry
		err := r.Scan(&p.typ, &p.text)
		return p, err
	})
	if err != nil {
		return err
	}
	waiting := len(pending) > 0 && !archived // an archived session waits on nobody

	var lastWaiting bool
	var lastPos int64
	err = a.pool.QueryRow(ctx, `SELECT waiting, position FROM session_announcements WHERE session_id = $1`, id).Scan(&lastWaiting, &lastPos)
	known := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if (known && lastWaiting == waiting) || (!known && !waiting) {
		return nil // no edge: a session never seen waiting has nothing to resume from
	}

	typ := TypeResumed
	if waiting {
		typ = TypeWaiting
	}
	data, _ := json.Marshal(map[string]any{
		"thread_id": id, "pending": len(pending), "reasons": reasons(pending), "position": position,
	})
	e := &bridgev1.CloudEvent{
		Id: fmt.Sprintf("%s:%d", id, position), Type: typ, Subject: "fleet/client/" + *invoker + "/" + id,
		Time: timestamppb.Now(), Data: data,
		Actor: "machine", ActorLogin: "core", Trusted: false,
	}
	if err := a.pub.Publish(ctx, e); err != nil {
		return err // the stored edge is unchanged: the next nudge or tick publishes again
	}
	_, err = a.pool.Exec(ctx, `
		INSERT INTO session_announcements (session_id, waiting, position) VALUES ($1, $2, $3)
		ON CONFLICT (session_id) DO UPDATE SET waiting = excluded.waiting, position = excluded.position, published_at = now()
		 WHERE session_announcements.position <= excluded.position`, id, waiting, position)
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
