package definitions

import (
	"io/fs"
	"os"
	"path"
	"testing"
)

// TestRepoDefinitions is the CI check (docs/adr/0062): the same validator core
// runs at sync time, over this repo's own definitions/. A client error, a bad
// path or an invalid non-abstract definition fails the build, so a broken
// definition can never reach a release tag, which is what the provisioner
// reads.
func TestRepoDefinitions(t *testing.T) {
	root := os.DirFS("../../..")
	files := map[string][]byte{}
	total := 0
	err := fs.WalkDir(root, "definitions", func(p string, d fs.DirEntry, err error) error {
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
		files[path.Clean(p)] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	results, fileErrs := Load(files)
	for _, e := range fileErrs {
		t.Errorf("file error: %v", e)
	}
	seen := map[string]bool{}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("definition %q is invalid: %v", r.Name, r.Err)
		}
		seen[r.Name] = !r.Abstract && r.Err == nil
	}
	for _, want := range []string{"opus-workspace", "dev-dream-analyst"} {
		if !seen[want] {
			t.Errorf("%s must be a valid, launchable definition", want)
		}
	}
}
