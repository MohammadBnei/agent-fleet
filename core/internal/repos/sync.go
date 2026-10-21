package repos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"
)

// GitHubRepo is one entry of the provisioner's ListGitHubRepos: a repo the
// bot owns or collaborates on (docs/adr/0074).
type GitHubRepo struct {
	ID            int64
	FullName      string
	Name          string
	CloneURL      string
	DefaultBranch string
}

// Override is one repo's entry in infra-bootstrap's fleet-repo-overrides
// file, keyed by owner/repo. ID must equal the live repo's GitHub id, so an
// entry never follows a name someone else re-created.
type Override struct {
	ID            int64  `yaml:"id"`
	Name          string `yaml:"name"`
	BaseBranch    string `yaml:"baseBranch"`
	Image         string `yaml:"image"`
	ClusterAccess bool   `yaml:"clusterAccess"`
}

// ParseOverrides decodes the overrides file. Empty is valid and means no
// overrides: that is how every grant is revoked. An unknown field is a
// warning, not an error, so a typo in one entry does not freeze the rest.
func ParseOverrides(b []byte) (map[string]Override, error) {
	// A top-level typo ("repo:") would otherwise decode as "no overrides",
	// revoking every grant and renaming every name-overridden row.
	var top map[string]any
	if err := yaml.Unmarshal(b, &top); err != nil {
		return nil, err
	}
	for k := range top {
		if k != "repos" {
			return nil, fmt.Errorf("overrides: unknown top-level key %q (want only \"repos\")", k)
		}
	}
	var doc struct {
		Repos map[string]Override `yaml:"repos"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		strictErr := err
		doc.Repos = nil
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return nil, err
		}
		slog.Warn("repos: overrides has unknown fields, ignored", "error", strictErr)
	}
	// GitHub owner/repo is case-insensitive; the listing returns the
	// canonical case, a hand-written key may not.
	out := make(map[string]Override, len(doc.Repos))
	for k, v := range doc.Repos {
		out[strings.ToLower(k)] = v
	}
	return out, nil
}

// Overrides keeps the last good copy of the overrides file. A missing file
// is a mount problem, never "no overrides", so it keeps what was read
// before; until one good read, Load reports !ok and the caller does not sync
// at all, so a core restart with a missing ConfigMap never revokes a grant.
type Overrides struct {
	Path string

	mu   sync.Mutex
	m    map[string]Override
	ok   bool
	last string // last logged error, so a standing one is logged once
}

func (o *Overrides) Load() (map[string]Override, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	b, err := os.ReadFile(o.Path)
	if err == nil {
		var m map[string]Override
		if m, err = ParseOverrides(b); err == nil {
			o.m, o.ok, o.last = m, true, ""
			return o.m, true
		}
	}
	if msg := err.Error(); msg != o.last {
		o.last = msg
		if errors.Is(err, fs.ErrNotExist) {
			slog.Error("repos: overrides file missing, keeping what is applied", "path", o.Path, "haveCopy", o.ok)
		} else {
			slog.Error("repos: overrides file unreadable, keeping the last good copy", "path", o.Path, "haveCopy", o.ok, "error", err)
		}
	}
	return o.m, o.ok
}

// maxGonePerTick is the shrink guard: a token or GitHub problem looks like
// many repos vanishing at once, a real removal like one or two.
const maxGonePerTick = 2

// bigGrowth: more new repos than this in one tick is logged loudly. Not a
// guard: both installations are repository_selection=all by the user's
// choice (docs/adr/0075 §4), so the first tick after the cutover adds ~150.
const bigGrowth = 10

// fleetName bounds a name the sync may write: lowercase like definition repo
// names, and at most 63 characters because it becomes a pod label.
var fleetName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

type row struct {
	name     string
	url      string
	githubID *int64
	gone     bool
}

// normURL is how a pre-0074 row is matched to its GitHub repo on the first
// sync (docs/adr/0074 §8): case, a trailing slash and ".git" do not matter.
func normURL(u string) string {
	return strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(u), "/"), ".git")
}

// Sync makes repos match gh, a COMPLETE listing, plus ov. One transaction:
// upsert every listed repo, mark the unlisted ones gone (unless more than
// maxGonePerTick would be), and delete rows gone for 7 days that no open
// session references. Returns how many rows it changed.
func (s *Store) Sync(ctx context.Context, gh []GitHubRepo, ov map[string]Override) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("repos sync: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rs, err := tx.Query(ctx, `SELECT name, url, github_id, gone_since IS NOT NULL FROM repos FOR UPDATE`)
	if err != nil {
		return 0, fmt.Errorf("repos sync: %w", err)
	}
	var rows []row
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.name, &r.url, &r.githubID, &r.gone); err != nil {
			rs.Close()
			return 0, fmt.Errorf("repos sync: %w", err)
		}
		rows = append(rows, r)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return 0, fmt.Errorf("repos sync: %w", err)
	}
	byID, byURL, byName := map[int64]*row{}, map[string]*row{}, map[string]*row{}
	for i := range rows {
		r := &rows[i]
		byName[r.name] = r
		if r.githubID != nil {
			byID[*r.githubID] = r
		}
		byURL[normURL(r.url)] = r
	}

	// Oldest repo first: on a short-name collision the older one keeps the
	// name, the same answer every tick.
	gh = slices.Clone(gh)
	lower := make(map[string]Override, len(ov))
	for k, v := range ov {
		lower[strings.ToLower(k)] = v
	}
	ov = lower
	sort.Slice(gh, func(i, j int) bool { return gh[i].ID < gh[j].ID })
	listed := map[int64]bool{}
	var added []string // new rows this tick
	for _, g := range gh {
		listed[g.ID] = true
	}
	changed := 0
	seen := map[*row]bool{}
	seenNames, granted := []string{}, []string{}
	claimed := map[string]string{} // fleet name → full name, this tick
	usedOverride := map[string]bool{}
	for _, g := range gh {
		key := strings.ToLower(g.FullName)
		o, has := ov[key]
		usedOverride[key] = has
		if has && o.ID != g.ID {
			slog.Error("repos: override id does not match the repo, ignored", "repo", g.FullName, "overrideId", o.ID, "githubId", g.ID)
			has = false
		}
		cur := byID[g.ID]
		if cur == nil {
			// An unadopted (pre-0074) row, or a row whose repo was deleted and
			// re-created at the same URL (its old id is no longer listed).
			if r := byURL[normURL(g.CloneURL)]; r != nil && (r.githubID == nil || !listed[*r.githubID]) {
				cur = r
			}
		}
		if cur != nil {
			// Matched even when skipped below: a collision is not a removal.
			seen[cur] = true
			seenNames = append(seenNames, cur.name)
		}
		// An existing row keeps its name: sessions and definitions reference it
		// with no FK, so only an explicit override renames. A new row takes the
		// GitHub name, lowercased like every fleet name.
		name := strings.ToLower(g.Name)
		if cur != nil {
			name = cur.name
		}
		base := g.DefaultBranch
		var image string
		var cluster bool
		if has {
			if o.Name != "" {
				name = o.Name
			}
			if o.BaseBranch != "" {
				base = o.BaseBranch
			}
			image, cluster = o.Image, o.ClusterAccess
		}
		if (cur == nil || name != cur.name) && !fleetName.MatchString(name) {
			slog.Error("repos: not a valid fleet name, add a name override", "repo", g.FullName, "name", name)
			continue
		}
		if other, ok := claimed[name]; ok {
			slog.Error("repos: fleet name collision, add a name override", "name", name, "repo", g.FullName, "holder", other)
			continue
		}
		if holder := byName[name]; holder != nil && holder != cur {
			slog.Error("repos: fleet name held by another row, add a name override", "name", name, "repo", g.FullName, "holderUrl", holder.url)
			continue
		}
		claimed[name] = g.FullName
		// Keep the stored URL when it is the same remote: the provisioner's
		// clone cache compares remote.origin.url byte for byte.
		url := g.CloneURL
		if cur != nil && normURL(cur.url) == normURL(url) {
			url = cur.url
		}
		var tag int64
		if cur != nil {
			t, err := tx.Exec(ctx, `
				UPDATE repos SET name = $2, url = $3, github_id = $4, full_name = $5, base_branch = $6,
				       image = $7, cluster_access = $8, gone_since = NULL, updated_at = now()
				WHERE name = $1
				  AND (name, url, github_id, full_name, base_branch, image, cluster_access, gone_since)
				      IS DISTINCT FROM ($2, $3, $4, $5, $6, $7, $8, NULL)`,
				cur.name, name, url, g.ID, g.FullName, base, image, cluster)
			if err != nil {
				return 0, fmt.Errorf("repos sync: update %s: %w", g.FullName, err)
			}
			tag = t.RowsAffected()
		} else {
			t, err := tx.Exec(ctx, `
				INSERT INTO repos (name, url, github_id, full_name, base_branch, image, cluster_access)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				name, url, g.ID, g.FullName, base, image, cluster)
			if err != nil {
				return 0, fmt.Errorf("repos sync: insert %s: %w", g.FullName, err)
			}
			tag = t.RowsAffected()
			added = append(added, name)
		}
		if cluster {
			granted = append(granted, name)
		}
		if tag > 0 {
			changed++
			slog.Info("repos: synced", "name", name, "repo", g.FullName, "baseBranch", base, "image", image, "clusterAccess", cluster)
		}
	}
	if len(added) > bigGrowth {
		slog.Warn("repos: many new repos in one tick; check the App's installations if unexpected", "count", len(added), "repos", added)
	}
	for k := range ov {
		if !usedOverride[k] {
			slog.Warn("repos: override matches no repo the bot reaches", "repo", k)
		}
	}

	// cluster_access only from a matching override applied THIS tick: a row
	// left unmatched (unadoptable URL, collision, shrink guard) never keeps a
	// grant from the console era.
	t, err := tx.Exec(ctx, `UPDATE repos SET cluster_access = false, updated_at = now() WHERE cluster_access AND NOT (name = ANY($1))`, granted)
	if err != nil {
		return 0, fmt.Errorf("repos sync: revoke: %w", err)
	}
	if n := t.RowsAffected(); n > 0 {
		changed += int(n)
		slog.Warn("repos: cluster access revoked, no matching override", "count", n)
	}

	var gone []string
	active := 0
	for i := range rows {
		if r := &rows[i]; !r.gone {
			active++
			if !seen[r] {
				gone = append(gone, r.name)
			}
		}
	}
	switch {
	// A token that sees nothing (wrong account, lost scope) answers 200 with
	// [], which on a small fleet would otherwise stay under the per-tick cap.
	case len(gone) > maxGonePerTick || (len(gone) > 0 && len(gone) == active):
		slog.Error("repos: shrink guard, too many repos vanished in one tick; nothing marked gone. Check the bot's token and GitHub", "gone", gone, "active", active)
	case len(gone) > 0:
		if _, err := tx.Exec(ctx, `UPDATE repos SET gone_since = now(), updated_at = now() WHERE name = ANY($1)`, gone); err != nil {
			return 0, fmt.Errorf("repos sync: mark gone: %w", err)
		}
		changed += len(gone)
		slog.Warn("repos: no longer listed for the bot, hidden; deleted after 7 days", "repos", gone)
	}

	// Never a row a listed repo matched this tick, even one skipped on a
	// collision and so still marked gone.
	t, err = tx.Exec(ctx, `
		DELETE FROM repos r
		WHERE r.gone_since < now() - interval '7 days'
		  AND NOT (r.name = ANY($1))
		  AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.repo = r.name AND s.archived_at IS NULL)`, seenNames)
	if err != nil {
		return 0, fmt.Errorf("repos sync: delete gone: %w", err)
	}
	if n := t.RowsAffected(); n > 0 {
		changed += int(n)
		slog.Info("repos: deleted repos gone for 7 days", "count", n)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("repos sync: %w", err)
	}
	return changed, nil
}
