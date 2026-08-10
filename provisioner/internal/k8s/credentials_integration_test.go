//go:build integration

package k8s

import (
	"context"
	"fmt"
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
