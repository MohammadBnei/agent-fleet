package packs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// staleAfter matches the definitions registry's: past it, a warm still uses
// the last good reviewed packs, and says so (ADR-0063 §5).
const staleAfter = 5 * time.Minute

// ParseReviewed reads packs/<name>/PACK.md and packs/<name>/skills/<skill>/SKILL.md
// out of a definitions listing. Any other file under a pack, or a bad name,
// fails that pack alone; the rest still sync.
// ponytail: text and skills only. MCP and plugin files fail their pack until
// the phase that wires them, so no reviewed pack carries them yet.
func ParseReviewed(files map[string][]byte) (map[string]Version, []error) {
	out := map[string]Version{}
	bad := map[string]error{}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		rest, ok := strings.CutPrefix(p, "packs/")
		if !ok {
			continue
		}
		name, file, _ := strings.Cut(rest, "/")
		if !nameRe.MatchString(name) {
			bad[name] = fmt.Errorf("%s: pack name must match %s", p, nameRe)
			continue
		}
		v := out[name]
		v.Pack = name
		parts := strings.Split(file, "/")
		switch {
		case file == "PACK.md":
			v.Text = string(files[p])
		case len(parts) == 3 && parts[0] == "skills" && parts[2] == "SKILL.md" && nameRe.MatchString(parts[1]):
			v.Skills = append(v.Skills, Skill{Name: parts[1], Body: string(files[p])})
		default:
			bad[name] = fmt.Errorf("%s: not PACK.md or skills/<name>/SKILL.md", p)
		}
		out[name] = v
	}
	var errs []error
	for name, err := range bad {
		delete(out, name)
		errs = append(errs, err)
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return out, errs
}

// SyncReviewed stores the listing's reviewed packs at sha. A pack that failed
// to parse keeps its previous row (and its old synced_at, so render reports
// it stale); a pack no longer in the listing is removed. A listing with no
// packs/ at all keeps everything: that is what an older provisioner answers,
// and packs/fleet always exists once shipped.
func (s *Store) SyncReviewed(ctx context.Context, files map[string][]byte, sha string) []error {
	parsed, errs := ParseReviewed(files)
	var listed []string
	seen := map[string]bool{}
	for p := range files {
		if rest, ok := strings.CutPrefix(p, "packs/"); ok {
			if name, _, _ := strings.Cut(rest, "/"); !seen[name] {
				seen[name] = true
				listed = append(listed, name)
			}
		}
	}
	if len(listed) == 0 {
		return errs
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM reviewed_packs WHERE NOT (name = ANY($1))`, listed); err != nil {
			return err
		}
		for name, v := range parsed {
			var live bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM packs WHERE name = $1)`, name).Scan(&live); err != nil {
				return err
			}
			if live {
				// One namespace (ADR-0063 §1): rename one of them.
				errs = append(errs, fmt.Errorf("packs/%s: a live pack has the same name", name))
				continue
			}
			skills := v.Skills
			if skills == nil {
				skills = []Skill{}
			}
			body, err := json.Marshal(skills)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO reviewed_packs (name, sha, text, skills, synced_at) VALUES ($1, $2, $3, $4, now())
				ON CONFLICT (name) DO UPDATE SET sha = $2, text = $3, skills = $4, synced_at = now()`,
				name, sha, v.Text, body); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("store reviewed packs: %w", err))
	}
	return errs
}

// OnDefinitionFiles is the definitions registry's hook: every good listing
// also syncs the reviewed packs. Errors are logged, never fatal to the
// definitions sync.
func (s *Store) OnDefinitionFiles(ctx context.Context, files map[string][]byte, sha string) {
	for _, err := range s.SyncReviewed(ctx, files, sha) {
		slog.Error("packs: reviewed pack sync", "sha", sha, "error", err)
	}
}

// reviewed loads the last good fleet pack and, for a repo session, the repo
// pack (nil when absent), with a note for each one past staleAfter.
func reviewed(ctx context.Context, q querier, withRepo bool) (repo, fleet *Version, notes []string, err error) {
	names := []string{"fleet"}
	if withRepo {
		names = append(names, "repo")
	}
	rows, err := q.Query(ctx, `SELECT name, sha, text, skills, synced_at FROM reviewed_packs WHERE name = ANY($1)`, names)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load reviewed packs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v Version
		var skills []byte
		var synced time.Time
		if err := rows.Scan(&v.Pack, &v.Ref, &v.Text, &skills, &synced); err != nil {
			return nil, nil, nil, err
		}
		if err := json.Unmarshal(skills, &v.Skills); err != nil {
			return nil, nil, nil, fmt.Errorf("reviewed pack %s skills: %w", v.Pack, err)
		}
		if age := time.Since(synced); age > staleAfter {
			notes = append(notes, fmt.Sprintf("reviewed:%s: stale, last synced %s ago", v.id(), age.Round(time.Second)))
		}
		if v.Pack == "fleet" {
			fleet = &v
		} else {
			repo = &v
		}
	}
	return repo, fleet, notes, rows.Err()
}
