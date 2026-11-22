//go:build integration

package k8s

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startAdminPostgres spins up a throwaway postgres:16-alpine with a known
// superuser password, standing in for a shared instance's own admin
// account (docs/adr/0034) — this test exercises mintPostgres directly
// against a real server, not through EnsureSharedInstance/Kubernetes.
func startAdminPostgres(t *testing.T) (host string, port int32, adminPassword string) {
	t.Helper()
	ctx := context.Background()
	const password = "admin-test-password"

	// No Docker socket (Forgejo's container runner): CI provides a Postgres
	// service whose superuser `postgres` has this password, reached by name.
	if h := os.Getenv("AGENTFLEET_TEST_ADMIN_PG_HOST"); h != "" {
		return h, 5432, password
	}
	// A runner with neither a Docker socket nor a superuser Postgres (the Talos
	// pull-request runner): CI sets this and the main gate runs the tests
	// (.github/workflows/go.yml, provisioner-mint).
	if os.Getenv("AGENTFLEET_TEST_NO_ADMIN_PG") != "" {
		t.Skip("needs a superuser Postgres; run by go.yml's provisioner-mint job on main")
	}

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithUsername("postgres"),
		postgres.WithPassword(password),
		postgres.WithDatabase("postgres"),
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

	h, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	mappedPort, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("mapped port: %v", err)
	}
	return h, int32(mappedPort.Num()), password
}

func TestMintPostgres_IdempotentAcrossRepeatedCalls(t *testing.T) {
	host, port, adminPassword := startAdminPostgres(t)
	ctx := context.Background()

	name := "task_idempotenttest"
	password := derivePassword(adminPassword, name)

	url1, err := mintPostgres(ctx, host, port, adminPassword, name, password)
	if err != nil {
		t.Fatalf("first mintPostgres: %v", err)
	}
	url2, err := mintPostgres(ctx, host, port, adminPassword, name, password)
	if err != nil {
		t.Fatalf("second mintPostgres: %v", err)
	}
	if url1 != url2 {
		t.Errorf("mint URLs differ across idempotent calls: %q vs %q", url1, url2)
	}

	conn, err := pgx.Connect(ctx, url1)
	if err != nil {
		t.Fatalf("connect with minted credentials: %v", err)
	}
	defer conn.Close(ctx)
	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("ping with minted credentials: %v", err)
	}
}

func TestMintPostgres_RaceSafeUnderConcurrentCallers(t *testing.T) {
	host, port, adminPassword := startAdminPostgres(t)
	ctx := context.Background()

	name := "task_racetest"
	password := derivePassword(adminPassword, name)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = mintPostgres(ctx, host, port, adminPassword, name, password)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent mintPostgres[%d]: %v", i, err)
		}
	}

	url := fmt.Sprintf("postgresql://%s:%s@%s:%d/%s?sslmode=disable", name, password, host, port, name)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect with minted credentials after race: %v", err)
	}
	defer conn.Close(ctx)
	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("ping after race: %v", err)
	}
}

// The race test above can pass without the lock on a quiet host (the retries
// absorb it), so this one pins the lock itself: a mint waits while its name's
// lock is held, fails after mintLockWait with Postgres' own lock timeout and
// creates nothing, then goes through once the lock is free.
func TestMintPostgres_WaitsForTheNamesLock(t *testing.T) {
	host, port, adminPassword := startAdminPostgres(t)
	ctx := context.Background()

	name := "task_locktest"
	password := derivePassword(adminPassword, name)

	holder, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:%s@%s:%d/postgres?sslmode=disable", adminPassword, host, port))
	if err != nil {
		t.Fatalf("connect as admin: %v", err)
	}
	defer holder.Close(ctx) //nolint:errcheck
	for _, q := range []string{`DROP DATABASE IF EXISTS ` + name, `DROP ROLE IF EXISTS ` + name} {
		if _, err := holder.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	roleExists := func() bool {
		t.Helper()
		var ok bool
		if err := holder.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, name).Scan(&ok); err != nil {
			t.Fatalf("pg_roles: %v", err)
		}
		return ok
	}

	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, name); err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	old := mintLockWait
	mintLockWait = 300 * time.Millisecond
	t.Cleanup(func() { mintLockWait = old })
	_, err = mintPostgres(ctx, host, port, adminPassword, name, password)
	if err == nil || !strings.HasPrefix(err.Error(), "lock "+name+":") || !isPgCode(err, "55P03") {
		t.Fatalf("mintPostgres with its name's lock held: %v, want a \"lock %s:\" lock_timeout (55P03) error", err, name)
	}
	if roleExists() {
		t.Fatal("role created while its name's lock was held")
	}

	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock(hashtext($1))`, name); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	if _, err := mintPostgres(ctx, host, port, adminPassword, name, password); err != nil {
		t.Fatalf("mintPostgres after release: %v", err)
	}
	if !roleExists() {
		t.Fatal("no role after the lock was released")
	}
}
