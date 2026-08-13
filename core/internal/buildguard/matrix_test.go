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

// TestEveryGoWorkModuleIsInTheGoWorkflow asserts that every module listed
// in go.work is mentioned in go.yml.
//
// Third instance of the same shape, found 2026-08-13: `e2e-runner/` was a
// go.work module — building locally, part of `go build ./...` — but had no
// paths-filter entry and no matrix entry in go.yml, so its Go code was
// never vetted, linted or tested by CI. That code is execmcp, the worker's
// entire build/test sandbox. A test added to it would have reported nothing
// forever.
//
// The sibling test above guards docker.yml against a missing *image*; this
// guards go.yml against a missing *module*. Same loose "mentioned anywhere"
// approach, same reason: a strict matrix parse would be brittle enough to
// get deleted, and the failure worth catching is absence, not arrangement.
func TestEveryGoWorkModuleIsInTheGoWorkflow(t *testing.T) {
	root := repoRoot(t)

	goWork, err := os.ReadFile(filepath.Join(root, "go.work"))
	if err != nil {
		t.Fatalf("read go.work: %v", err)
	}
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "go.yml"))
	if err != nil {
		t.Fatalf("read go.yml: %v", err)
	}
	got := string(workflow)

	for _, line := range strings.Split(string(goWork), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "./") {
			continue
		}
		// "./proto/gen/go" is generated code compiled as part of its
		// dependents, not a module with its own CI job.
		module := strings.TrimPrefix(line, "./")
		if strings.Contains(module, "/") {
			continue
		}
		if !strings.Contains(got, module) {
			t.Errorf(
				"go.work lists %q but .github/workflows/go.yml never mentions it — "+
					"its code is never vetted, linted or tested in CI, and any test in it silently never runs",
				module)
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

// TestComponentsBuiltIntoAnotherImageAreWiredIntoCI covers the blind spot in
// TestEveryDockerfileComponentIsWiredIntoCI above: that one only inspects
// directories that ship a Dockerfile of their own. A component compiled
// *into* someone else's image has none, so it is invisible there — and was.
//
// dashboard/ builds into core's binary (core/Dockerfile's spa stage COPYs
// dashboard/ and runs `bun run build`, then the Go stage COPYs the dist into
// core/internal/webui/dist). Until this was fixed, docker.yml's `core` path
// filter matched only 'core/**', so a dashboard-only PR built no image, ran
// no test, and reported green having compiled nothing at all.
func TestComponentsBuiltIntoAnotherImageAreWiredIntoCI(t *testing.T) {
	root := repoRoot(t)

	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "docker.yml"))
	if err != nil {
		t.Fatalf("read docker.yml: %v", err)
	}
	got := string(workflow)

	// Each entry: a source directory with no Dockerfile of its own, and the
	// image whose Dockerfile consumes it.
	embedded := map[string]string{"dashboard": "core"}

	for component, host := range embedded {
		dockerfile, err := os.ReadFile(filepath.Join(root, host, "Dockerfile"))
		if err != nil {
			t.Fatalf("read %s/Dockerfile: %v", host, err)
		}
		if !strings.Contains(string(dockerfile), component+"/") {
			t.Errorf(
				"%s/Dockerfile no longer references %s/ — this guard is now describing a coupling that doesn't exist; "+
					"delete the entry rather than leaving a test that asserts nothing",
				host, component)
			continue
		}
		if !strings.Contains(got, "'"+component+"/**'") {
			t.Errorf(
				"%s/ is compiled into %s's image but no path filter in .github/workflows/docker.yml matches '%s/**' — "+
					"a %s-only change will build nothing and pass CI having compiled nothing",
				component, host, component, component)
		}
	}
}
