// Package repos owns the `repos` table (docs/adr/0028). Since docs/adr/0074
// nothing edits it by hand: Sync projects the bot's GitHub repos plus
// infra-bootstrap's overrides file into it.
package repos

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo struct {
	Name       string
	URL        string
	BaseBranch string // GitHub's default_branch unless overridden; "" means "main"
	// Image is the container image this repo's sessions run. "" means the
	// fleet default. Replaces the four toolchain ingredients of
	// docs/adr/0034's recipe system (go-toolchain, bun-toolchain,
	// golangci-lint, buf), which existed only because the worker image was
	// generic — an init container copying a Go toolchain onto an emptyDir is
	// an elaborate way of saying "this repo needs Go".
	Image string
	// ClusterAccess is whether this repo's sessions get the kubectl shim that
	// RPCs to thot-executor (docs/adr/0037) — the one recipe ingredient that
	// did NOT collapse into Image, because it is a privilege grant rather
	// than a toolchain. The pod still holds zero Kubernetes credentials
	// either way; the shim is just an RPC client.
	//
	// Data rather than code for docs/adr/0037's own reason: which sessions
	// may reach the cluster is a human's decision. Since docs/adr/0074 that
	// decision is made only in infra-bootstrap's overrides file, by a
	// reviewed PR. A bool rather than a profile row because there is exactly
	// one such privilege and it composes with nothing.
	ClusterAccess bool
	// FullName is GitHub's owner/repo ("" until the first sync).
	FullName string
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) List(ctx context.Context) ([]Repo, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, url, base_branch, image, cluster_access, full_name FROM repos WHERE gone_since IS NULL ORDER BY name`)
	if err != nil {
		slog.Error("repos List", "error", err)
		return nil, fmt.Errorf("list repos: %w", err)
	}
	defer rows.Close()

	// []Repo{}, not var repos []Repo — a nil slice marshals to JSON `null`
	// (same fix as tasks.Store.ListRecentTasks), and this is exposed
	// directly through the dashboard's ListRepos RPC.
	result := []Repo{}
	for rows.Next() {
		var r Repo
		if err := rows.Scan(&r.Name, &r.URL, &r.BaseBranch, &r.Image, &r.ClusterAccess, &r.FullName); err != nil {
			slog.Error("repos List: scan", "error", err)
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// Get returns nil, nil when no repo with this name exists, or when it is
// gone (docs/adr/0074): every create and warm looks its repo up here, so a
// gone repo is refused everywhere at once.
func (s *Store) Get(ctx context.Context, name string) (*Repo, error) {
	var r Repo
	err := s.pool.QueryRow(ctx, `SELECT name, url, base_branch, image, cluster_access, full_name FROM repos WHERE name = $1 AND gone_since IS NULL`, name).
		Scan(&r.Name, &r.URL, &r.BaseBranch, &r.Image, &r.ClusterAccess, &r.FullName)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		slog.Error("repos Get", "name", name, "error", err)
		return nil, fmt.Errorf("get repo: %w", err)
	}
	return &r, nil
}
