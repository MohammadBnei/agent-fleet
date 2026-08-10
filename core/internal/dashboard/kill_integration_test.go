//go:build integration

package dashboard

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/jackc/pgx/v5/pgxpool"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
)

// Real Postgres — Kill now writes to tasks.Store (MarkStopRequested), a
// concrete Postgres-backed type server_test.go's recordingStore-style fake
// can't stand in for. dbtest.NewPool applies the real db/migrations/
// (docs/adr/0030) rather than a hand-rolled subset.
func newTestPool(t *testing.T) *pgxpool.Pool {
	return dbtest.NewPool(t)
}

func seedTask(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO tasks (repo, description, discord_channel_id) VALUES ('dream-analyst', 'task', 'chan')
		RETURNING id
	`).Scan(&id); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return id
}

// TestServer_Kill_DefaultReason covers the plain-unit test this replaces
// (server_test.go, before Kill also wrote to tasks.Store), plus the new
// side effect: stop_requested_at gets set on the task row.
func TestServer_Kill_DefaultReason(t *testing.T) {
	pool := newTestPool(t)
	taskID := seedTask(t, pool)
	store := &recordingStore{}
	s := NewServer(tasks.NewStore(pool), store, nil, nil, nil, nil, nil, nil, nil, 5, nil)

	resp, err := s.Kill(context.Background(), connect.NewRequest(&agentfleetv1.KillRequest{TaskId: taskID}))
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if resp.Msg.GetStatus() != "killing" {
		t.Errorf("status = %q, want %q", resp.Msg.GetStatus(), "killing")
	}
	if store.lastText != "killed by human" || store.lastType != "abort" {
		t.Errorf("got (%q, %q), want (killed by human, abort)", store.lastText, store.lastType)
	}

	var stopRequestedAt *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT stop_requested_at FROM tasks WHERE id = $1`, taskID).Scan(&stopRequestedAt); err != nil {
		t.Fatalf("read stop_requested_at: %v", err)
	}
	if stopRequestedAt == nil {
		t.Error("expected stop_requested_at to be set after Kill")
	}
}

func TestServer_Kill_CustomReason(t *testing.T) {
	pool := newTestPool(t)
	taskID := seedTask(t, pool)
	store := &recordingStore{}
	s := NewServer(tasks.NewStore(pool), store, nil, nil, nil, nil, nil, nil, nil, 5, nil)

	reason := "wrong direction"
	req := connect.NewRequest(&agentfleetv1.KillRequest{TaskId: taskID, Reason: &reason})
	if _, err := s.Kill(context.Background(), req); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if store.lastText != "wrong direction" {
		t.Errorf("text = %q, want %q", store.lastText, "wrong direction")
	}
}
