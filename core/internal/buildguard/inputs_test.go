package buildguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestImageInputsCoverEveryCopy guards the skip in docker.yml's build-push.
//
// An image is tagged by a hash of the paths .github/image-inputs.txt lists
// for it, and not rebuilt while that hash matches the deployed tag. If a
// Dockerfile COPYs a path the list omits, a change to that path leaves the
// hash alone: CI goes green, the release deploys, and the change is in no
// image. This fails on any COPY/ADD source (other than --from a build stage)
// that no listed input covers.
//
// Reads files outside this module: run with -count=1, like the rest of this
// package.
func TestImageInputsCoverEveryCopy(t *testing.T) {
	root := repoRoot(t)
	inputs := readImageInputs(t, root)

	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "docker.yml"))
	if err != nil {
		t.Fatalf("read docker.yml: %v", err)
	}
	if !strings.Contains(string(workflow), ".github/image-inputs.txt") {
		t.Error("docker.yml no longer reads .github/image-inputs.txt — this guard checks a list nothing uses")
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read repo root: %v", err)
	}
	checked := 0
	for _, e := range entries {
		dockerfile := filepath.Join(root, e.Name(), "Dockerfile")
		if !e.IsDir() {
			continue
		}
		src, err := os.ReadFile(dockerfile)
		if err != nil {
			continue
		}
		component := e.Name()
		paths, ok := inputs[component]
		if !ok {
			t.Errorf("%s/ ships a Dockerfile but has no line in .github/image-inputs.txt — docker.yml cannot hash it", component)
			continue
		}
		if !covered(component+"/Dockerfile", paths) {
			t.Errorf("%s: inputs %v do not cover its own Dockerfile", component, paths)
		}
		for _, p := range paths {
			if _, err := os.Stat(filepath.Join(root, p)); err != nil {
				t.Errorf("%s: input %q does not exist — `git rev-parse HEAD:%s` fails the build", component, p, p)
			}
		}
		for _, s := range copySources(string(src)) {
			checked++
			if !covered(s, paths) {
				t.Errorf("%s/Dockerfile copies %q, which .github/image-inputs.txt does not list for %s: "+
					"a change there would not change the image's hash, and docker.yml would skip the build", component, s, component)
			}
		}
	}
	if checked < 10 {
		t.Fatalf("only found %d COPY/ADD sources — the parse broke and this guard is guarding nothing", checked)
	}
}

func readImageInputs(t *testing.T, root string) map[string][]string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, ".github", "image-inputs.txt"))
	if err != nil {
		t.Fatalf("read image-inputs.txt: %v", err)
	}
	out := map[string][]string{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("image-inputs.txt: malformed line %q", line)
		}
		out[strings.TrimSpace(name)] = strings.Fields(rest)
	}
	return out
}

// copySources returns the context paths a Dockerfile's COPY/ADD lines read.
// One instruction per line, which is how every Dockerfile here is written; a
// JSON-form or continued COPY yields junk sources that fail as uncovered,
// never a silent pass.
func copySources(dockerfile string) []string {
	var out []string
	for _, line := range strings.Split(dockerfile, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || (!strings.EqualFold(f[0], "COPY") && !strings.EqualFold(f[0], "ADD")) {
			continue
		}
		args, fromStage := f[1:], false
		for len(args) > 0 && strings.HasPrefix(args[0], "--") {
			fromStage = fromStage || strings.HasPrefix(args[0], "--from")
			args = args[1:]
		}
		if fromStage || len(args) < 2 {
			continue
		}
		out = append(out, args[:len(args)-1]...)
	}
	return out
}

func covered(src string, inputs []string) bool {
	src = strings.TrimSuffix(strings.TrimPrefix(filepath.Clean(src), "./"), "/")
	for _, in := range inputs {
		if src == in || strings.HasPrefix(src, in+"/") {
			return true
		}
	}
	return false
}
