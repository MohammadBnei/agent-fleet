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

// pgMigrator starts postgres, migrates to version 'to', and returns the migrator and a pool.
func pgMigrator(t *testing.T, to uint) (*migrate.Migrate, *pgxpool.Pool) {
	t.Helper()
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
	if err := m.Migrate(to); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return m, pool
}

// appenders runs 8 goroutines appending result entries; stop() ends them and returns the totals.
func appenders(t *testing.T, pool *pgxpool.Pool, ids []string) (stop func() (appends, deadlocks int64, maxLatency time.Duration)) {
	t.Helper()
	ctx := context.Background()
	store := transcript.NewPostgresStore(pool, decisions.OnAppend)
	done := make(chan struct{})
	var wg sync.WaitGroup
	var n, dl, maxNs atomic.Int64
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; ; k++ {
				select {
				case <-done:
					return
				default:
				}
				start := time.Now()
				_, err := store.Append(ctx, ids[i], "worker", "x", "result", fmt.Sprintf("live-%d-%d", i, k))
				d := time.Since(start).Nanoseconds()
				for {
					cur := maxNs.Load()
					if d <= cur || maxNs.CompareAndSwap(cur, d) {
						break
					}
				}
				var pe *pgconn.PgError
				if errors.As(err, &pe) && pe.Code == "40P01" {
					dl.Add(1)
				}
				n.Add(1)
			}
		}()
	}
	return func() (int64, int64, time.Duration) {
		close(done)
		wg.Wait()
		return n.Load(), dl.Load(), time.Duration(maxNs.Load())
	}
}

func seed(t *testing.T, pool *pgxpool.Pool, workers, rows int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, workers)
	for i := range ids {
		if err := pool.QueryRow(ctx, `INSERT INTO sessions (repo) VALUES ('r') RETURNING id::text`).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key)
			SELECT $1, g, 'w', 'x', 'result', 'seed' || g FROM generate_series(1, $2::int) g`, ids[i], rows); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

// The session-event migrations run while appends are in flight: no deadlock (40P01), no append stuck for
// more than a second. 000044 is the CONCURRENTLY index, run by golang-migrate exactly as production does.
func TestMigrationsUnderAppends(t *testing.T) {
	ctx := context.Background()
	m, pool := pgMigrator(t, 43)
	stop := appenders(t, pool, seed(t, pool, 8, 20000))
	time.Sleep(300 * time.Millisecond)
	if err := m.Up(); err != nil {
		t.Fatalf("migrate up under load: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	n, dl, maxLat := stop()
	t.Logf("%d appends during the migrations, %d deadlocks, slowest %v", n, dl, maxLat)
	if dl != 0 || maxLat > time.Second {
		t.Fatalf("deadlocks %d, slowest append %v", dl, maxLat)
	}
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid = 'transcript_result_idx'::regclass`).Scan(&valid); err != nil || !valid {
		t.Fatalf("index not valid: %v %v", valid, err)
	}
}

// 044 alone, on a table big enough that the build takes a while: an append must never wait for it.
// A plain CREATE INDEX (SHARE lock) holds appends for the whole build.
func TestIndexBuildDoesNotBlockAppends(t *testing.T) {
	m, pool := pgMigrator(t, 43)
	stop := appenders(t, pool, seed(t, pool, 8, 150000))
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	if err := m.Steps(1); err != nil {
		t.Fatal(err)
	}
	build := time.Since(start)
	n, _, maxLat := stop()
	t.Logf("build %v, %d appends, slowest %v", build, n, maxLat)
	if build < 500*time.Millisecond {
		t.Fatalf("the build took %v: too short to prove anything, raise the row count", build)
	}
	if maxLat > 300*time.Millisecond {
		t.Fatalf("an append waited %v during the index build", maxLat)
	}
}

// A passing reader or row lock (5s, longer than one 3s lock_timeout) must not dirty 045 or 046.
func TestLockTimeoutsDoNotDirtyTheVersion(t *testing.T) {
	for _, c := range []struct {
		name   string
		from   uint
		hold   string
		wantAt uint
	}{
		{"reader on sessions blocks 045", 44, `SELECT count(*) FROM sessions`, 46},
		{"row lock on sessions blocks 046", 45, `SELECT id FROM sessions FOR UPDATE`, 46},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			m, pool := pgMigrator(t, c.from)
			if _, err := pool.Exec(ctx, `INSERT INTO sessions (repo) VALUES ('r')`); err != nil {
				t.Fatal(err)
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, c.hold); err != nil {
				t.Fatal(err)
			}
			go func() { time.Sleep(5 * time.Second); _ = tx.Commit(ctx) }()
			if err := m.Up(); err != nil {
				t.Fatalf("migrate up: %v", err)
			}
			v, dirty, err := m.Version()
			if err != nil || dirty || v != c.wantAt {
				t.Fatalf("version %d dirty %v err %v", v, dirty, err)
			}
		})
	}
}
