//go:build integration

package announce

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
	bridgev1 "github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1"
)

type fakePub struct {
	events []*bridgev1.CloudEvent
	fail   error
}

func (f *fakePub) Publish(_ context.Context, e *bridgev1.CloudEvent) error {
	if f.fail != nil {
		return f.fail
	}
	f.events = append(f.events, e)
	return nil
}

func thread(t *testing.T, pool *pgxpool.Pool, invoker string) string {
	t.Helper()
	var id string
	if _, err := pool.Exec(context.Background(), `INSERT INTO agent_definitions (name, sha, spec, effective, valid)
		VALUES ('d', 'sha', '{}', '{}', true) ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	err := pool.QueryRow(context.Background(), `
		INSERT INTO sessions (repo, definition_name, definition_sha, owner, invoker, thread_key, visibility)
		VALUES ('r', 'd', 'sha', 'client:' || $1, $1, gen_random_uuid()::text, 'private') RETURNING id`, invoker).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestEdgesForOptedInClientsOnly(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()
	tr := transcript.NewPostgresStore(pool)
	pub := &fakePub{}
	a := New(pool, pub, func() []string { return []string{"game"} })

	game := thread(t, pool, "game")
	other := thread(t, pool, "bridge")
	var console string
	if err := pool.QueryRow(ctx, `INSERT INTO sessions (repo) VALUES ('r') RETURNING id`).Scan(&console); err != nil {
		t.Fatal(err)
	}

	// A question and a plan wait; nothing else is announced.
	q, err := tr.Append(ctx, game, "agent", "Which colour?", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := tr.Append(ctx, game, "agent", `{"tool":"ExitPlanMode","input":{"plan":"secret plan text"}}`, "permission_request", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{other, console} {
		if _, err := tr.Append(ctx, id, "agent", "?", "question", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(pub.events) != 1 {
		t.Fatalf("published %d events, want only the game thread's", len(pub.events))
	}
	w := pub.events[0]
	if w.Type != TypeWaiting || w.Subject != "fleet/client/game/"+game || w.Actor != "machine" || w.ActorLogin != "core" || w.Trusted {
		t.Fatalf("waiting event %+v", w)
	}
	var data struct {
		Thread   string   `json:"thread_id"`
		Pending  int      `json:"pending"`
		Reasons  []string `json:"reasons"`
		Position int64    `json:"position"`
	}
	if err := json.Unmarshal(w.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Thread != game || data.Pending != 2 || len(data.Reasons) != 2 || data.Reasons[0] != "question" || data.Reasons[1] != "plan" || data.Position != plan {
		t.Fatalf("data %+v", data)
	}
	if bytes.Contains(w.Data, []byte("secret")) || bytes.Contains(w.Data, []byte("colour")) {
		t.Fatal("transcript text leaked into the announcement")
	}

	// No new edge: nothing more, also after a restart (a new Announcer).
	if err := New(pool, pub, func() []string { return []string{"game"} }).Tick(ctx); err != nil || len(pub.events) != 1 {
		t.Fatalf("re-announced an unchanged edge: %d %v", len(pub.events), err)
	}

	// One answered: still waiting. Both: resumed.
	if _, err := tr.AppendReply(ctx, game, "human", "blue", "answer", "", q); err != nil {
		t.Fatal(err)
	}
	if err := a.Check(ctx, game); err != nil || len(pub.events) != 1 {
		t.Fatalf("one answer of two published: %d %v", len(pub.events), err)
	}
	last, err := tr.AppendReply(ctx, game, "human", `{"behavior":"allow"}`, "permission_response", "", plan)
	if err != nil {
		t.Fatal(err)
	}
	// A failed publish leaves the edge to the next check.
	pub.fail = errors.New("bridge down")
	if err := a.Check(ctx, game); err == nil {
		t.Fatal("a failed publish was not reported")
	}
	pub.fail = nil
	if err := a.Check(ctx, game); err != nil {
		t.Fatal(err)
	}
	if len(pub.events) != 2 || pub.events[1].Type != TypeResumed || pub.events[1].Id != game+":"+strconv.FormatInt(last, 10)+":resumed" {
		t.Fatalf("resumed: %+v", pub.events[len(pub.events)-1])
	}

	// A client that opts out stops being announced.
	a.clients = func() []string { return nil }
	if _, err := tr.Append(ctx, game, "agent", "again?", "question", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.Tick(ctx); err != nil || len(pub.events) != 2 {
		t.Fatalf("announced for a client that opted out: %d %v", len(pub.events), err)
	}
}

// A session archived while waiting resumes, with an id distinct from its wait
// (same position: no reply row is written), so the bridge does not dedup it.
func TestArchivedWhileWaitingResumes(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()
	tr := transcript.NewPostgresStore(pool)
	pub := &fakePub{}
	a := New(pool, pub, func() []string { return []string{"game"} })
	id := thread(t, pool, "game")
	if _, err := tr.Append(ctx, id, "agent", "?", "question", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.Tick(ctx); err != nil || len(pub.events) != 1 {
		t.Fatalf("waiting: %d %v", len(pub.events), err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := a.Tick(ctx); err != nil || len(pub.events) != 2 {
		t.Fatalf("resumed on archive: %d %v", len(pub.events), err)
	}
	if pub.events[1].Type != TypeResumed || pub.events[1].Id == pub.events[0].Id {
		t.Fatalf("archive resume %q reuses the wait's id %q", pub.events[1].Id, pub.events[0].Id)
	}
	if err := a.Tick(ctx); err != nil || len(pub.events) != 2 {
		t.Fatalf("an archived, resumed session was announced again: %d %v", len(pub.events), err)
	}
}

// A failed publish backs off and then catches up.
func TestBackoff(t *testing.T) {
	a := New(nil, &fakePub{}, nil)
	now := time.Unix(0, 0)
	a.now = func() time.Time { return now }
	a.settle(errors.New("down"))
	if a.backoff != BackoffMin || !a.retryAt.Equal(now.Add(BackoffMin)) {
		t.Fatalf("first failure: %v %v", a.backoff, a.retryAt)
	}
	for range 10 {
		a.settle(errors.New("down"))
	}
	if a.backoff != BackoffMax {
		t.Fatalf("backoff %v, want capped at %v", a.backoff, BackoffMax)
	}
	a.settle(nil)
	if a.backoff != 0 || !a.retryAt.IsZero() {
		t.Fatal("a success did not reset the backoff")
	}
}
