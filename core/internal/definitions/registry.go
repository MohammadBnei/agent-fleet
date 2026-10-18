package definitions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Lister is the provisioner's ListDefinitionFiles (provisionerclient.Client).
type Lister interface {
	ListDefinitionFiles(ctx context.Context) (map[string][]byte, string, error)
}

var (
	ErrUnknown   = errors.New("no such definition")
	ErrInvalid   = errors.New("definition has no valid version")
	ErrNotSynced = errors.New("definitions not synced yet")
)

// Head is one present definition's current state: its latest VALID version
// (what a new session pins), or the error when it has none.
type Head struct {
	Name      string
	SHA       string // "" when there is no valid version
	Effective *Spec  // nil when there is no valid version
	SyncedAt  time.Time
	Err       string // set only when there is no valid version
	// NewerErr is set when the checked-out content is invalid and SHA is an
	// older valid version (a merged typo degrades, it does not unpublish).
	NewerErr string
}

// Registry syncs definitions/ from the provisioner into agent_definitions and
// keeps the current version of each name in memory (docs/adr/0062).
//
// ponytail: `heads` lives in memory because core is single-replica. Empty
// after a restart until the first sync, which runs immediately; a heads
// table replaces it if core ever runs replicated.
type Registry struct {
	pool   *pgxpool.Pool
	lister Lister

	mu       sync.RWMutex
	synced   bool
	heads    map[string]Head
	syncErrs []string
}

func NewRegistry(pool *pgxpool.Pool, lister Lister) *Registry {
	return &Registry{pool: pool, lister: lister}
}

const syncTimeout = 30 * time.Second

// Sync lists, validates and stores every definition, then re-judges the
// stored valid rows of each present name under this core's ceiling. On any
// error the previous state is kept (last known good), so a provisioner blip
// does not unpublish definitions.
func (r *Registry) Sync(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()

	files, sha, err := r.lister.ListDefinitionFiles(ctx)
	if err != nil {
		return err
	}
	results, fileErrs := Load(files)
	syncErrs := make([]string, 0, len(fileErrs))
	for _, e := range fileErrs {
		slog.Error("definitions: file error", "sha", sha, "error", e)
		syncErrs = append(syncErrs, e.Error())
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("definitions sync: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	heads := map[string]Head{}
	for _, res := range results {
		if res.Abstract {
			continue
		}
		var effective []byte
		var errText *string
		if res.Err != nil {
			slog.Warn("definitions: invalid", "name", res.Name, "sha", sha, "error", res.Err)
			msg := res.Err.Error()
			errText = &msg
		} else if effective, err = json.Marshal(res.Effective); err != nil {
			return fmt.Errorf("definitions sync: marshal %s: %w", res.Name, err)
		}
		// Reuse any row with identical content (a revert after a typo gets
		// the old sha back), else a new version at this sha. Every release
		// commit moves HEAD; without this every definition would get a row
		// per commit, and the pin would stop meaning "this content".
		var rowSHA string
		err := tx.QueryRow(ctx, `
			UPDATE agent_definitions SET synced_at = now()
			 WHERE (name, sha) = (SELECT name, sha FROM agent_definitions
			                       WHERE name = $1
			                         AND spec IS NOT DISTINCT FROM $2::jsonb
			                         AND effective IS NOT DISTINCT FROM $3::jsonb
			                         AND valid = $4 AND error IS NOT DISTINCT FROM $5
			                       ORDER BY synced_at DESC, sha LIMIT 1)
			RETURNING sha`, res.Name, nullJSON(res.Own), nullJSON(effective), res.Err == nil, errText).Scan(&rowSHA)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `
				INSERT INTO agent_definitions (name, sha, spec, effective, valid, error)
				VALUES ($1, $2, $3::jsonb, $4::jsonb, $5, $6)
				ON CONFLICT (name, sha) DO UPDATE
				   SET spec = EXCLUDED.spec, effective = EXCLUDED.effective,
				       valid = EXCLUDED.valid, error = EXCLUDED.error, synced_at = now()
				RETURNING sha`, res.Name, sha, nullJSON(res.Own), nullJSON(effective), res.Err == nil, errText).Scan(&rowSHA)
		}
		if err != nil {
			return fmt.Errorf("definitions sync: upsert %s: %w", res.Name, err)
		}
		h, err := r.current(ctx, tx, res.Name)
		if err != nil {
			return err
		}
		if res.Err != nil {
			if h.SHA == "" {
				h.Err = res.Err.Error()
			} else {
				h.NewerErr = res.Err.Error()
			}
		}
		heads[res.Name] = h
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("definitions sync: commit: %w", err)
	}

	r.mu.Lock()
	r.synced, r.heads, r.syncErrs = true, heads, syncErrs
	r.mu.Unlock()
	return nil
}

// current re-judges name's valid rows, newest first, and returns the first
// that still passes. A row that fails is flipped invalid in place (effective
// kept), which is what makes a ceiling tightened in a core upgrade revoke old
// versions. Rows per name are a handful.
//
// ponytail: re-judges the ceiling only. The client-agreement half of Load
// (invokers) is not re-run on old rows; invokers is inert until P4.
func (r *Registry) current(ctx context.Context, tx pgx.Tx, name string) (Head, error) {
	rows, err := tx.Query(ctx, `
		SELECT sha, effective, synced_at FROM agent_definitions
		 WHERE name = $1 AND valid ORDER BY synced_at DESC, sha`, name)
	if err != nil {
		return Head{}, fmt.Errorf("definitions sync: current %s: %w", name, err)
	}
	type row struct {
		sha      string
		eff      []byte
		syncedAt time.Time
	}
	var all []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.sha, &x.eff, &x.syncedAt); err != nil {
			rows.Close()
			return Head{}, err
		}
		all = append(all, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Head{}, err
	}
	for _, x := range all {
		var s Spec
		dec := json.NewDecoder(bytes.NewReader(x.eff))
		dec.DisallowUnknownFields()
		err := dec.Decode(&s)
		if err == nil {
			err = Validate(name, &s)
		}
		if err == nil {
			return Head{Name: name, SHA: x.sha, Effective: &s, SyncedAt: x.syncedAt}, nil
		}
		slog.Warn("definitions: stored version revoked by the current ceiling", "name", name, "sha", x.sha, "error", err)
		if _, err := tx.Exec(ctx, `UPDATE agent_definitions SET valid = false, error = $3 WHERE name = $1 AND sha = $2`,
			name, x.sha, "revoked by the current ceiling: "+err.Error()); err != nil {
			return Head{}, fmt.Errorf("definitions sync: revoke %s@%s: %w", name, x.sha, err)
		}
	}
	return Head{Name: name}, nil
}

func nullJSON(b []byte) any {
	if b == nil {
		return nil
	}
	return string(b)
}

// List returns every present definition's head, by name, and the last
// sync's errors that have no row.
func (r *Registry) List() ([]Head, []string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Head, 0, len(r.heads))
	for _, h := range r.heads {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, append([]string(nil), r.syncErrs...)
}

// Resolve returns name's current valid version: ErrNotSynced before the first
// sync, ErrUnknown when it is not in git, ErrInvalid (wrapped with the latest
// error) when it has no valid version.
func (r *Registry) Resolve(name string) (Head, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.synced {
		return Head{}, ErrNotSynced
	}
	h, ok := r.heads[name]
	if !ok {
		return Head{}, fmt.Errorf("%w %q", ErrUnknown, name)
	}
	if h.SHA == "" {
		return Head{}, fmt.Errorf("%w: %s", ErrInvalid, h.Err)
	}
	return h, nil
}

// Run syncs now and then every interval until ctx ends. Its own goroutine,
// not a step of sessions.Loop: a sync of up to syncTimeout would otherwise
// slip that serial loop's reconcile and sweeps by as much.
//
// ponytail: a fixed poll. At a release tag the fetch is a no-op; a webhook
// can replace this if a branch override ever needs to be faster.
func (r *Registry) Run(ctx context.Context, interval time.Duration) {
	for {
		if err := r.Sync(ctx); err != nil {
			slog.Error("definitions: sync failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
