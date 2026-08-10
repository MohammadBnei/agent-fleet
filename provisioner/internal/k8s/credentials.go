package k8s

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	mathrand "math/rand"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	ScopeModePodScoped  = "pod-scoped"
	ScopeModeTaskScoped = "task-scoped"
	ScopeModeRepoScoped = "repo-scoped"
)

// postgresDuplicateObject/postgresDuplicateDatabase are the Postgres error
// codes for "already exists" on CREATE ROLE / CREATE DATABASE — caught and
// ignored so the mint sequence is idempotent under a genuine race between
// two pods for the same task (see MintServiceCredentials's doc comment).
const (
	postgresDuplicateObject   = "42710"
	postgresDuplicateDatabase = "42P04"
	// postgresUniqueViolation is the SQLSTATE a concurrent CREATE ROLE can
	// surface when it hits pg_authid's unique index directly rather than
	// the "nicer" duplicate_object check — see mintPostgres's mintOnce.
	postgresUniqueViolation = "23505"
)

// mintRetryAttempts/mintRetryBackoff bound the retry loop for transient
// Postgres system-catalog DDL concurrency errors (mintPostgres). Backoff is
// jittered (see jitter()) so concurrent callers don't retry in lockstep.
const (
	mintRetryAttempts = 6
	mintRetryBackoff  = 50 * time.Millisecond
)

func jitter() time.Duration {
	return time.Duration(mathrand.Intn(50)) * time.Millisecond
}

// MintServiceCredentials resolves a task-scoped or repo-scoped service
// ingredient into a ready-to-use connection URL, minting the underlying
// role/database (postgres) or ACL user (redis) idempotently if needed.
// Called synchronously inside CreateWorkerPod/CreateE2eSession's gRPC
// handler, before the consuming pod exists — a failure here means the RPC
// itself errors, so the pod is never created (docs/adr/0034's stronger
// fail-loud guarantee than a StartupProbe).
//
// Name and password are fully deterministic — task_<shortID(taskID)> or
// repo_<repo>, password = hmacSHA256(adminPassword, name) — never stored
// or looked up anywhere. A second pod for the same task (e.g. an e2e pod
// requested moments after its worker pod) independently recomputes the
// identical credentials and re-runs the same idempotent mint sequence, a
// safe no-op — this is what makes concurrent callers race-safe without a
// lookup service or a per-task Secret.
func (c *Client) MintServiceCredentials(ctx context.Context, repo, taskID, serviceKey, scopeMode string) (url string, err error) {
	if scopeMode == ScopeModePodScoped {
		return "", fmt.Errorf("MintServiceCredentials called with pod-scoped mode — pod-scoped services never mint, they're a same-pod sidecar")
	}

	host, port, adminPassword, err := c.EnsureSharedInstance(ctx, repo, serviceKey)
	if err != nil {
		return "", fmt.Errorf("ensure shared instance: %w", err)
	}

	name, err := scopedName(repo, taskID, scopeMode)
	if err != nil {
		return "", err
	}
	password := derivePassword(adminPassword, name)

	switch serviceKey {
	case "postgres":
		return mintPostgres(ctx, host, port, adminPassword, name, password)
	case "redis":
		return mintRedis(ctx, host, port, adminPassword, name, password)
	default:
		return "", fmt.Errorf("unknown service ingredient %q", serviceKey)
	}
}

func scopedName(repo, taskID, scopeMode string) (string, error) {
	switch scopeMode {
	case ScopeModeTaskScoped:
		if taskID == "" {
			return "", errors.New("task-scoped credentials require a task id")
		}
		return "task_" + shortID(taskID), nil
	case ScopeModeRepoScoped:
		return "repo_" + sanitizeForIdentifier(repo), nil
	default:
		return "", fmt.Errorf("unknown scope mode %q", scopeMode)
	}
}

// sanitizeForIdentifier keeps repo-derived Postgres role/database names and
// Redis ACL usernames safe — repo names are operator-entered (the repos
// table, docs/adr/0028), not attacker-controlled, but this is cheap
// insurance against a repo name containing a character that breaks the
// derived identifier.
func sanitizeForIdentifier(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		b := s[i]
		if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' {
			out = append(out, b)
		} else if b >= 'A' && b <= 'Z' {
			out = append(out, b+('a'-'A'))
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

func derivePassword(adminPassword, name string) string {
	mac := hmac.New(sha256.New, []byte(adminPassword))
	mac.Write([]byte(name))
	sum := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return sum[:32]
}

func mintPostgres(ctx context.Context, host string, port int32, adminPassword, name, password string) (string, error) {
	adminDSN := fmt.Sprintf("postgres://postgres:%s@%s:%d/postgres?sslmode=disable", adminPassword, host, port)
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return "", fmt.Errorf("connect as admin: %w", err)
	}
	defer conn.Close(ctx)

	// Plain sequential statements, not a transaction: Postgres can't run
	// CREATE DATABASE inside a transaction block.
	//
	// mintOnce is retried below (mintRetryAttempts) rather than relying on
	// catching specific SQLSTATEs: CREATE ROLE/CREATE DATABASE touch shared
	// system catalogs (pg_authid/pg_database), which aren't MVCC-protected
	// the way regular tables are — confirmed live under concurrent callers,
	// a genuine race can surface as 42710 (duplicate_object, the "nice"
	// case), 23505 (unique_violation, hitting the catalog index directly),
	// or an internal "tuple concurrently updated" error on ALTER ROLE with
	// no clean SQLSTATE at all. A short retry loop handles all of these
	// uniformly instead of trying to enumerate Postgres's internal DDL
	// concurrency errors one by one.
	mintOnce := func() error {
		if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, pgQuoteIdent(name), pgEscapeLiteral(password))); err != nil &&
			!isPgCode(err, postgresDuplicateObject) && !isPgCode(err, postgresUniqueViolation) {
			return fmt.Errorf("create role: %w", err)
		}
		// Unconditional — always converges to the same value, safe under a
		// genuine race between two concurrent callers (retried below if the
		// underlying catalog update itself races).
		if _, err := conn.Exec(ctx, fmt.Sprintf(`ALTER ROLE %s WITH PASSWORD '%s'`, pgQuoteIdent(name), pgEscapeLiteral(password))); err != nil {
			return fmt.Errorf("alter role: %w", err)
		}
		if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, pgQuoteIdent(name), pgQuoteIdent(name))); err != nil && !isPgCode(err, postgresDuplicateDatabase) {
			return fmt.Errorf("create database: %w", err)
		}
		return nil
	}

	var mintErr error
	for attempt := range mintRetryAttempts {
		if mintErr = mintOnce(); mintErr == nil {
			break
		}
		// Jittered, not a fixed sleep: concurrent callers hitting the same
		// catalog contention at the same instant would otherwise retry in
		// lockstep and keep colliding with each other on every attempt
		// (confirmed live — a fixed 100ms backoff still failed under
		// sustained 8-way concurrency because every goroutine woke up and
		// retried at the same moment).
		time.Sleep(mintRetryBackoff*time.Duration(attempt+1) + jitter())
	}
	if mintErr != nil {
		return "", mintErr
	}

	return fmt.Sprintf("postgresql://%s:%s@%s:%d/%s?sslmode=disable", name, password, host, port, name), nil
}

// mintRedis grants name/password auth-level isolation via Redis ACL —
// ponytail: allkeys/allcommands, not a `~name:*` key-pattern restriction,
// because true per-task key-namespace isolation would require the
// consuming app to know and apply that prefix itself (ioredis has no way
// to learn it from a bare REDIS_URL). Separate revocable credentials per
// task/repo is the real isolation gained here; upgrade to key-pattern
// scoping if a task ever needs protection from another task's Redis keys,
// not just its own credentials.
func mintRedis(ctx context.Context, host string, port int32, adminPassword, name, password string) (string, error) {
	conn, err := dialRedisAuthenticated(ctx, host, port, adminPassword)
	if err != nil {
		return "", fmt.Errorf("connect as admin: %w", err)
	}
	defer conn.Close()

	if _, err := conn.Write(respCommand("ACL", "SETUSER", name, "on", ">"+password, "allkeys", "allcommands")); err != nil {
		return "", fmt.Errorf("acl setuser: %w", err)
	}
	if err := readRESPStatus(conn); err != nil {
		return "", fmt.Errorf("acl setuser: %w", err)
	}

	return fmt.Sprintf("redis://%s:%s@%s:%d", name, password, host, port), nil
}

func isPgCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// pgQuoteIdent/pgEscapeLiteral: name/password are our own deterministic
// derivations (not attacker-controlled input), but quoting them properly
// is cheap insurance against a name/password containing a character that
// would otherwise break the SQL statement.
func pgQuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func pgEscapeLiteral(s string) string {
	return strings.ReplaceAll(s, `'`, `''`)
}
