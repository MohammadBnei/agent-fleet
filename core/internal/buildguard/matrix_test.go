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

	"go.yaml.in/yaml/v3"
)

// Both tests read files OUTSIDE this module (.github/, proto/), which Go's
// test cache cannot see. A cached PASS therefore survives edits to the
// very files being guarded — verified live: removing `executor` from
// docker.yml still reported PASS until `-count=1` forced a real run.
//
// A guard that can be silently cached is worth nothing, so both tests
// touch the files through a helper that fails loudly if they are missing,
// and CI must run these with -count=1: .github/scripts/go-test.sh keeps this
// package in its uncached invocation, and TestGoTestScriptRunsBuildguardUncached
// holds it there. If you run them by hand, do the same.

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
// directory shipping a Dockerfile is built by a docker.yml job calling
// ukubi-ci's image.yml with `dockerfile: <dir>/Dockerfile`.
//
// It used to be a "mentioned anywhere" check, and that is how provisioner,
// worker and sidecar went unbuilt from #443 to the next fix: docker.yml still
// named all three, in a comment explaining why they were not built, so the
// guard passed while nothing pushed their images.
func TestEveryDockerfileComponentIsWiredIntoCI(t *testing.T) {
	root := repoRoot(t)
	jobs := dockerJobs(t)

	built := map[string]bool{}
	for _, j := range jobs {
		if strings.HasPrefix(j.Uses, ukubiCI+"image.yml@") {
			built[j.With["dockerfile"]] = true
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read repo root: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dockerfile := e.Name() + "/Dockerfile"
		if _, err := os.Stat(filepath.Join(root, dockerfile)); err != nil {
			continue
		}
		if !built[dockerfile] {
			t.Errorf(
				"%s ships but no .github/workflows/docker.yml job calls image.yml with `dockerfile: %s` — "+
					"its image will not be built or pushed, and a manifest referencing it will fail to pull",
				dockerfile, dockerfile)
		}
	}
}

// TestProvisionerPinsReachAManifest: a docker.yml job pinning `.images.<name>`
// feeds k8s/provisioner/templates/manifests.yaml, which swaps the pin into
// every `registry.bnei.lan:5000/agent-fleet-<name>:` ref. A values-key or image
// name that matches no ref would push and pin an image nothing deploys; the
// template fails on it too, but only once ArgoCD renders it.
func TestProvisionerPinsReachAManifest(t *testing.T) {
	root := repoRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "k8s", "provisioner", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no manifests under k8s/provisioner (%v)", err)
	}
	var manifests strings.Builder
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		manifests.Write(b)
	}
	n := 0
	for id, j := range dockerJobs(t) {
		name, ok := strings.CutPrefix(j.With["values-key"], ".images.")
		if !ok {
			continue
		}
		n++
		if j.With["image"] != "agent-fleet-"+name {
			t.Errorf("docker.yml job %q pins .images.%s but builds %q, want agent-fleet-%s", id, name, j.With["image"], name)
		}
		if ref := "registry.bnei.lan:5000/agent-fleet-" + name + ":"; !strings.Contains(manifests.String(), ref) {
			t.Errorf("docker.yml job %q pins .images.%s, but no k8s/provisioner manifest has %s", id, name, ref)
		}
	}
	if n != 3 {
		t.Errorf("docker.yml pins %d .images.* images, want 3 (provisioner, worker, sidecar)", n)
	}
}

type dockerJob struct {
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
}

func dockerJobs(t *testing.T) map[string]dockerJob {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "docker.yml"))
	if err != nil {
		t.Fatalf("read docker.yml: %v", err)
	}
	var w struct {
		Jobs map[string]dockerJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &w); err != nil {
		t.Fatalf("parse docker.yml: %v", err)
	}
	return w.Jobs
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
// guards go.yml against a missing *module*. This one stays a loose
// "mentioned anywhere" check: go.yml's matrix is computed in shell, so a
// strict parse would be brittle enough to get deleted. It can be fooled by a
// comment the way the docker.yml check was.
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

// TestGoTestScriptRunsBuildguardUncached: go.yml hands every module's tests to
// .github/scripts/go-test.sh, which lets Go's test cache serve most packages.
// This package reads files outside its module, so a cached PASS would survive
// edits to the files it guards. The script must keep it in the -count=1 run.
func TestGoTestScriptRunsBuildguardUncached(t *testing.T) {
	root := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(root, ".github", "scripts", "go-test.sh"))
	if err != nil {
		t.Fatalf("read go-test.sh: %v", err)
	}
	for _, want := range []string{"internal/buildguard", "-count=1"} {
		if !strings.Contains(string(script), want) {
			t.Errorf(".github/scripts/go-test.sh no longer mentions %q — buildguard would run from Go's test cache", want)
		}
	}
	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "go.yml"))
	if err != nil {
		t.Fatalf("read go.yml: %v", err)
	}
	if !strings.Contains(string(wf), ".github/scripts/go-test.sh") {
		t.Errorf("go.yml does not run .github/scripts/go-test.sh, so the module tests are not split into cached and -count=1 runs")
	}
}
