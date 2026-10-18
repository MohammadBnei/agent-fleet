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
	err := fs.WalkDir(root, "definitions", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(root, p)
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
