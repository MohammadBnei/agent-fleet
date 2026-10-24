package buildguard

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// releaseJobs may hold a writable GITHUB_TOKEN: release-it's version commit
// and tag, and deploy's image pins. Each runs on a push to main only.
var releaseJobs = map[string][]string{
	"docker.yml": {"release", "deploy"},
}

// pushOnly is the conjunct a release job's `if:` must carry, ANDed with the
// rest: an `||` or a negation could admit another event around it.
const pushOnly = "github.event_name == 'push'"

// negation is a `!` that is not part of `!=` (`needs.x.result != 'failure'`
// is an ordinary conjunct).
var negation = regexp.MustCompile(`!([^=]|$)`)

type workflow struct {
	// `on:` may be a string, a list or a map; only the map form names
	// branches, so it is decoded loosely and read by pushBranches.
	On          any            `yaml:"on"`
	Permissions any            `yaml:"permissions"`
	Jobs        map[string]job `yaml:"jobs"`
}

type job struct {
	If          string `yaml:"if"`
	Permissions any    `yaml:"permissions"`
}

// pushBranches is on.push.branches; nil when push is not filtered to
// branches (including `on: push`, which matches every branch).
func (w workflow) pushBranches() []string {
	on, _ := w.On.(map[string]any)
	push, _ := on["push"].(map[string]any)
	var out []string
	for _, b := range asList(push["branches"]) {
		if s, ok := b.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func asList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

// writes reports whether a permissions block grants GITHUB_TOKEN any write
// scope. id-token: write (OIDC to Infisical) is the one exception: it mints
// a credential for something else, not a token that acts on this repo.
func writes(p any) bool {
	switch v := p.(type) {
	case string:
		return v == "write-all"
	case map[string]any:
		for k, val := range v {
			if k != "id-token" && val == "write" {
				return true
			}
		}
	}
	return false
}

// pushGated reports whether `if:` requires a push: it has pushOnly as one of
// its top-level && conjuncts, and no || or negation anywhere.
func pushGated(cond string) bool {
	if strings.Contains(cond, "||") || negation.MatchString(cond) {
		return false
	}
	for _, c := range strings.Split(cond, "&&") {
		if strings.Join(strings.Fields(c), " ") == pushOnly {
			return true
		}
	}
	return false
}

// TestWorkflowsWriteOnlyInReleaseJobs: every workflow is read-only by
// default, and only the named release jobs raise GITHUB_TOKEN to write —
// jobs that run on a push to main, never on PR code.
//
// Most jobs here run PR code: tests, bun install scripts, buf plugins, in a
// repo whose PRs are often an agent's. A writable token there can push to any
// branch, main included while main has no PR rule, or (actions: write)
// dispatch workflows on main. When a ruleset starts requiring PRs, these
// release jobs move to a dedicated App token (VISION "The process is the
// quality gate"); until then GITHUB_TOKEN is the path that runs.
func TestWorkflowsWriteOnlyInReleaseJobs(t *testing.T) {
	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	var files []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil {
			t.Fatalf("glob %s: %v", pat, err)
		}
		files = append(files, m...)
	}
	if len(files) == 0 {
		t.Fatalf("no workflows under %s", dir)
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		name := filepath.Base(f)
		var w workflow
		if err := yaml.Unmarshal(body, &w); err != nil {
			t.Errorf("parse %s: %v", name, err)
			continue
		}
		top, _ := w.Permissions.(map[string]any)
		if top["contents"] != "read" || writes(w.Permissions) {
			t.Errorf("%s: top-level permissions must be `contents: read` with no write scope — a job without its own block inherits it", name)
		}
		for id, j := range w.Jobs {
			if !writes(j.Permissions) {
				continue
			}
			if !slices.Contains(releaseJobs[name], id) {
				t.Errorf("%s: job %q grants GITHUB_TOKEN a write scope; only %v may", name, id, releaseJobs[name])
				continue
			}
			if !pushGated(j.If) {
				t.Errorf("%s: release job %q writes, so its `if:` must AND %q with no || or negation, got %q", name, id, pushOnly, j.If)
			}
			if b := w.pushBranches(); !slices.Equal(b, []string{"main"}) {
				t.Errorf("%s: release job %q writes, so the push trigger must be main only, got %v", name, id, b)
			}
		}
	}
}

// The guard's own edge cases, so a loosening of it shows up here first.
func TestWorkflowGuardHelpers(t *testing.T) {
	for cond, want := range map[string]bool{
		"always() && github.event_name == 'push'":                                  true,
		"always()\n&& github.event_name == 'push'\n&& needs.x.result == 'success'": true,
		"github.event_name == 'push' || github.event_name == 'pull_request'":       false,
		"!(github.event_name != 'push')":                                           false,
		"github.event_name != 'pull_request'":                                      false,
		"always() && github.event_name == 'push' && needs.t.result != 'failure'":   true,
		"": false,
	} {
		if got := pushGated(cond); got != want {
			t.Errorf("pushGated(%q) = %v, want %v", cond, got, want)
		}
	}
	for perms, want := range map[string]bool{
		"contents: read":                    false,
		"{contents: read, id-token: write}": false,
		"{contents: read, actions: write}":  true,
		"write-all":                         true,
		"{pull-requests: write}":            true,
	} {
		var p any
		if err := yaml.Unmarshal([]byte(perms), &p); err != nil {
			t.Fatal(err)
		}
		if got := writes(p); got != want {
			t.Errorf("writes(%s) = %v, want %v", perms, got, want)
		}
	}
	for on, want := range map[string][]string{
		"on: push":                            nil,
		"on: [push, pull_request]":            nil,
		"on: {push: {branches: [main]}}":      {"main"},
		"on: {push: {branches: [main, dev]}}": {"main", "dev"},
	} {
		var w workflow
		if err := yaml.Unmarshal([]byte(on), &w); err != nil {
			t.Fatalf("parse %q: %v", on, err)
		}
		if got := w.pushBranches(); !slices.Equal(got, want) {
			t.Errorf("pushBranches(%q) = %v, want %v", on, got, want)
		}
	}
}
