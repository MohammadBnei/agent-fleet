//go:build integration

package sessionevents

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MohammadBnei/agent-fleet/core/internal/dbtest"
	"github.com/MohammadBnei/agent-fleet/core/internal/decisions"
	"github.com/MohammadBnei/agent-fleet/core/internal/transcript"
)

// appenders runs one goroutine per session appending result entries; stop() ends them and returns the totals.
func appenders(pool *pgxpool.Pool, ids []string) (stop func() (appends, deadlocks, failures int64)) {
	ctx := context.Background()
	store := transcript.NewPostgresStore(pool, decisions.OnAppend)
	done := make(chan struct{})
	var wg sync.WaitGroup
	var n, dl, bad atomic.Int64
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
				_, err := store.Append(ctx, ids[i], "worker", "x", "result", fmt.Sprintf("live-%d-%d", i, k))
				var pe *pgconn.PgError
				if errors.As(err, &pe) && pe.Code == "40P01" {
					dl.Add(1)
				}
				if err != nil {
					bad.Add(1)
				}
				n.Add(1)
			}
		}()
	}
	return func() (int64, int64, int64) {
		close(done)
		wg.Wait()
		return n.Load(), dl.Load(), bad.Load()
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

// The session-event migrations run while appends are in flight: no deadlock (40P01) and no failed append.
// 044 is the CONCURRENTLY index, run by golang-migrate exactly as production does.
func TestMigrationsUnderAppends(t *testing.T) {
	ctx := context.Background()
	m, pool := dbtest.NewMigrated(t, 43)
	stop := appenders(pool, seed(t, pool, 8, 20000))
	time.Sleep(300 * time.Millisecond)
	if err := m.Up(); err != nil {
		t.Fatalf("migrate up under load: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	n, dl, bad := stop()
	t.Logf("%d appends during the migrations, %d deadlocks, %d failed", n, dl, bad)
	if dl != 0 || bad != 0 {
		t.Fatalf("deadlocks %d, failed appends %d", dl, bad)
	}
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid = 'transcript_result_idx'::regclass`).Scan(&valid); err != nil || !valid {
		t.Fatalf("index not valid: %v %v", valid, err)
	}
}

// 044 alone. An uncommitted insert into transcript holds ROW EXCLUSIVE, so the build has to wait: a plain
// CREATE INDEX (SHARE lock) queues for it and every later append queues behind that request; CREATE INDEX
// CONCURRENTLY (SHARE UPDATE EXCLUSIVE) takes no lock appends conflict with and waits only for the old
// transaction to end. While the build waits, an append must complete. No timing of the server matters.
func TestIndexBuildDoesNotBlockAppends(t *testing.T) {
	ctx := context.Background()
	m, pool := dbtest.NewMigrated(t, 43)
	ids := seed(t, pool, 2, 100)
	old, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = old.Rollback(ctx) })
	if _, err := old.Exec(ctx, `INSERT INTO transcript (session_id, seq, "from", text, type, idempotency_key)
		VALUES ($1::uuid, 1000000, 'w', 'x', 'result', 'old-tx')`, ids[0]); err != nil {
		t.Fatal(err)
	}
	built := make(chan error, 1)
	go func() { built <- m.Steps(1) }()
	// Wait until the build is visibly blocked.
	waiting := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && !waiting; time.Sleep(100 * time.Millisecond) {
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
			AND pid <> pg_backend_pid() AND query ILIKE '%CREATE INDEX%' AND state = 'active' AND wait_event_type = 'Lock')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if !waiting {
		t.Fatal("the index build never reached its wait for the open transaction")
	}
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	store := transcript.NewPostgresStore(pool, decisions.OnAppend)
	if _, err := store.Append(actx, ids[1], "worker", "x", "result", "during-build"); err != nil {
		t.Fatalf("an append was blocked while the index build waited: %v", err)
	}
	select {
	case err := <-built:
		t.Fatalf("the build finished while the old transaction was open (%v): the test proves nothing", err)
	default:
	}
	if err := old.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Other tests' open transactions on a shared server can delay a CONCURRENTLY build: bounded, not a flake.
	select {
	case err := <-built:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("the index build did not finish within 2 minutes of the old transaction ending: another open transaction in this database is holding it")
	}
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid = 'transcript_result_idx'::regclass`).Scan(&valid); err != nil || !valid {
		t.Fatalf("index not valid: %v %v", valid, err)
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
			m, pool := dbtest.NewMigrated(t, c.from)
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
