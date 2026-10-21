package packs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrExists = errors.New("pack already exists")
	// ErrNotFound covers an unknown pack, and a repo with no GitHub id yet.
	ErrNotFound = errors.New("not found")
	// ErrInvalid wraps every refusal of the pack's own content or size: the
	// caller's fault, never the database's.
	ErrInvalid = errors.New("invalid pack")
)

const (
	KindDefinition = "definition"
	KindRepo       = "repo"
	// AllTargets attaches a pack to every target of its kind, current and
	// future, so a pack meant for the whole fleet needs no per-repo rows.
	AllTargets = "*"
)

type Pack struct {
	Name       string
	OwnerGroup string
	Latest     int // 0 = no version saved yet
	// The latest version's content, and where the pack is attached.
	Text        string
	Skills      []Skill
	Attachments []Attachment
}

// Attachment names a target the way the console does: a definition by name,
// a repo by its fleet name, or "#<github id>" for a repo that left the fleet.
type Attachment struct {
	Kind   string
	Target string
}

type Store struct {
	pool *pgxpool.Pool

	mu               sync.Mutex
	lastReviewedErrs string // logged on change only
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func (s *Store) Create(ctx context.Context, name, ownerGroup string) error {
	if err := checkPackName(name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	// One namespace with the reviewed packs (ADR-0063 §1): checkPackName
	// reserves every reviewed name.
	_, err := s.pool.Exec(ctx, `INSERT INTO packs (name, owner_group) VALUES ($1, $2)`, name, ownerGroup)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrExists
	}
	if err != nil {
		return fmt.Errorf("create pack: %w", err)
	}
	slog.Info("packs Create", "name", name, "owner_group", ownerGroup)
	return nil
}

func (s *Store) List(ctx context.Context) ([]Pack, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.name, p.owner_group, COALESCE(v.version, 0), COALESCE(v.text, ''), COALESCE(v.skills, '[]')
		FROM packs p LEFT JOIN LATERAL (
		  SELECT version, text, skills FROM pack_versions WHERE pack = p.name ORDER BY version DESC LIMIT 1
		) v ON true
		ORDER BY p.name`)
	if err != nil {
		return nil, fmt.Errorf("list packs: %w", err)
	}
	result := []Pack{}
	index := map[string]int{}
	for rows.Next() {
		var p Pack
		var skills []byte
		if err := rows.Scan(&p.Name, &p.OwnerGroup, &p.Latest, &p.Text, &skills); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan pack: %w", err)
		}
		if err := json.Unmarshal(skills, &p.Skills); err != nil {
			rows.Close()
			return nil, fmt.Errorf("pack %s skills: %w", p.Name, err)
		}
		index[p.Name] = len(result)
		result = append(result, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.pool.Query(ctx, `
		SELECT a.pack, a.target_kind, CASE WHEN a.target_kind = 'repo' AND a.target <> '*' THEN COALESCE(r.name, '#' || a.target) ELSE a.target END
		FROM pack_attachments a LEFT JOIN repos r
		  -- CASE, not AND: Postgres does not promise to short-circuit, and a
		  -- definition name must never reach the cast.
		  ON r.github_id = CASE WHEN a.target_kind = 'repo' AND a.target ~ '^[0-9]+$' THEN a.target::bigint END
		ORDER BY 1, 2, 3`)
	if err != nil {
		return nil, fmt.Errorf("list attachments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pack string
		var a Attachment
		if err := rows.Scan(&pack, &a.Kind, &a.Target); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		if i, ok := index[pack]; ok {
			result[i].Attachments = append(result[i].Attachments, a)
		}
	}
	return result, rows.Err()
}

// Delete removes the pack, its versions and its attachments.
func (s *Store) Delete(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM packs WHERE name = $1`, name)
	if err != nil {
		return fmt.Errorf("delete pack: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	slog.Info("packs Delete", "name", name)
	return nil
}

// SaveVersion validates v and stores it as the pack's next version.
func (s *Store) SaveVersion(ctx context.Context, v Version, by string) (int, error) {
	if err := Validate(v); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	skills := v.Skills
	if skills == nil {
		skills = []Skill{}
	}
	body, err := json.Marshal(skills)
	if err != nil {
		return 0, err
	}
	// The packs row lock serializes concurrent saves of one pack. NO KEY
	// UPDATE, not UPDATE: Attach's FK check takes KEY SHARE on the same row
	// while holding its target lock, and the two must not deadlock.
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(ctx, `SELECT name FROM packs WHERE name = $1 FOR NO KEY UPDATE`, v.Pack).Scan(&name); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("pack %q: %w", v.Pack, ErrNotFound)
			}
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO pack_versions (pack, version, text, skills, created_by)
			SELECT $1, COALESCE(MAX(version), 0) + 1, $2, $3, $4 FROM pack_versions WHERE pack = $1
			RETURNING version`, v.Pack, v.Text, body, by).Scan(&v.Version); err != nil {
			return err
		}
		// The new version is now the latest everywhere the pack is attached.
		rows, err := tx.Query(ctx, `SELECT target_kind, target FROM pack_attachments WHERE pack = $1 ORDER BY 1, 2`, v.Pack)
		if err != nil {
			return err
		}
		targets, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Kind, Target string }])
		if err != nil {
			return err
		}
		for _, t := range targets {
			if err := checkComposed(ctx, tx, t.Kind, t.Target); err != nil {
				return fmt.Errorf("version %d of %q: %w", v.Version, v.Pack, err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("save pack version: %w", err)
	}
	slog.Info("packs SaveVersion", "pack", v.Pack, "version", v.Version, "by", by)
	return v.Version, nil
}

// targetID maps a fleet-facing target to the stored key: a definition by
// name, a repo by its GitHub id (ADR-0074), so an override rename keeps it.
// AllTargets is stored as itself.
func targetID(ctx context.Context, q querier, kind, name string) (string, error) {
	if name == AllTargets && (kind == KindDefinition || kind == KindRepo) {
		return AllTargets, nil
	}
	switch kind {
	case KindDefinition:
		return name, nil
	case KindRepo:
		// List shows a repo that left the fleet as "#<github id>"; that is
		// already the stored key, so it can still be detached.
		if id, ok := strings.CutPrefix(name, "#"); ok && id != "" && strings.Trim(id, "0123456789") == "" {
			return id, nil
		}
		var id *int64
		err := q.QueryRow(ctx, `SELECT github_id FROM repos WHERE name = $1`, name).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && id == nil) {
			return "", fmt.Errorf("repo %q has no GitHub id: %w", name, ErrNotFound)
		}
		if err != nil {
			return "", err
		}
		return fmt.Sprint(*id), nil
	}
	return "", fmt.Errorf("unknown target kind %q", kind)
}

// Attach attaches pack to a target, refusing when the target's composed
// latest versions would exceed the caps.
func (s *Store) Attach(ctx context.Context, kind, name, pack, by string) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		target, err := targetID(ctx, tx, kind, name)
		if err != nil {
			return err
		}
		if kind == KindDefinition && target != AllTargets {
			var known bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_definitions WHERE name = $1)`, name).Scan(&known); err != nil {
				return err
			}
			if !known {
				return fmt.Errorf("definition %q: %w", name, ErrNotFound)
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO pack_attachments (target_kind, target, pack, attached_by)
			VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, kind, target, pack, by); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				return fmt.Errorf("pack %q: %w", pack, ErrNotFound)
			}
			return err
		}
		if err := checkComposed(ctx, tx, kind, target); err != nil {
			return fmt.Errorf("attaching %q to %s %q: %w", pack, kind, name, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	slog.Info("packs Attach", "kind", kind, "target", name, "pack", pack, "by", by)
	return nil
}

func (s *Store) Detach(ctx context.Context, kind, name, pack string) error {
	target, err := targetID(ctx, s.pool, kind, name)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM pack_attachments WHERE target_kind = $1 AND target = $2 AND pack = $3`,
		kind, target, pack)
	if err != nil {
		return fmt.Errorf("detach pack: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	slog.Info("packs Detach", "kind", kind, "target", name, "pack", pack)
	return nil
}

// RenderContext renders what a warm of this target loads: its attached live
// packs at their latest version, plus the last good reviewed packs (fleet for
// every pod, repo for a repo session). A repo with no GitHub id renders
// with only its kind's "*" packs rather than failing the warm.
func (s *Store) RenderContext(ctx context.Context, kind, name string) (Rendered, error) {
	repo, fleet, notes, err := reviewed(ctx, s.pool, kind == KindRepo)
	if err != nil {
		return Rendered{}, err
	}
	var live []Version
	target, err := targetID(ctx, s.pool, kind, name)
	switch {
	case errors.Is(err, ErrNotFound):
		// Its own packs are unreachable, the kind's "*" packs are not.
		notes = append(notes, err.Error())
		if live, err = latest(ctx, s.pool, kind, AllTargets); err != nil {
			return Rendered{}, err
		}
	case err != nil:
		return Rendered{}, err
	default:
		if live, err = latest(ctx, s.pool, kind, target); err != nil {
			return Rendered{}, err
		}
	}
	r := Render(live, repo, fleet)
	r.Skipped = append(r.Skipped, notes...)
	return r, nil
}

// checkComposed refuses a target whose attached latest versions, plus the
// reviewed packs it gets, exceed the caps. AllTargets checks every target of
// the kind that has its own packs, since each composes with it. The per-kind
// advisory lock makes two concurrent attaches or saves see each other's rows,
// a wildcard one included. Render still enforces the sum at warm time, when a
// reviewed pack may have grown.
// ponytail: one lock per kind; per-target locks if attach contention shows.
func checkComposed(ctx context.Context, tx pgx.Tx, kind, target string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('packs:' || $1))`, kind); err != nil {
		return err
	}
	targets := []string{target}
	if target == AllTargets {
		rows, err := tx.Query(ctx, `SELECT DISTINCT target FROM pack_attachments WHERE target_kind = $1 AND target <> '*'`, kind)
		if err != nil {
			return err
		}
		more, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		targets = append(targets, more...)
	}
	repo, fleet, _, err := reviewed(ctx, tx, kind == KindRepo)
	if err != nil {
		return err
	}
	for _, t := range targets {
		live, err := latest(ctx, tx, kind, t)
		if err != nil {
			return err
		}
		for _, v := range []*Version{repo, fleet} {
			if v != nil {
				live = append(live, *v)
			}
		}
		// Named, since under "*" it may be any target of the kind, a repo
		// that left the fleet (#<github id>) included.
		if text, skills := composedSize(live); text > MaxText || skills > MaxSkills {
			return fmt.Errorf("%w: %s %s: composed size %d text / %d skill bytes, max %d / %d", ErrInvalid, kind, t, text, skills, MaxText, MaxSkills)
		}
	}
	return nil
}

func latest(ctx context.Context, q querier, kind, target string) ([]Version, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT ON (v.pack) v.pack, v.version, v.text, v.skills
		FROM pack_attachments a JOIN pack_versions v ON v.pack = a.pack
		WHERE a.target_kind = $1 AND a.target IN ($2, '*')
		ORDER BY v.pack, v.version DESC`, kind, target)
	if err != nil {
		return nil, fmt.Errorf("load attached packs: %w", err)
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		var v Version
		var skills []byte
		if err := rows.Scan(&v.Pack, &v.Version, &v.Text, &skills); err != nil {
			return nil, fmt.Errorf("scan pack version: %w", err)
		}
		if err := json.Unmarshal(skills, &v.Skills); err != nil {
			return nil, fmt.Errorf("pack %s@%d skills: %w", v.Pack, v.Version, err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
