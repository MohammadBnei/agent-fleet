package k8s

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runClone runs the clone script for base against a cache of a real origin,
// with /workspace and /repo-cache pointed into dir.
func runClone(t *testing.T, dir, base string) error {
	t.Helper()
	script := strings.NewReplacer(sessionWorkdir, filepath.Join(dir, "ws"), "/repo-cache/", filepath.Join(dir, "cache")+"/").
		Replace(cloneTail(WorkerPodSpec{Repo: "r", RepoURL: "https://github.com/o/r.git"}, base))
	cmd := exec.Command("sh", "-c", "set -eu\n"+script)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	return err
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// cacheDir builds dir/cache/r the way the provisioner's cache looks: a clone
// of an origin with a "main" branch holding README.md.
func cacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin")
	git(t, dir, "init", "-b", "main", origin)
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "add", ".")
	git(t, origin, "commit", "-m", "init")
	git(t, dir, "clone", origin, filepath.Join(dir, "cache", "r"))
	return dir
}

func TestCloneScript_FreshCloneChecksOutTheRealRemote(t *testing.T) {
	dir := cacheDir(t)
	if err := runClone(t, dir, "main"); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(dir, "ws")
	if _, err := os.Stat(filepath.Join(ws, "README.md")); err != nil {
		t.Errorf("tree not checked out: %v", err)
	}
	if u := git(t, ws, "remote", "get-url", "origin"); u != "https://github.com/o/r.git" {
		t.Errorf("origin %q", u)
	}
}

// A tree from before the marker (an old repo clone: checked out, no marker)
// is kept, uncommitted work and all.
func TestCloneScript_KeepsAnOldRepoTree(t *testing.T) {
	dir := cacheDir(t)
	ws := filepath.Join(dir, "ws")
	git(t, dir, "clone", filepath.Join(dir, "cache", "r"), ws)
	if err := os.WriteFile(filepath.Join(ws, "work.txt"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runClone(t, dir, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "work.txt")); err != nil {
		t.Error("an old repo tree was wiped")
	}
}

// A clone that died before its checkout (here: a base branch the cache does
// not have) left .git and no index. The next warm must not take it for a
// tree: the agent would see every file as deleted.
func TestCloneScript_RedoesACloneKilledBeforeCheckout(t *testing.T) {
	dir := cacheDir(t)
	if err := runClone(t, dir, "nope"); err == nil {
		t.Fatal("a missing base branch succeeded")
	}
	if err := runClone(t, dir, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ws", "README.md")); err != nil {
		t.Errorf("the re-warm kept the empty partial clone: %v", err)
	}
}
