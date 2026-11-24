//go:build integration

package sessionevents

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	bridgev1 "github.com/MohammadBnei/ukubi-bridge/proto/gen/go/bridge/v1"
)

type fakePub struct{ events []*bridgev1.CloudEvent }

func (f *fakePub) Publish(_ context.Context, e *bridgev1.CloudEvent) error {
	f.events = append(f.events, e)
	return nil
}

func TestOldRowsNotPublishedNewOnesAre(t *testing.T) {
	pool := dbtest.NewPool(t)
	ctx := context.Background()
	var old, fresh string
	ins := `INSERT INTO sessions (repo, title) VALUES ('r', 't') RETURNING id`
	if err := pool.QueryRow(ctx, ins).Scan(&old); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key) VALUES ($1, 1, 'worker', '{"isError":false}', 'result', 'a')`,
		`UPDATE sessions SET bridge_state = NULL, bridge_turn_seq = 0 WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, q, old); err != nil {
			t.Fatal(err)
		}
	}
	// Run the migration's own backfill over the pre-existing rows.
	sql, err := os.ReadFile("../../../db/migrations/000043_session_bridge_events.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(sql), "UPDATE sessions")
	j := strings.Index(string(sql), "-- Finds")
	if _, err := pool.Exec(ctx, string(sql)[i:j]); err != nil {
		t.Fatal(err)
	}
	pub := &fakePub{}
	e := New(pool, pub, true)
	if err := e.Tick(ctx); err != nil || len(pub.events) != 0 {
		t.Fatalf("old rows published: %v %v", err, pub.events)
	}
	// A new session, a turn, then archive: opened, turn_ended, ended, once each across ticks.
	if err := pool.QueryRow(ctx, ins).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key) VALUES ($1, 1, 'worker', '{"isError":true}', 'result', 'a')`, fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET archived_at = now() WHERE id = $1`, fresh); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, ev := range pub.events {
		got = append(got, ev.Type[len(TypePrefix):])
	}
	if strings.Join(got, ",") != "opened,turn_ended,ended" {
		t.Fatalf("events %v", got)
	}
}
