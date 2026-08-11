// Package repos owns the dashboard-editable `repos` table (docs/adr/0028)
// — the DB-backed replacement for the hardcoded tasks.KnownRepos Go map,
// so onboarding/editing a target repo no longer needs a core redeploy.
package repos

import (
	"context"
	"strings"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrExists is returned by Create when a repo with the same name already
// exists — callers map it to connect.CodeAlreadyExists.
var ErrExists = errors.New("repo already exists")

// ErrSSHURL rejects the git@host:path form at the boundary.
//
// The provisioner authenticates with `gh auth setup-git`, which wires
// HTTPS credentials only — an SSH URL can never authenticate, so the repo
// looks fine until a task is dispatched and then dies with
// "clone/fetch failed", marked failed_permanently with no obvious cause.
//
// Confirmed live 2026-08-11: infra-bootstrap was seeded with the SSH form
// and every task against it failed at clone. Catching it here turns a
// crashed pod ten minutes later into an error message at the moment
// someone types the URL.
var ErrSSHURL = errors.New("repo URL must be https:// — the provisioner authenticates via `gh auth setup-git`, which only wires HTTPS credentials, so an SSH URL can never clone")

type Repo struct {
	Name       string
	URL        string
	BaseBranch string // "" means the provisioner defaults to "main"
}

type Store struct {
	pool     *pgxpool.Pool
	onChange func() // optional; set via SetOnChange
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// SetOnChange wires a callback fired after every successful Create/Update/
// Delete — same pattern as tasks.Store.SetNudge. core/cmd/core/run.go uses
// this to refresh Discord's /task repo choices live, without a redeploy.
func (s *Store) SetOnChange(onChange func()) {
	s.onChange = onChange
}

func (s *Store) List(ctx context.Context) ([]Repo, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, url, base_branch FROM repos ORDER BY name`)
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
		if err := rows.Scan(&r.Name, &r.URL, &r.BaseBranch); err != nil {
			slog.Error("repos List: scan", "error", err)
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// Get returns nil, nil when no repo with this name exists.
func (s *Store) Get(ctx context.Context, name string) (*Repo, error) {
	var r Repo
	err := s.pool.QueryRow(ctx, `SELECT name, url, base_branch FROM repos WHERE name = $1`, name).
		Scan(&r.Name, &r.URL, &r.BaseBranch)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		slog.Error("repos Get", "name", name, "error", err)
		return nil, fmt.Errorf("get repo: %w", err)
	}
	return &r, nil
}

func (s *Store) Create(ctx context.Context, r Repo) error {
	if err := validateURL(r.URL); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO repos (name, url, base_branch) VALUES ($1, $2, $3)`,
		r.Name, r.URL, r.BaseBranch)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrExists
		}
		slog.Error("repos Create", "name", r.Name, "error", err)
		return fmt.Errorf("create repo: %w", err)
	}
	slog.Info("repos Create", "name", r.Name)
	if s.onChange != nil {
		s.onChange()
	}
	return nil
}

// Update returns pgx.ErrNoRows if no repo with this name exists.
func (s *Store) Update(ctx context.Context, r Repo) error {
	if err := validateURL(r.URL); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE repos SET url = $2, base_branch = $3, updated_at = now() WHERE name = $1
	`, r.Name, r.URL, r.BaseBranch)
	if err != nil {
		slog.Error("repos Update", "name", r.Name, "error", err)
		return fmt.Errorf("update repo: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	slog.Info("repos Update", "name", r.Name)
	if s.onChange != nil {
		s.onChange()
	}
	return nil
}

// Delete returns pgx.ErrNoRows if no repo with this name exists.
func (s *Store) Delete(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM repos WHERE name = $1`, name)
	if err != nil {
		slog.Error("repos Delete", "name", name, "error", err)
		return fmt.Errorf("delete repo: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	slog.Info("repos Delete", "name", name)
	if s.onChange != nil {
		s.onChange()
	}
	return nil
}

// validateURL rejects the one URL shape that is syntactically fine and
// operationally impossible here. Deliberately narrow: it does not try to
// validate that the repo exists or is reachable, only that its scheme is
// one this fleet can actually authenticate.
func validateURL(url string) error {
	if strings.HasPrefix(url, "git@") || strings.HasPrefix(url, "ssh://") {
		return ErrSSHURL
	}
	return nil
}
