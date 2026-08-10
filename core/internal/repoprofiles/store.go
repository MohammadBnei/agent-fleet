// Package repoprofiles owns the dashboard-editable repo_profiles table
// (docs/adr/0034) — a repo declares named profiles ("worker", "e2e",
// "lint", ...) built from a bounded catalog of tool/service ingredients,
// replacing provisioner/internal/k8s/names.go's hardcoded StartCmdFor
// switch. Mirrors core/internal/repos.Store's shape (docs/adr/0028).
package repoprofiles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrExists is returned by Create when a profile with the same
// (repo, name) already exists — callers map it to connect.CodeAlreadyExists.
var ErrExists = errors.New("repo profile already exists")

// ServiceIngredient is a service-kind ingredient (postgres/redis) plus how
// it's shared across pods/tasks — see docs/adr/0034 for the three modes.
type ServiceIngredient struct {
	Key       string
	ScopeMode string // "pod-scoped" | "task-scoped" | "repo-scoped"
}

type Profile struct {
	ID       string
	RepoName string
	Name     string
	StartCmd string
	Tools    []string
	Services []ServiceIngredient
}

type Store struct {
	pool     *pgxpool.Pool
	onChange func() // optional; set via SetOnChange
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// SetOnChange wires a callback fired after every successful Create/Update/
// Delete — same pattern as repos.Store.SetOnChange.
func (s *Store) SetOnChange(onChange func()) {
	s.onChange = onChange
}

func (s *Store) List(ctx context.Context, repoName string) ([]Profile, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, repo_name, name, start_cmd FROM repo_profiles
		WHERE repo_name = $1 ORDER BY name
	`, repoName)
	if err != nil {
		slog.Error("repoprofiles List", "repo", repoName, "error", err)
		return nil, fmt.Errorf("list repo profiles: %w", err)
	}
	defer rows.Close()

	// []Profile{}, not var profiles []Profile — a nil slice marshals to
	// JSON `null` (same fix as repos.Store.List), exposed directly through
	// the dashboard's ListRepoProfiles RPC.
	result := []Profile{}
	for rows.Next() {
		var p Profile
		if err := rows.Scan(&p.ID, &p.RepoName, &p.Name, &p.StartCmd); err != nil {
			slog.Error("repoprofiles List: scan", "error", err)
			return nil, fmt.Errorf("scan repo profile: %w", err)
		}
		result = append(result, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range result {
		if err := s.loadIngredients(ctx, &result[i]); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Get returns nil, nil when no profile with this (repoName, name) exists.
func (s *Store) Get(ctx context.Context, repoName, name string) (*Profile, error) {
	var p Profile
	err := s.pool.QueryRow(ctx, `
		SELECT id, repo_name, name, start_cmd FROM repo_profiles
		WHERE repo_name = $1 AND name = $2
	`, repoName, name).Scan(&p.ID, &p.RepoName, &p.Name, &p.StartCmd)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		slog.Error("repoprofiles Get", "repo", repoName, "name", name, "error", err)
		return nil, fmt.Errorf("get repo profile: %w", err)
	}
	if err := s.loadIngredients(ctx, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) loadIngredients(ctx context.Context, p *Profile) error {
	toolRows, err := s.pool.Query(ctx,
		`SELECT tool_key FROM repo_profile_tools WHERE profile_id = $1 ORDER BY tool_key`, p.ID)
	if err != nil {
		return fmt.Errorf("list profile tools: %w", err)
	}
	defer toolRows.Close()
	p.Tools = []string{}
	for toolRows.Next() {
		var key string
		if err := toolRows.Scan(&key); err != nil {
			return fmt.Errorf("scan profile tool: %w", err)
		}
		p.Tools = append(p.Tools, key)
	}
	if err := toolRows.Err(); err != nil {
		return err
	}

	svcRows, err := s.pool.Query(ctx,
		`SELECT service_key, scope_mode FROM repo_profile_services WHERE profile_id = $1 ORDER BY service_key`, p.ID)
	if err != nil {
		return fmt.Errorf("list profile services: %w", err)
	}
	defer svcRows.Close()
	p.Services = []ServiceIngredient{}
	for svcRows.Next() {
		var si ServiceIngredient
		if err := svcRows.Scan(&si.Key, &si.ScopeMode); err != nil {
			return fmt.Errorf("scan profile service: %w", err)
		}
		p.Services = append(p.Services, si)
	}
	return svcRows.Err()
}

// Create inserts a profile and its ingredients in one transaction.
func (s *Store) Create(ctx context.Context, p Profile) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO repo_profiles (repo_name, name, start_cmd) VALUES ($1, $2, $3) RETURNING id
	`, p.RepoName, p.Name, p.StartCmd).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return "", ErrExists
		}
		slog.Error("repoprofiles Create", "repo", p.RepoName, "name", p.Name, "error", err)
		return "", fmt.Errorf("create repo profile: %w", err)
	}
	if err := insertIngredients(ctx, tx, id, p.Tools, p.Services); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	slog.Info("repoprofiles Create", "repo", p.RepoName, "name", p.Name)
	if s.onChange != nil {
		s.onChange()
	}
	return id, nil
}

// Update replaces a profile's start_cmd and ingredients wholesale — matches
// how a save-whole-profile dashboard form naturally submits. Returns
// pgx.ErrNoRows if no profile with this (repoName, name) exists.
func (s *Store) Update(ctx context.Context, p Profile) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var id string
	err = tx.QueryRow(ctx, `
		UPDATE repo_profiles SET start_cmd = $3, updated_at = now()
		WHERE repo_name = $1 AND name = $2 RETURNING id
	`, p.RepoName, p.Name, p.StartCmd).Scan(&id)
	if err == pgx.ErrNoRows {
		return pgx.ErrNoRows
	}
	if err != nil {
		slog.Error("repoprofiles Update", "repo", p.RepoName, "name", p.Name, "error", err)
		return fmt.Errorf("update repo profile: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM repo_profile_tools WHERE profile_id = $1`, id); err != nil {
		return fmt.Errorf("clear profile tools: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM repo_profile_services WHERE profile_id = $1`, id); err != nil {
		return fmt.Errorf("clear profile services: %w", err)
	}
	if err := insertIngredients(ctx, tx, id, p.Tools, p.Services); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	slog.Info("repoprofiles Update", "repo", p.RepoName, "name", p.Name)
	if s.onChange != nil {
		s.onChange()
	}
	return nil
}

func insertIngredients(ctx context.Context, tx pgx.Tx, profileID string, tools []string, services []ServiceIngredient) error {
	for _, key := range tools {
		if _, err := tx.Exec(ctx,
			`INSERT INTO repo_profile_tools (profile_id, tool_key) VALUES ($1, $2)`, profileID, key,
		); err != nil {
			return fmt.Errorf("insert profile tool %q: %w", key, err)
		}
	}
	for _, si := range services {
		if _, err := tx.Exec(ctx,
			`INSERT INTO repo_profile_services (profile_id, service_key, scope_mode) VALUES ($1, $2, $3)`,
			profileID, si.Key, si.ScopeMode,
		); err != nil {
			return fmt.Errorf("insert profile service %q: %w", si.Key, err)
		}
	}
	return nil
}

// Delete returns pgx.ErrNoRows if no profile with this (repoName, name) exists.
func (s *Store) Delete(ctx context.Context, repoName, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM repo_profiles WHERE repo_name = $1 AND name = $2`, repoName, name)
	if err != nil {
		slog.Error("repoprofiles Delete", "repo", repoName, "name", name, "error", err)
		return fmt.Errorf("delete repo profile: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	slog.Info("repoprofiles Delete", "repo", repoName, "name", name)
	if s.onChange != nil {
		s.onChange()
	}
	return nil
}
