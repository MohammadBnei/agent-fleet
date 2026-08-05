//go:build integration

package coreserver

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"

	"github.com/MohammadBnei/agent-fleet/core/internal/journal"
	"github.com/MohammadBnei/agent-fleet/core/internal/tasks"
)

// Real Postgres (tasks + knowledge_journal, the two tables ReportPodEvents
// actually touches) — matches tasks/store_test.go's own container setup.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("agentfleettest"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `
		CREATE EXTENSION IF NOT EXISTS pgcrypto;
		CREATE TABLE tasks (
			id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			repo               TEXT NOT NULL,
			description        TEXT NOT NULL,
			status             TEXT NOT NULL DEFAULT 'pending',
			discord_channel_id TEXT,
			discord_thread_id  TEXT,
			pr_url             TEXT,
			notes              TEXT,
			last_error         TEXT,
			planning_session_id TEXT,
			model              TEXT,
			retry_count        INT NOT NULL DEFAULT 0,
			heartbeat_at       TIMESTAMPTZ,
			lease_id           UUID,
			pod_phase          TEXT,
			pod_message        TEXT,
			created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE TABLE knowledge_journal (
			id         BIGSERIAL PRIMARY KEY,
			repo       TEXT,
			actor      TEXT NOT NULL,
			event_type TEXT NOT NULL,
			payload    JSONB NOT NULL DEFAULT '{}',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`)
	if err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return pool
}

// newBufconnClient drives ReportPodEvents (a real streaming RPC) against
// srv without a real network listener — same bufconn pattern
// provisionerclient/client_test.go already established for testing the
// other direction of this same call.
func newBufconnClient(t *testing.T, srv *Server) agentfleetv1.CoreServiceClient {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() { _ = lis.Close() })

	grpcSrv := grpc.NewServer()
	agentfleetv1.RegisterCoreServiceServer(grpcSrv, srv)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return agentfleetv1.NewCoreServiceClient(conn)
}

func sendOneEvent(t *testing.T, client agentfleetv1.CoreServiceClient, event *agentfleetv1.PodEvent) {
	t.Helper()
	stream, err := client.ReportPodEvents(context.Background())
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := stream.Send(event); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatalf("close and recv: %v", err)
	}
}

// TestReportPodEvents_CrashedWorkerMarksNonTerminalTaskCrashed covers
// reliability-findings.md #1's fast-path accelerant: a CRASHED event for
// a non-terminal task should make it immediately reclaim-eligible via
// MarkCrashed, instead of waiting out the full 10-minute staleness
// window.
func TestReportPodEvents_CrashedWorkerMarksNonTerminalTaskCrashed(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	taskStore := tasks.NewStore(pool)
	srv := New(nil, taskStore, journal.NewStore(pool), nil)

	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO tasks (repo, description, discord_channel_id, status, heartbeat_at)
		VALUES ('dream-analyst', 'task', 'chan', 'implementing', now())
		RETURNING id
	`).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	sendOneEvent(t, newBufconnClient(t, srv), &agentfleetv1.PodEvent{
		TaskId: taskID, Kind: agentfleetv1.SessionKind_SESSION_KIND_WORKER,
		Phase: agentfleetv1.PodPhase_POD_PHASE_CRASHED, PodName: "worker-abc", Message: "test crash",
	})

	claimed, err := taskStore.ClaimNextTask(ctx, 1000, 1000)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ID != taskID {
		t.Fatalf("expected the crashed task to be immediately reclaim-eligible, got %+v", claimed)
	}
}

// TestReportPodEvents_CrashedWorkerNoopsForTerminalTask covers the safe-
// no-op guarantee: a crash event for an already-terminal task (a race
// between the provisioner's reconcile loop and core's own opportunistic
// teardown) must not resurrect it.
func TestReportPodEvents_CrashedWorkerNoopsForTerminalTask(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	srv := New(nil, tasks.NewStore(pool), journal.NewStore(pool), nil)

	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO tasks (repo, description, discord_channel_id, status, heartbeat_at)
		VALUES ('dream-analyst', 'task', 'chan', 'done', now())
		RETURNING id
	`).Scan(&taskID); err != nil {
		t.Fatalf("seed a done task: %v", err)
	}

	sendOneEvent(t, newBufconnClient(t, srv), &agentfleetv1.PodEvent{
		TaskId: taskID, Kind: agentfleetv1.SessionKind_SESSION_KIND_WORKER, Phase: agentfleetv1.PodPhase_POD_PHASE_CRASHED,
	})

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id = $1`, taskID).Scan(&status); err != nil {
		t.Fatalf("check status: %v", err)
	}
	if status != "done" {
		t.Fatalf("expected a terminal task's status to be untouched, got %q", status)
	}
}

// TestReportPodEvents_NonCrashedEventDoesNotMarkCrashed confirms a
// non-crash phase (e.g. RUNNING) never triggers MarkCrashed.
func TestReportPodEvents_NonCrashedEventDoesNotMarkCrashed(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	srv := New(nil, tasks.NewStore(pool), journal.NewStore(pool), nil)

	var taskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO tasks (repo, description, discord_channel_id, status, heartbeat_at)
		VALUES ('dream-analyst', 'task', 'chan', 'implementing', now())
		RETURNING id
	`).Scan(&taskID); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	sendOneEvent(t, newBufconnClient(t, srv), &agentfleetv1.PodEvent{
		TaskId: taskID, Kind: agentfleetv1.SessionKind_SESSION_KIND_WORKER, Phase: agentfleetv1.PodPhase_POD_PHASE_RUNNING,
	})

	// MarkCrashed backdates heartbeat_at by 11 minutes — if it had
	// (wrongly) fired, this would be a large positive number instead of
	// ~0.
	var secondsAgo float64
	if err := pool.QueryRow(ctx, `SELECT extract(epoch from now() - heartbeat_at) FROM tasks WHERE id = $1`, taskID).Scan(&secondsAgo); err != nil {
		t.Fatalf("check heartbeat_at: %v", err)
	}
	if secondsAgo > 5 {
		t.Fatalf("expected heartbeat_at untouched by a non-crash event, got %.1fs old", secondsAgo)
	}
}
