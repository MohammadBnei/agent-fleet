package buildguard

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The docs drift these two guard was found on 2026-10-22, all of it silent:
// an ADR index that called 0075 Proposed after its file said Accepted, five
// files still Accepted that the index had marked Superseded, and skills that
// told agents to edit worker/src/planning.ts and core/internal/dispatch/loop.go
// long after both were deleted. A reader trusts whichever copy they open
// first. CI runs these on every PR (go.yml's docs-check job, no path filter):
// a code-only rename breaks a skill as easily as a docs edit does.

var adrStatusKeyword = regexp.MustCompile(`\b(Accepted|Proposed|Superseded|Rejected)\b`)

// statusKeyword is the first lifecycle word in s: "Accepted (rollout
// complete)" and "Accepted — amended by 0059" are both Accepted. The free
// text after it is history for humans; only the keyword has to agree.
func statusKeyword(s string) string {
	return adrStatusKeyword.FindString(s)
}

// TestDocsADRIndexMatchesFiles: every ADR file has exactly one index row,
// every row names a real file, and the two agree on the status keyword.
func TestDocsADRIndexMatchesFiles(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "docs", "adr")
	index, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("read ADR index: %v", err)
	}

	row := regexp.MustCompile(`(?m)^\|\s*\[(\d{4})\]\(([^)]+)\)\s*\|.*\|\s*([^|]*?)\s*\|\s*$`)
	indexed := map[string]string{} // file -> status cell
	for _, m := range row.FindAllStringSubmatch(string(index), -1) {
		file := m[2]
		if !strings.HasPrefix(file, m[1]) {
			t.Errorf("index row %s links %s", m[1], file)
		}
		if _, dup := indexed[file]; dup {
			t.Errorf("%s has more than one index row", file)
		}
		indexed[file] = m[3]
	}

	files, err := filepath.Glob(filepath.Join(dir, "[0-9][0-9][0-9][0-9]-*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no ADR files under %s (err %v)", dir, err)
	}
	statusLine := regexp.MustCompile(`(?m)^\s*(?:-\s*)?\*\*Status:?\*\*:?\s*(.*)$`)
	seen := map[string]bool{}
	for _, path := range files {
		name := filepath.Base(path)
		seen[name] = true
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		m := statusLine.FindSubmatch(body)
		if m == nil {
			t.Errorf("%s has no **Status:** line", name)
			continue
		}
		fileStatus := statusKeyword(string(m[1]))
		if fileStatus == "" {
			t.Errorf("%s: status line %q names none of Accepted/Proposed/Superseded/Rejected", name, m[1])
			continue
		}
		cell, ok := indexed[name]
		if !ok {
			t.Errorf("%s has no row in docs/adr/README.md", name)
			continue
		}
		if got := statusKeyword(cell); got != fileStatus {
			t.Errorf("%s: index says %q, file says %q", name, got, fileStatus)
		}
	}
	for file := range indexed {
		if !seen[file] {
			t.Errorf("docs/adr/README.md links %s, which does not exist", file)
		}
	}
}

// TestDocsSkillPathsExist: a repo path a skill names in backticks exists.
// Skills are instructions an agent follows literally; a dead path sends it
// to rebuild something deliberately deleted.
//
// Narrow on purpose, so it never cries wolf: only backticked tokens that
// look like a file path (a slash and a known extension), resolved against
// the repo root, a component directory, or the skill's own directory.
// Absolute paths, home paths, globs, placeholders and infra-bootstrap paths
// are someone else's filesystem and are skipped.
func TestDocsSkillPathsExist(t *testing.T) {
	root := repoRoot(t)
	skills, err := filepath.Glob(filepath.Join(root, ".claude", "skills", "*", "*.md"))
	if err != nil || len(skills) == 0 {
		t.Fatalf("no skills under .claude/skills (err %v)", err)
	}
	components := []string{"", "core", "provisioner", "sidecar", "worker", "dashboard", "executor", "islands"}
	pathLike := regexp.MustCompile("`([A-Za-z0-9_.\\-/]+/[A-Za-z0-9_.\\-]+\\.(?:go|ts|tsx|md|ya?ml|sql|json|proto|sh|mjs))(?::\\d+(?:-\\d+)?)?`")

	var missing []string
	for _, skill := range skills {
		body, err := os.ReadFile(skill)
		if err != nil {
			t.Fatalf("read %s: %v", skill, err)
		}
		for _, m := range pathLike.FindAllStringSubmatch(string(body), -1) {
			p := m[1]
			if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") ||
				strings.HasPrefix(p, "infra-bootstrap") || strings.HasPrefix(p, "gitops/") {
				continue
			}
			p = strings.TrimPrefix(p, "agent-fleet/")
			found := false
			for _, base := range append(components, filepath.Dir(skill)) {
				// The skill's own directory is absolute; components are
				// relative to the repo root ("" is the root itself).
				candidate := filepath.Join(root, base, p)
				if filepath.IsAbs(base) {
					candidate = filepath.Join(base, p)
				}
				if _, err := os.Stat(candidate); err == nil {
					found = true
					break
				}
			}
			if !found {
				rel, _ := filepath.Rel(root, skill)
				missing = append(missing, rel+": "+m[1])
			}
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("skill cites a path that does not exist: %s", m)
	}
}
