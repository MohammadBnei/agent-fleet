package definitions

import (
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestRepoDefinitions is the CI check (docs/adr/0062): the same validator core
// runs at sync time, over this repo's own definitions/. A client error, a bad
// path or an invalid non-abstract definition fails the build, so a broken
// definition can never reach a release tag, which is what the provisioner
// reads.
func TestRepoDefinitions(t *testing.T) {
	results, fileErrs := Load(definitionTree(t, "definitions"))
	for _, e := range fileErrs {
		t.Errorf("file error: %v", e)
	}
	seen := map[string]bool{}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("definition %q is invalid: %v", r.Name, r.Err)
		}
		seen[r.Name] = !r.Abstract && r.Err == nil
		// workspace-base launches its children in auto (docs/adr/0076 §1);
		// none of them narrows mode.
		if seen[r.Name] && (r.Effective.Mode == nil || r.Effective.Mode.Default != "auto") {
			t.Errorf("%s: effective mode %+v, want default auto from workspace-base", r.Name, r.Effective.Mode)
		}
	}
	for _, want := range []string{"opus-workspace", "dev-dream-analyst", "dev-notes"} {
		if !seen[want] {
			t.Errorf("%s must be a valid, launchable definition", want)
		}
	}
}

// TestResourceCeilingIsTheLimitRange pins maxMilliCPU/maxMemoryMi to
// limitRange.max in k8s/core.yaml, the namespace ceiling a definition pod is
// admitted under. Above it the pod is rejected at admission and never
// created. Reads a file outside the module: run with -count=1.
func TestResourceCeilingIsTheLimitRange(t *testing.T) {
	b, err := os.ReadFile("../../../k8s/core.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		LimitRange struct {
			Max struct{ CPU, Memory string } `yaml:"max"`
		} `yaml:"limitRange"`
	}
	if err := yaml.Unmarshal(b, &values); err != nil {
		t.Fatal(err)
	}
	max := values.LimitRange.Max
	cpu, mem := cpuRe.FindStringSubmatch(max.CPU), memoryRe.FindStringSubmatch(max.Memory)
	if cpu == nil || mem == nil {
		t.Fatalf("limitRange.max %+v: want cores or millicores, and Mi or Gi", max)
	}
	milli, _ := strconv.Atoi(cpu[1])
	if cpu[2] == "" {
		milli *= 1000
	}
	mi, _ := strconv.Atoi(mem[1])
	if mem[2] == "Gi" {
		mi *= 1024
	}
	if milli != maxMilliCPU || mi != maxMemoryMi {
		t.Errorf("definition ceiling %dm/%dMi, limitRange.max %s/%s: move them together", maxMilliCPU, maxMemoryMi, max.CPU, max.Memory)
	}
}

// docs/adr/0080 §4: the local stack's game definitions, checked by the same
// loader, as /kind-local lays them out: in place of definitions/. Loaded on
// their own, since that is the whole set the local mirror holds.
func TestLocalKindDefinitions(t *testing.T) {
	results, fileErrs := Load(definitionTree(t, "local/kind/definitions"))
	for _, e := range fileErrs {
		t.Errorf("file error: %v", e)
	}
	launchable := false
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("definition %q is invalid: %v", r.Name, r.Err)
		}
		if r.Name == "game-ukubi-game" && !r.Abstract && r.Err == nil {
			launchable = r.Effective.Mode != nil && r.Effective.Mode.Default == "auto"
		}
	}
	if !launchable {
		t.Error("game-ukubi-game must be a valid, launchable definition in auto")
	}
}

// definitionTree reads dir under the repo root as the provisioner would list
// definitions/, keyed by its definitions/ path.
func definitionTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	root := os.DirFS("../../..")
	files := map[string][]byte{}
	total := 0
	err := fs.WalkDir(root, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// The provisioner's ReadDefinitions fails the WHOLE listing on a
		// non-regular file or an oversize one, so CI must refuse them too
		// (provisioner/internal/git/git.go maxDefinitionFile/Total).
		if !d.Type().IsRegular() {
			t.Errorf("%s is not a regular file: the provisioner refuses the whole listing", p)
			return nil
		}
		b, err := fs.ReadFile(root, p)
		if total += len(b); len(b) > 64<<10 || total > 1<<20 {
			t.Errorf("%s: over the provisioner's 64 KiB per-file or 1 MiB total limit", p)
		}
		files[path.Join("definitions", strings.TrimPrefix(path.Clean(p), dir))] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
