package buildguard

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWorkflowsNeverWriteWithGithubToken: no workflow asks for a writable
// GITHUB_TOKEN, and each one is read-only by default.
//
// main is behind a ruleset whose bypass list holds the owner and the release
// App — not GitHub Actions. Were GITHUB_TOKEN ever made a bypass actor (it is
// the obvious fix the day a release push is rejected), any job holding
// `contents: write` could write main, and most jobs here run PR code: tests,
// bun install scripts, buf plugins, in a repo whose PRs are often an agent's.
// The pushes that must happen (release-it, deploy's image pins) mint a token
// from the release App inside the main-only `release` environment instead.
//
// A job that truly needs another write scope (id-token for OIDC is fine) can
// still ask for it; contents and pull-requests are the two that move code.
func TestWorkflowsNeverWriteWithGithubToken(t *testing.T) {
	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	files, err := filepath.Glob(filepath.Join(dir, "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows under %s (err %v)", dir, err)
	}
	// `permission-contents: write` is the release App token's input, not a
	// GITHUB_TOKEN grant: the leading \s+ plus the bare key keeps it out.
	writeGrant := regexp.MustCompile(`(?m)^\s+(contents|pull-requests):\s*write\b`)
	topLevelRead := regexp.MustCompile(`(?m)^permissions:\s*\n\s+contents:\s*read\s*$`)
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		name := filepath.Base(f)
		for _, m := range writeGrant.FindAllString(string(body), -1) {
			t.Errorf("%s grants GITHUB_TOKEN %q — push with the release App's token instead", name, strings.TrimSpace(m))
		}
		if !topLevelRead.Match(body) {
			t.Errorf("%s has no top-level `permissions: contents: read` — a job without its own block would inherit the repo default", name)
		}
	}
}
