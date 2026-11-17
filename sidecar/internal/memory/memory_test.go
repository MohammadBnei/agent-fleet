package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func log(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "log", "--format=%an|%B").CombinedOutput()
	if err != nil {
		t.Fatal(err, string(out))
	}
	return string(out)
}

func TestTrustedWriteCommitsWithProvenance(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root, Name: "kai", SessionID: "s1", Set: "BC"}
	for _, c := range []string{"one", "two"} {
		if _, err := s.Write("notes/a.md", c); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(root, "agent-kai-memory")
	out := log(t, dir)
	if n := strings.Count(out, "Session: s1"); n != 2 || !strings.Contains(out, "Set: BC") || !strings.Contains(out, "ukubi-agent|") {
		t.Errorf("want 2 tagged commits by ukubi-agent:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "agent-kai-quarantine")); err == nil {
		t.Error("trusted write touched quarantine")
	}
}

func TestSetWithAGoesToQuarantineTaggedNeither(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root, Name: "kai", SessionID: "s2", Set: "AB"}
	if _, err := s.Write("x.md", "hi"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "agent-kai-memory")); err == nil {
		t.Error("A session wrote the trusted repo")
	}
	out := log(t, filepath.Join(root, "agent-kai-quarantine"))
	if !strings.Contains(out, "Set: neither") || strings.Contains(out, "Set: AB") {
		t.Errorf("quarantine must be tagged neither:\n%s", out)
	}
}

func TestRefusals(t *testing.T) {
	root := t.TempDir()
	if _, err := (&Store{Root: root, Name: "kai", Set: "B"}).Write("x", "y"); err != ErrRefused {
		t.Errorf("B-only set: %v", err)
	}
	if _, err := (&Store{Root: root, Name: "kai", Set: ""}).Write("x", "y"); err != ErrRefused {
		t.Errorf("empty set: %v", err)
	}
	s := &Store{Root: root, Name: "kai", Set: "BC"}
	for _, p := range []string{"", "x\n\nSession: other\nSet: BC", "a b", "a\x00b", "a/.git/x", "A/.GIT", "../x", "/etc/x", ".git/config", "a/../../x", ".", ".git"} {
		if _, err := s.Write(p, "y"); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
	if _, err := (&Store{Root: root, Name: "../x", Set: "BC"}).Write("x", "y"); err == nil {
		t.Error("bad name accepted")
	}
}

func TestSymlinkEscapeRefused(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()
	s := &Store{Root: root, Name: "kai", Set: "BC"}
	if _, err := s.Write("a", "1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, filepath.Join(root, "agent-kai-memory", "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write("link/x", "2"); err == nil {
		t.Error("symlink escape accepted")
	}
	if _, err := s.Write("link/new/x", "2"); err == nil {
		t.Error("symlink escape through a missing dir accepted")
	}
	if err := os.Symlink(filepath.Join(out, "t"), filepath.Join(root, "agent-kai-memory", "leaf")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write("leaf", "2"); err == nil {
		t.Error("final-component symlink accepted")
	}
	if _, err := os.Stat(filepath.Join(out, "new")); err == nil {
		t.Error("dir created outside repo")
	}
	if _, err := os.Stat(filepath.Join(out, "t")); err == nil {
		t.Error("file written outside repo")
	}
	if _, err := os.Stat(filepath.Join(out, "x")); err == nil {
		t.Error("file written outside repo")
	}
}

func TestContentCap(t *testing.T) {
	s := &Store{Root: t.TempDir(), Name: "kai", Set: "BC"}
	if _, err := s.Write("big", strings.Repeat("x", MaxContent+1)); err == nil {
		t.Error("oversized write accepted")
	}
	if _, err := s.Write("ok", strings.Repeat("x", MaxContent)); err != nil {
		t.Error(err)
	}
}

// A failed commit leaves no content behind, and nothing staged rides into the next commit.
func TestFailedCommitLeavesNothing(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root, Name: "kai", SessionID: "s1", Set: "BC"}
	dir := filepath.Join(root, "agent-kai-memory")
	if _, err := s.Write("keep", "1"); err != nil {
		t.Fatal(err)
	}
	// A directory where the index lock goes makes add fail.
	if err := os.Mkdir(filepath.Join(dir, ".git", "index.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write("new", "2"); err == nil {
		t.Fatal("write succeeded with a locked index")
	}
	if _, err := s.Write("keep", "changed"); err == nil {
		t.Fatal("write succeeded with a locked index")
	}
	if _, err := os.Stat(filepath.Join(dir, "new")); err == nil {
		t.Error("new file left without a commit")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "keep")); string(b) != "1" {
		t.Errorf("modified file not restored: %q", b)
	}
}

func TestCommitTakesOnlyItsPath(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root, Name: "kai", SessionID: "s1", Set: "BC"}
	dir := filepath.Join(root, "agent-kai-memory")
	if _, err := s.Write("a", "1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := git(dir, "add", "stray"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write("b", "2"); err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command("git", "-C", dir, "show", "--stat", "--format=", "HEAD").CombinedOutput()
	if strings.Contains(string(out), "stray") {
		t.Errorf("staged stray path rode into the commit:\n%s", out)
	}
}

// Core's word and the pod's own set must agree, or the write is refused and nothing lands.
func TestWriteAs_RefusesDisagreement(t *testing.T) {
	root := t.TempDir()
	if _, err := (&Store{Root: root, Name: "kai", Set: "AB"}).WriteAs(false, "x", "y"); err != ErrRefused {
		t.Errorf("A pod told memory: %v", err)
	}
	if _, err := (&Store{Root: root, Name: "kai", Set: "BC"}).WriteAs(true, "x", "y"); err != ErrRefused {
		t.Errorf("BC pod told quarantine: %v", err)
	}
	if _, err := (&Store{Root: root, Name: "kai", Set: "AB"}).WriteAs(true, "x", "y"); err != nil {
		t.Errorf("agreeing write: %v", err)
	}
}

// A sidecar killed mid-commit leaves index.lock; the next pod's first write clears it.
func TestStaleIndexLockClearedOnFirstWrite(t *testing.T) {
	root := t.TempDir()
	first := &Store{Root: root, Name: "kai", SessionID: "s1", Set: "BC"}
	if _, err := first.Write("a", "1"); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(root, "agent-kai-memory", ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	next := &Store{Root: root, Name: "kai", SessionID: "s2", Set: "BC"}
	if _, err := next.Write("b", "2"); err != nil {
		t.Fatalf("stale lock not cleared: %v", err)
	}
}
