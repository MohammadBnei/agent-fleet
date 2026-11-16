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
	for _, p := range []string{"", "../x", "/etc/x", ".git/config", "a/../../x", ".", ".git"} {
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
	if _, err := os.Stat(filepath.Join(out, "x")); err == nil {
		t.Error("file written outside repo")
	}
}
