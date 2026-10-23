//go:build integration

package announce

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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
	if string(w.Data) != "" && (contains(w.Data, "secret") || contains(w.Data, "colour")) {
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
	if len(pub.events) != 2 || pub.events[1].Type != TypeResumed || pub.events[1].Id != game+":"+itoa(last) {
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

func contains(b []byte, s string) bool { return len(s) > 0 && bytesIndex(b, s) >= 0 }

func bytesIndex(b []byte, s string) int {
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == s {
			return i
		}
	}
	return -1
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
