// Package buildguard holds no code — only guards against a class of bug
// that has now bitten this repo twice, and that CI cannot catch on its
// own because the failure is *silence*.
//
// Twice on 2026-08-11 a new component was added and wired into only part
// of the CI pipeline:
//
//   - `thot/` gained a ts_proto codegen target in buf.gen.yaml, but no
//     matching `bun install` step, so `buf generate` failed with "no such
//     file or directory" — invisible locally, where node_modules already
//     existed.
//   - `executor/` was added to the pull_request build matrix but not the
//     hardcoded release-tag list, so it built on every PR (where
//     push: false) and was NEVER published. The deployed manifest pointed
//     at an image tag that did not exist, and nothing failed loudly —
//     the pod simply couldn't start.
//
// Both share a shape: the component works everywhere you look, and is
// missing exactly where nobody looks. A test is the only thing that
// notices.
package buildguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Both tests read files OUTSIDE this module (.github/, proto/), which Go's
// test cache cannot see. A cached PASS therefore survives edits to the
// very files being guarded — verified live: removing `executor` from
// docker.yml still reported PASS until `-count=1` forced a real run.
//
// A guard that can be silently cached is worth nothing, so both tests
// touch the files through a helper that fails loudly if they are missing,
// and CI must run these with -count=1 (see go.yml). If you run them by
// hand, do the same.

// repoRoot walks up from this package to the module root's parent — the
// agent-fleet checkout itself.
func repoRoot(t *testing.T) string {
	t.Helper()
	// core/internal/buildguard -> core/internal -> core -> repo root
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

// TestEveryDockerfileComponentIsWiredIntoCI asserts that every top-level
// directory shipping a Dockerfile is mentioned somewhere in docker.yml.
//
// Deliberately a "mentioned anywhere" check rather than a precise matrix
// parse: the workflow has several shapes (a bun matrix, a hardcoded
// release list, per-component jobs for migration/e2e-runner), and a
// strict parser would be brittle enough to get deleted the first time it
// false-positived. The failure this catches — a component wired into
// nothing at all — is caught fine by the loose version.
func TestEveryDockerfileComponentIsWiredIntoCI(t *testing.T) {
	root := repoRoot(t)

	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "docker.yml"))
	if err != nil {
		t.Fatalf("read docker.yml: %v", err)
	}
	got := string(workflow)

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read repo root: %v", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		component := e.Name()
		if _, err := os.Stat(filepath.Join(root, component, "Dockerfile")); err != nil {
			continue
		}
		if !strings.Contains(got, component) {
			t.Errorf(
				"%s/ ships a Dockerfile but is never mentioned in .github/workflows/docker.yml — "+
					"its image will not be built or pushed, and a manifest referencing it will fail to pull",
				component)
		}
	}
}

// TestWorkerImageHasProcps asserts the worker image installs procps.
//
// Same shape as the two above — missing exactly where nobody looks. The
// bundled Claude Code CLI kills subprocess trees with tree-kill, which on
// Linux shells out to `ps --ppid <pid>`; oven/bun:1-slim has no `ps`, and
// Bun's spawn throws ENOENT synchronously instead of emitting the 'error'
// event tree-kill expects. The CLI process just dies, and the only symptom
// the fleet sees is "Claude Code process exited with code 1".
func TestWorkerImageHasProcps(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join(repoRoot(t), "worker", "Dockerfile"))
	if err != nil {
		t.Fatalf("read worker/Dockerfile: %v", err)
	}
	if !strings.Contains(string(dockerfile), "procps") {
		t.Error("worker/Dockerfile no longer installs procps — the Claude Code CLI needs `ps` " +
			"to kill subprocess trees, and without it the agent crashes mid-session")
	}
}

// TestEveryCodegenTargetHasAnInstallStep asserts that every local plugin
// buf.gen.yaml resolves out of a component's node_modules has a matching
// dependency install in the workflow that runs `buf generate`.
//
// This is the exact miss that broke the buf job: a plugin path pointing
// into a directory CI never installed. It passes locally forever, because
// the directory is already populated there.
func TestEveryCodegenTargetHasAnInstallStep(t *testing.T) {
	root := repoRoot(t)

	gen, err := os.ReadFile(filepath.Join(root, "proto", "buf.gen.yaml"))
	if err != nil {
		t.Fatalf("read buf.gen.yaml: %v", err)
	}
	goWorkflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "go.yml"))
	if err != nil {
		t.Fatalf("read go.yml: %v", err)
	}
	workflow := string(goWorkflow)

	// Lines look like: `- local: ../worker/node_modules/.bin/protoc-gen-ts_proto`
	for _, line := range strings.Split(string(gen), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- local: ../") {
			continue
		}
		rest := strings.TrimPrefix(line, "- local: ../")
		component, _, ok := strings.Cut(rest, "/")
		if !ok || component == "" {
			continue
		}
		// The workflow must install that component's deps before running
		// `buf generate`, or the plugin binary won't exist.
		if !strings.Contains(workflow, "working-directory: "+component) {
			t.Errorf(
				"buf.gen.yaml resolves a plugin from %s/node_modules, but go.yml has no "+
					"`working-directory: %s` install step — `buf generate` will fail in CI with "+
					"\"no such file or directory\" while passing locally",
				component, component)
		}
	}
}
