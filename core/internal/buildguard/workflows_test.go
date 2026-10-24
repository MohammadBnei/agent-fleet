package buildguard

import (
	"os"
	"path/filepath"
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

type workflow struct {
	On struct {
		Push struct {
			Branches []string `yaml:"branches"`
		} `yaml:"push"`
	} `yaml:"on"`
	Permissions any            `yaml:"permissions"`
	Jobs        map[string]job `yaml:"jobs"`
}

type job struct {
	If          string `yaml:"if"`
	Permissions any    `yaml:"permissions"`
}

// writes reports whether a permissions block lets GITHUB_TOKEN move code.
// id-token: write (OIDC) is fine; contents and pull-requests are the two
// that write to the repo.
func writes(p any) bool {
	switch v := p.(type) {
	case string:
		return v == "write-all"
	case map[string]any:
		for _, k := range []string{"contents", "pull-requests"} {
			if v[k] == "write" {
				return true
			}
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
// branch, main included while main has no PR rule. When a ruleset starts
// requiring PRs, these release jobs move to a dedicated App token (VISION
// "The process is the quality gate"); until then GITHUB_TOKEN is the path
// that runs.
func TestWorkflowsWriteOnlyInReleaseJobs(t *testing.T) {
	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	files, err := filepath.Glob(filepath.Join(dir, "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows under %s (err %v)", dir, err)
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		name := filepath.Base(f)
		var w workflow
		if err := yaml.Unmarshal(body, &w); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		top, _ := w.Permissions.(map[string]any)
		if top["contents"] != "read" || writes(w.Permissions) {
			t.Errorf("%s: top-level permissions must be `contents: read` — a job without its own block inherits it", name)
		}
		for id, j := range w.Jobs {
			if !writes(j.Permissions) {
				continue
			}
			if !slices.Contains(releaseJobs[name], id) {
				t.Errorf("%s: job %q grants GITHUB_TOKEN write; only %v may", name, id, releaseJobs[name])
				continue
			}
			if !strings.Contains(j.If, "github.event_name == 'push'") {
				t.Errorf("%s: release job %q writes but its `if:` does not require a push", name, id)
			}
			if !slices.Equal(w.On.Push.Branches, []string{"main"}) {
				t.Errorf("%s: release job %q writes, so the push trigger must be main only, got %v", name, id, w.On.Push.Branches)
			}
		}
	}
}
