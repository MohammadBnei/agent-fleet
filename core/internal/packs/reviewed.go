package packs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const (
	// staleAfter matches the definitions registry's: past it, a warm still
	// uses the last good reviewed packs, and says so (ADR-0063 §5).
	staleAfter = 5 * time.Minute
	// Each reviewed pack gets a quarter of the per-target caps, so fleet plus
	// repo always leave room for live packs.
	maxReviewedText   = MaxText / 4
	maxReviewedSkills = MaxSkills / 4
	// The provisioner's markers (provisioner/internal/git ReadDefinitions).
	listedMarker  = "packs/.listed.md"
	invalidMarker = ".invalid.md"
)

// reviewedNames are the only reviewed packs until definitions can name one
// (ADR-0063 Phase D). Both are reserved for live packs, so the two never share
// a name.
var reviewedNames = map[string]bool{"fleet": true, "repo": true}

// ParseReviewed reads packs/<name>/PACK.md and packs/<name>/skills/<skill>/SKILL.md
// out of a definitions listing. Anything wrong with a pack (another file, a
// provisioner .invalid.md marker, bad bytes, an @ import, size) fails that
// pack alone, keyed by name in bad.
// ponytail: text and skills only. MCP and plugin files fail their pack until
// the phase that wires them.
func ParseReviewed(files map[string][]byte) (good map[string]Version, bad map[string]string) {
	good, bad = map[string]Version{}, map[string]string{}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		rest, ok := strings.CutPrefix(p, "packs/")
		if !ok || p == listedMarker {
			continue
		}
		name, file, _ := strings.Cut(rest, "/")
		body := files[p]
		if !reviewedNames[name] {
			bad[name] = fmt.Sprintf("%s: only packs/fleet and packs/repo are read for now", p)
			continue
		}
		v := good[name]
		v.Pack = name
		parts := strings.Split(file, "/")
		switch {
		case file == invalidMarker:
			bad[name] = string(body)
		case !utf8.Valid(body) || strings.ContainsRune(string(body), 0):
			bad[name] = p + ": not UTF-8 text"
		case importRe.Match(body):
			bad[name] = p + ": @path imports are not allowed"
		case file == "PACK.md":
			v.Text = string(body)
		case len(parts) == 3 && parts[0] == "skills" && parts[2] == "SKILL.md" && nameRe.MatchString(parts[1]):
			v.Skills = append(v.Skills, Skill{Name: parts[1], Body: string(body)})
		default:
			bad[name] = p + ": not PACK.md or skills/<name>/SKILL.md"
		}
		good[name] = v
	}
	for name, v := range good {
		if text, skills := composedSize([]Version{v}); text > maxReviewedText || skills > maxReviewedSkills {
			bad[name] = fmt.Sprintf("packs/%s: %d text / %d skill bytes, max %d / %d", name, text, skills, maxReviewedText, maxReviewedSkills)
		}
	}
	for name := range bad {
		delete(good, name)
	}
	return good, bad
}

// SyncReviewed stores the listing's reviewed packs at sha. A pack that fails
// keeps its last good content and records why, so render reports it. Without
// the provisioner's packs/.listed.md marker (an older provisioner) nothing
// changes; with it, a pack absent from the listing is removed.
func (s *Store) SyncReviewed(ctx context.Context, files map[string][]byte, sha string) map[string]string {
	if _, ok := files[listedMarker]; !ok {
		return nil
	}
	good, bad := ParseReviewed(files)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		present := []string{}
		for name := range good {
			present = append(present, name)
		}
		for name := range bad {
			present = append(present, name)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM reviewed_packs WHERE NOT (name = ANY($1))`, present); err != nil {
			return err
		}
		for name, v := range good {
			skills := v.Skills
			if skills == nil {
				skills = []Skill{}
			}
			body, err := json.Marshal(skills)
			if err != nil {
				return err
			}
			// sha moves only with the content, so an unchanged pack keeps its id
			// across release commits.
			if _, err := tx.Exec(ctx, `
				INSERT INTO reviewed_packs (name, sha, text, skills, error, synced_at) VALUES ($1, $2, $3, $4, NULL, now())
				ON CONFLICT (name) DO UPDATE SET
				  sha = CASE WHEN reviewed_packs.text = $3 AND reviewed_packs.skills = $4::jsonb THEN reviewed_packs.sha ELSE $2 END,
				  text = $3, skills = $4, error = NULL, synced_at = now()`,
				name, sha, v.Text, body); err != nil {
				return err
			}
		}
		for name, why := range bad {
			// A first commit that fails has no last good content: an empty row
			// that renders nothing but carries the reason.
			if _, err := tx.Exec(ctx, `
				INSERT INTO reviewed_packs (name, sha, error) VALUES ($1, $2, $3)
				ON CONFLICT (name) DO UPDATE SET error = $3`, name, sha, why); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		bad["*"] = "store reviewed packs: " + err.Error()
	}
	return bad
}

// OnDefinitionFiles is the definitions registry's hook. Errors are logged
// when they change, like the registry's own file errors, never every tick.
func (s *Store) OnDefinitionFiles(ctx context.Context, files map[string][]byte, sha string) {
	bad := s.SyncReviewed(ctx, files, sha)
	keys := make([]string, 0, len(bad))
	for name, why := range bad {
		keys = append(keys, name+": "+why)
	}
	sort.Strings(keys)
	summary := strings.Join(keys, "\n")
	s.mu.Lock()
	changed := summary != s.lastReviewedErrs
	s.lastReviewedErrs = summary
	s.mu.Unlock()
	if changed {
		for _, k := range keys {
			slog.Error("packs: reviewed pack", "sha", sha, "error", k)
		}
	}
}

// reviewed loads the last good fleet pack and, for a repo session, the repo
// pack (nil when absent), with a note for each one past staleAfter or
// carrying an error.
func reviewed(ctx context.Context, q querier, withRepo bool) (repo, fleet *Version, notes []string, err error) {
	names := []string{"fleet"}
	if withRepo {
		names = append(names, "repo")
	}
	rows, err := q.Query(ctx, `SELECT name, sha, text, skills, error, synced_at FROM reviewed_packs WHERE name = ANY($1) ORDER BY name`, names)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load reviewed packs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v Version
		var skills []byte
		var why *string
		var synced time.Time
		if err := rows.Scan(&v.Pack, &v.Ref, &v.Text, &skills, &why, &synced); err != nil {
			return nil, nil, nil, err
		}
		if err := json.Unmarshal(skills, &v.Skills); err != nil {
			return nil, nil, nil, fmt.Errorf("reviewed pack %s skills: %w", v.Pack, err)
		}
		if why != nil {
			notes = append(notes, fmt.Sprintf("reviewed:%s: last sync failed, using the last good copy: %s", v.id(), *why))
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
