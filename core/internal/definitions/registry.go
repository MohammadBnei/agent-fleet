package definitions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentfleetv1 "github.com/MohammadBnei/agent-fleet/proto/gen/go/agentfleet/v1"
)

// Lister is the provisioner's ListDefinitionFiles (provisionerclient.Client).
type Lister interface {
	ListDefinitionFiles(ctx context.Context) (map[string][]byte, string, error)
}

var (
	ErrUnknown   = errors.New("no such definition")
	ErrInvalid   = errors.New("definition is invalid at HEAD")
	errNotSynced = errors.New("definitions not synced yet")
	errStale     = errors.New("definitions are stale")
)

// staleAfter bounds last-known-good: past it Resolve fails closed, so a
// narrowing or deletion that core cannot fetch is never papered over by the
// wider version it replaced for longer than this. Five sync intervals.
const staleAfter = 5 * time.Minute

// Head is one present definition's current state: the row of the content at
// the listed commit when that row is valid (what a new session pins), or its
// error. Never an older version: an invalid HEAD fails closed, so a narrowing
// edit that also carries a mistake cannot resurrect the wider version it
// replaced.
type Head struct {
	Name      string
	SHA       string // "" when HEAD's content is invalid
	Effective *Spec  // nil when HEAD's content is invalid
	SyncedAt  time.Time
	Err       string // set only when HEAD's content is invalid
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
	// lastErr is the last failed sync's error, cleared by a good one: the
	// heads are then last-known-good, and List says so.
	lastErr  error
	lastGood time.Time
	// onFiles sees every good listing (docs/adr/0063: packs/ rides along).
	onFiles func(ctx context.Context, files map[string][]byte, sha string)
}

// SetOnFiles hands every listing whose definitions were stored to f, with its
// own time budget.
func (r *Registry) SetOnFiles(f func(ctx context.Context, files map[string][]byte, sha string)) {
	r.onFiles = f
}

func NewRegistry(pool *pgxpool.Pool, lister Lister) *Registry {
	return &Registry{pool: pool, lister: lister}
}

const syncTimeout = 30 * time.Second

// Sync lists, validates and stores every definition, then re-judges every
// stored valid row under this core's ceiling. On any error the previous state
// is kept (last known good), so a provisioner blip does not unpublish
// definitions, and List reports the failure. Past staleAfter, Resolve fails
// closed.
func (r *Registry) Sync(ctx context.Context) error {
	err := r.sync(ctx)
	r.mu.Lock()
	r.lastErr = err
	r.mu.Unlock()
	return err
}

func (r *Registry) sync(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()

	files, sha, err := r.lister.ListDefinitionFiles(ctx)
	if err != nil {
		return err
	}
	results, fileErrs := Load(files)
	syncErrs := make([]string, 0, len(fileErrs))
	for _, e := range fileErrs {
		syncErrs = append(syncErrs, e.Error())
	}
	// Logged when the set changes, not every 60s: the console shows them,
	// and a known, persisted state at error level every minute buries real
	// errors.
	r.mu.RLock()
	changed := !slices.Equal(syncErrs, r.syncErrs)
	r.mu.RUnlock()
	if changed {
		for _, e := range fileErrs {
			slog.Error("definitions: file error", "sha", sha, "error", e)
		}
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
			// Load refuses a NUL in the file; this covers one in an error
			// string, which TEXT cannot hold either.
			msg := strings.ReplaceAll(res.Err.Error(), "\x00", `\0`)
			errText = &msg
		} else if effective, err = json.Marshal(res.Effective); err != nil {
			return fmt.Errorf("definitions sync: marshal %s: %w", res.Name, err)
		}
		// Reuse any row with identical content (a revert after a typo gets
		// the old sha back), else a new version at this sha. Every release
		// commit moves HEAD; without this every definition would get a row
		// per commit, and the pin would stop meaning "this content".
		//
		// The same (name, sha) judged differently (a core rollout overlap, a
		// core upgrade) never rewrites a VALID row's content: sessions may pin
		// it. It may only be invalidated (effective kept), and the re-judge in
		// current() holds the kept content to this core's ceiling. An invalid
		// row is not launchable, so it is replaced outright (an older core's
		// strict decode rejecting a field a newer core knows).
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
			// New content, or a new judgement of it: the one time it is logged.
			if res.Err != nil {
				slog.Warn("definitions: invalid", "name", res.Name, "sha", sha, "error", res.Err)
			}
			err = tx.QueryRow(ctx, `
				INSERT INTO agent_definitions (name, sha, spec, effective, valid, error)
				VALUES ($1, $2, $3::jsonb, $4::jsonb, $5, $6)
				ON CONFLICT (name, sha) DO UPDATE
				   SET spec      = CASE WHEN agent_definitions.valid THEN agent_definitions.spec ELSE EXCLUDED.spec END,
				       effective = CASE WHEN agent_definitions.valid THEN agent_definitions.effective
				                        ELSE COALESCE(EXCLUDED.effective, agent_definitions.effective) END,
				       valid = EXCLUDED.valid, error = EXCLUDED.error, synced_at = now()
				RETURNING sha`, res.Name, sha, nullJSON(res.Own), nullJSON(effective), res.Err == nil, errText).Scan(&rowSHA)
		}
		if err != nil {
			return fmt.Errorf("definitions sync: upsert %s: %w", res.Name, err)
		}
		h, err := r.current(ctx, tx, res.Name, rowSHA)
		if err != nil {
			return err
		}
		heads[res.Name] = h
	}
	// Names no longer listed (deleted, or now abstract) keep their rows, and
	// sessions keep their pins: re-judge those too, so a tightened ceiling
	// also revokes a version nothing in git names any more.
	var gone []string
	rows, err := tx.Query(ctx, `SELECT DISTINCT name FROM agent_definitions WHERE valid`)
	if err != nil {
		return fmt.Errorf("definitions sync: names: %w", err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		if _, ok := heads[n]; !ok {
			gone = append(gone, n)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, n := range gone {
		if _, err := r.current(ctx, tx, n, ""); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("definitions sync: commit: %w", err)
	}

	r.mu.Lock()
	r.synced, r.heads, r.syncErrs, r.lastGood = true, heads, syncErrs, time.Now()
	r.mu.Unlock()
	if r.onFiles != nil {
		hookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncTimeout)
		defer cancel()
		r.onFiles(hookCtx, files, sha)
	}
	return nil
}

// current re-judges name's valid rows under this core's ceiling and returns
// headSHA's row as the head when it is (still) valid ("" judges only). A row
// that fails is
// flipped invalid in place (effective kept), which is what makes a ceiling
// tightened in a core upgrade revoke old versions, including the ones live
// pods are pinned to (sessions.SetPermissionMode reads valid). Rows per name
// are a handful.
//
// ponytail: re-judges the ceiling only. The client-agreement half of Load
// (invokers) is not re-run on old rows; invokers is inert until P4.
func (r *Registry) current(ctx context.Context, tx pgx.Tx, name, headSHA string) (Head, error) {
	head := Head{Name: name}
	if headSHA != "" {
		if err := tx.QueryRow(ctx, `SELECT COALESCE(error, '') FROM agent_definitions WHERE name = $1 AND sha = $2`,
			name, headSHA).Scan(&head.Err); err != nil {
			return Head{}, fmt.Errorf("definitions sync: head %s: %w", name, err)
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT sha, effective, synced_at FROM agent_definitions
		 WHERE name = $1 AND valid`, name)
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
		if err := dec.Decode(&s); err != nil {
			// Only a core writes effective, so a row this core cannot decode
			// was written by a NEWER one (a rollout overlap, a rollback). Not
			// this core's to judge: never revoke it, or the newer core would
			// re-insert at its sha and strand every session pinned to this one.
			// It is still no head for THIS core.
			// Debug: it is seen every sync until the newer core is back.
			slog.Debug("definitions: stored version not understood by this core, left as is", "name", name, "sha", x.sha, "error", err)
			if x.sha == headSHA {
				head.Err = "stored version not understood by this core: " + err.Error()
			}
			continue
		}
		err := validate(name, &s)
		if err == nil {
			if x.sha == headSHA {
				head.SHA, head.Effective, head.SyncedAt = x.sha, &s, x.syncedAt
			}
			continue
		}
		slog.Warn("definitions: stored version revoked by the current ceiling", "name", name, "sha", x.sha, "error", err)
		msg := "revoked by the current ceiling: " + err.Error()
		if _, err := tx.Exec(ctx, `UPDATE agent_definitions SET valid = false, error = $3 WHERE name = $1 AND sha = $2`,
			name, x.sha, msg); err != nil {
			return Head{}, fmt.Errorf("definitions sync: revoke %s@%s: %w", name, x.sha, err)
		}
		if x.sha == headSHA {
			head.Err = msg
		}
	}
	return head, nil
}

func nullJSON(b []byte) any {
	if b == nil {
		return nil
	}
	return string(b)
}

// List returns every present definition's head, by name, and the sync
// errors that have no row: the last good sync's file errors, plus "not synced
// yet" or the last failed sync's error, so the console can tell an empty or
// stale list from a real one.
func (r *Registry) List() ([]Head, []string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Head, 0, len(r.heads))
	for _, h := range r.heads {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	errs := append([]string(nil), r.syncErrs...)
	switch {
	case !r.synced && r.lastErr != nil:
		errs = append(errs, fmt.Sprintf("%v: %v", errNotSynced, r.lastErr))
	case !r.synced:
		errs = append(errs, errNotSynced.Error())
	case time.Since(r.lastGood) > staleAfter:
		errs = append(errs, fmt.Sprintf("%v: no good sync for over %s, so no definition session can be created: %v", errStale, staleAfter, r.lastErr))
	case r.lastErr != nil:
		errs = append(errs, "last sync failed, showing the last good one: "+r.lastErr.Error())
	}
	return out, errs
}

// Resolve returns name's current version: errNotSynced before the first sync,
// errStale when the last good sync is older than staleAfter, ErrUnknown when
// it is not in git, ErrInvalid (wrapped with HEAD's error) when the content at
// HEAD is invalid.
func (r *Registry) Resolve(name string) (Head, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.synced {
		return Head{}, errNotSynced
	}
	if age := time.Since(r.lastGood); age > staleAfter {
		return Head{}, fmt.Errorf("%w: no good sync for %s: %v", errStale, age.Round(time.Second), r.lastErr)
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

// Vaults is every valid HEAD definition's vault after the last good sync
// (docs/adr/0065): the full desired set core pushes with ReconcileVaults. An
// invalid-at-HEAD or deleted definition is simply absent, and the
// provisioner's prune empties absent vaults: the same fail-closed rule as
// Resolve. Key names only; values live on the provisioner's side.
func (r *Registry) Vaults() []*agentfleetv1.VaultSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*agentfleetv1.VaultSpec{}
	for _, h := range r.heads {
		if h.Effective != nil && h.Effective.Credentials != nil {
			out = append(out, VaultSpec(h.Effective.Credentials))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
}

// VaultSpec is a definition's credentials on the provisioner wire.
func VaultSpec(c *Credentials) *agentfleetv1.VaultSpec {
	v := &agentfleetv1.VaultSpec{Name: c.Vault}
	for _, s := range c.Services {
		v.Services = append(v.Services, &agentfleetv1.VaultService{Host: s.Host, Paths: s.Paths, Secret: s.Secret})
	}
	return v
}
