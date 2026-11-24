//go:build integration

package sessionevents

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// The session-event migrations run while appends are in flight: no deadlock (40P01), no append stuck for
// more than a second. 000043 is the CONCURRENTLY index, run by golang-migrate exactly as production does.
func TestMigrationsUnderAppends(t *testing.T) {
	ctx := context.Background()
	c, err := postgres.Run(ctx, "postgres:18", postgres.WithDatabase("m"), postgres.WithUsername("test"), postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := filepath.Abs("../../../db/migrations")
	m, err := migrate.New("file://"+dir, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	if err := m.Migrate(42); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const workers = 8
	ids := make([]string, workers)
	for i := range ids {
		if err := pool.QueryRow(ctx, `INSERT INTO sessions (repo) VALUES ('r') RETURNING id::text`).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
		// Enough rows that the index build is not instant.
		if _, err := pool.Exec(ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key)
			SELECT $1, g, 'w', 'x', 'result', 'seed' || g FROM generate_series(1, 20000) g`, ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	store := transcript.NewPostgresStore(pool, decisions.OnAppend)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var appends, deadlocks, slow atomic.Int64
	var firstErr atomic.Value
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				start := time.Now()
				_, err := store.Append(ctx, ids[i], "worker", "x", "result", fmt.Sprintf("live-%d-%d", i, n))
				if d := time.Since(start); d > time.Second {
					slow.Add(1)
				}
				var pe *pgconn.PgError
				if errors.As(err, &pe) && pe.Code == "40P01" {
					deadlocks.Add(1)
				}
				if err != nil {
					firstErr.CompareAndSwap(nil, err)
				}
				appends.Add(1)
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	if err := m.Up(); err != nil {
		t.Fatalf("migrate up under load: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	t.Logf("%d appends during the migrations, %d deadlocks, %d slower than 1s", appends.Load(), deadlocks.Load(), slow.Load())
	if deadlocks.Load() != 0 || slow.Load() != 0 {
		t.Fatalf("deadlocks %d, slow appends %d", deadlocks.Load(), slow.Load())
	}
	if e := firstErr.Load(); e != nil {
		t.Fatalf("an append failed: %v", e)
	}
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid = 'transcript_result_idx'::regclass`).Scan(&valid); err != nil || !valid {
		t.Fatalf("index not valid: %v %v", valid, err)
	}
}
