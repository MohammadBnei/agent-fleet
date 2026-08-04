package telemetry

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Real temp git repo, no mocking — same convention this fleet's other git-
// adjacent tests already use (worker/src/git.test.ts, provisioner's
// grpcserver_test.go).
func newTestWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-b", "agent/task-1")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	run("add", "a.txt")
	run("commit", "-m", "init")
	return dir
}

func TestComputeSummary_NoChanges(t *testing.T) {
	dir := newTestWorktree(t)
	s, err := computeSummary(dir)
	if err != nil {
		t.Fatalf("computeSummary: %v", err)
	}
	if s.Branch != "agent/task-1" {
		t.Errorf("expected branch agent/task-1, got %q", s.Branch)
	}
	if len(s.Files) != 0 {
		t.Errorf("expected no file changes against a clean worktree, got %+v", s.Files)
	}
}

func TestComputeSummary_ModifiedAndNewFile(t *testing.T) {
	dir := newTestWorktree(t)

	// Modify the tracked file: +1/-1 line.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nCHANGED\nthree\n"), 0o644); err != nil {
		t.Fatalf("modify file: %v", err)
	}
	// New tracked file (staged, so it shows up in `git diff HEAD`).
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatalf("write new file: %v", err)
	}
	addCmd := exec.Command("git", "add", ".")
	addCmd.Dir = dir
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}

	s, err := computeSummary(dir)
	if err != nil {
		t.Fatalf("computeSummary: %v", err)
	}
	if len(s.Files) != 2 {
		t.Fatalf("expected 2 changed files, got %+v", s.Files)
	}
	byPath := map[string]fileChange{}
	for _, f := range s.Files {
		byPath[f.Path] = f
	}
	if a := byPath["a.txt"]; a.Added != 1 || a.Removed != 1 {
		t.Errorf("expected a.txt +1/-1, got +%d/-%d", a.Added, a.Removed)
	}
	if b := byPath["b.txt"]; b.Added != 1 || b.Removed != 0 {
		t.Errorf("expected b.txt +1/-0, got +%d/-%d", b.Added, b.Removed)
	}
}
