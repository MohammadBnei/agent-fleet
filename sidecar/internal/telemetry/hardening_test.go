package telemetry

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The tree is the agent's: its .git/config must not run anything in the
// sidecar, which holds the lease and, in a repo pod, the vault token.
func TestGitOutput_TreeConfigRunsNothing(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "evil.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"},
		{"config", "core.fsmonitor", script},
		{"config", "diff.external", script},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Command("git", "-C", dir, "add", "-N", "a.txt").CombinedOutput(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(marker) // add ran the fsmonitor itself
	for _, args := range [][]string{{"diff", "--numstat", "HEAD"}, {"diff", "HEAD", "--", "a.txt"}} {
		if _, err := gitOutput(dir, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the tree's git config ran a program in the sidecar")
	}
}
