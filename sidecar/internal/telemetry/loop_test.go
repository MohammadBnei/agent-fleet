package telemetry

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// --- diff exchange ------------------------------------------------------------
// The half the console could never show: a file changed by something other than
// an Edit/Write tool call has no captured tool input to replay, so the CHANGES
// panel listed a line count and the modal showed nothing. These pin the git side
// of the answer.

func TestDiffsJSON_ChangeMadeOutsideAToolCall(t *testing.T) {
	dir := newTestWorktree(t)
	// sed, a formatter, a codegen script — the case with no tool input.
	cmd := exec.Command("sed", "-i", "s/two/TWO/", "a.txt")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sed: %v: %s", err, out)
	}

	var diffs map[string]string
	if err := json.Unmarshal([]byte(diffsJSON(dir, []string{"a.txt"})), &diffs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := diffs["a.txt"]
	if !strings.Contains(got, "-two") || !strings.Contains(got, "+TWO") {
		t.Errorf("expected a unified diff of the sed edit, got %q", got)
	}
}

// An unknown path must come back as an empty STRING, not as an absent key. The
// console polls until it gets an answer, so an omission is an infinite spinner
// where "git knows nothing about this file" is a complete answer.
func TestDiffsJSON_UnknownPathAnswersEmptyRatherThanVanishing(t *testing.T) {
	dir := newTestWorktree(t)
	var diffs map[string]string
	if err := json.Unmarshal([]byte(diffsJSON(dir, []string{"a.txt", "never-existed.txt"})), &diffs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := diffs["never-existed.txt"]; !ok {
		t.Error("an unknown path must still get a key, or the console polls forever")
	}
	if diffs["never-existed.txt"] != "" {
		t.Errorf("expected an empty diff, got %q", diffs["never-existed.txt"])
	}
	// A clean tracked file is the same shape of answer, not a missing one.
	if got, ok := diffs["a.txt"]; !ok || got != "" {
		t.Errorf("expected a clean file to answer empty, got %q (present=%v)", got, ok)
	}
}

func TestDiffsJSON_NothingWantedSendsNothing(t *testing.T) {
	if got := diffsJSON(newTestWorktree(t), nil); got != "" {
		t.Errorf("expected no payload when core wants nothing, got %q", got)
	}
}

// These paths come from core, which got them from a browser. git refuses most
// of them on its own; this is the guard that survives someone later swapping
// `git diff --` for something with a --no-index flag.
func TestSafeRelPath(t *testing.T) {
	for _, path := range []string{"src/foo.ts", "a.txt", "dir/sub/x.go"} {
		if !safeRelPath(path) {
			t.Errorf("%q should be accepted", path)
		}
	}
	for _, path := range []string{"", "/etc/passwd", "../../etc/passwd", "src/../../x", "--output=/tmp/x"} {
		if safeRelPath(path) {
			t.Errorf("%q should be refused", path)
		}
	}
}

// Escapes are dropped, not passed through with an empty answer: an unsafe path
// is a bug or an attack, and either way it is not a file the console asked
// about.
func TestDiffsJSON_SkipsUnsafePaths(t *testing.T) {
	dir := newTestWorktree(t)
	var diffs map[string]string
	if err := json.Unmarshal([]byte(diffsJSON(dir, []string{"a.txt", "../../etc/passwd"})), &diffs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := diffs["../../etc/passwd"]; ok {
		t.Error("an unsafe path must not reach git at all")
	}
	if len(diffs) != 1 {
		t.Errorf("expected only the safe path, got %+v", diffs)
	}
}

// The list and the diff must ask git the same question. computeSummary uses
// `git diff --numstat HEAD`, so a STAGED change is listed by the CHANGES
// panel — and a bare `git diff` (against the index) reports nothing for it,
// which the console renders as "committed or reverted". Any session where the
// agent has run `git add` is in this state, so it is common rather than an
// edge case.
func TestDiffsJSON_StagedChangeIsStillADiff(t *testing.T) {
	dir := newTestWorktree(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nTWO\nthree\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	add := exec.Command("git", "add", "a.txt")
	add.Dir = dir
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}

	// Precondition: numstat lists it, so the panel offers a row to click.
	s, err := computeSummary(dir)
	if err != nil {
		t.Fatalf("computeSummary: %v", err)
	}
	if len(s.Files) != 1 || s.Files[0].Path != "a.txt" {
		t.Fatalf("expected numstat to list the staged file, got %+v", s.Files)
	}

	var diffs map[string]string
	if err := json.Unmarshal([]byte(diffsJSON(dir, []string{"a.txt"})), &diffs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(diffs["a.txt"], "+TWO") {
		t.Errorf("a staged change must still produce a diff, got %q", diffs["a.txt"])
	}
}
