package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// newTestOriginRepo creates a real local git repo (a commit on "main") to
// clone from — same "real temp git repo, no mocking" convention
// grpcserver/server_test.go already uses for this exact kind of logic.
func newTestOriginRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// TestCreateWorktree_ReuseNotWipe covers reliability-findings.md #2's core
// fix: a same-task-ID retry (e.g. after a transient dispatch failure) must
// not destroy uncommitted work already sitting in the worktree — the old
// unconditional os.RemoveAll did exactly that, even though the branch
// itself was already being correctly reused.
func TestCreateWorktree_ReuseNotWipe(t *testing.T) {
	origin := newTestOriginRepo(t)
	m := NewManager(t.TempDir())
	ctx := context.Background()

	if err := m.EnsureRepoCloned(ctx, "repo1", origin); err != nil {
		t.Fatalf("EnsureRepoCloned: %v", err)
	}
	path, branch, err := m.CreateWorktree(ctx, "repo1", "task-1", "main")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	uncommittedFile := filepath.Join(path, "in-progress.txt")
	if err := os.WriteFile(uncommittedFile, []byte("work in progress"), 0o644); err != nil {
		t.Fatalf("write uncommitted file: %v", err)
	}

	path2, branch2, err := m.CreateWorktree(ctx, "repo1", "task-1", "main")
	if err != nil {
		t.Fatalf("CreateWorktree (retry): %v", err)
	}
	if path2 != path || branch2 != branch {
		t.Fatalf("expected retry to return the same path/branch, got %s/%s want %s/%s", path2, branch2, path, branch)
	}
	if _, err := os.Stat(uncommittedFile); err != nil {
		t.Fatalf("expected uncommitted file to survive a same-task-ID retry, stat err: %v", err)
	}
}

// TestSweepGoneBranches_RemovesWorktreeThenBranch covers the sweep's
// [gone]-detection and the remove-before-branch-delete ordering
// (reliability-findings.md #2) — git refuses `branch -D` on a checked-out
// branch, so removing the worktree first is load-bearing, not cosmetic.
func TestSweepGoneBranches_RemovesWorktreeThenBranch(t *testing.T) {
	origin := newTestOriginRepo(t)
	m := NewManager(t.TempDir())
	ctx := context.Background()

	if err := m.EnsureRepoCloned(ctx, "repo1", origin); err != nil {
		t.Fatalf("EnsureRepoCloned: %v", err)
	}
	path, branch, err := m.CreateWorktree(ctx, "repo1", "task-1", "main")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	// Push the branch, then delete it on "origin" — the trigger that makes
	// the local branch's upstream tracking state become [gone] after the
	// sweep's own `fetch --prune`.
	runGit(t, path, "push", "-u", "origin", branch)
	runGit(t, origin, "branch", "-D", branch)

	if err := m.SweepGoneBranches(ctx, "repo1"); err != nil {
		t.Fatalf("SweepGoneBranches: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected worktree dir to be removed, stat err: %v", err)
	}
	out := runGit(t, m.repoPath("repo1"), "branch", "--list", branch)
	if out != "" {
		t.Fatalf("expected branch %s to be deleted, still exists: %q", branch, out)
	}
}

// TestSweepGoneBranches_LeavesActiveBranchesAlone is the negative case: a
// branch whose upstream is still alive (or has no upstream at all yet —
// the sweep's own accepted gap) must not be touched.
func TestSweepGoneBranches_LeavesActiveBranchesAlone(t *testing.T) {
	origin := newTestOriginRepo(t)
	m := NewManager(t.TempDir())
	ctx := context.Background()

	if err := m.EnsureRepoCloned(ctx, "repo1", origin); err != nil {
		t.Fatalf("EnsureRepoCloned: %v", err)
	}
	path, branch, err := m.CreateWorktree(ctx, "repo1", "task-1", "main")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	// Never pushed — no upstream at all, the sweep's documented accepted
	// gap. Must survive.
	if err := m.SweepGoneBranches(ctx, "repo1"); err != nil {
		t.Fatalf("SweepGoneBranches: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected never-pushed worktree to survive the sweep, stat err: %v", err)
	}
	out := runGit(t, m.repoPath("repo1"), "branch", "--list", branch)
	if out == "" {
		t.Fatalf("expected branch %s to still exist", branch)
	}
}

func TestDeleteWorktree_AlsoDeleteBranchIsOptional(t *testing.T) {
	origin := newTestOriginRepo(t)
	m := NewManager(t.TempDir())
	ctx := context.Background()

	if err := m.EnsureRepoCloned(ctx, "repo1", origin); err != nil {
		t.Fatalf("EnsureRepoCloned: %v", err)
	}
	path, branch, err := m.CreateWorktree(ctx, "repo1", "task-1", "main")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	if err := m.DeleteWorktree(ctx, "repo1", "task-1", false); err != nil {
		t.Fatalf("DeleteWorktree: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected worktree dir to be removed, stat err: %v", err)
	}
	out := runGit(t, m.repoPath("repo1"), "branch", "--list", branch)
	if out == "" {
		t.Fatalf("expected branch %s to survive when alsoDeleteBranch=false", branch)
	}
}

// newFleetSharedOriginRepo creates a real local git repo shaped like the
// fleet-shared source dir (CLAUDE.md + a nested skill) — SyncFleetShared's
// own fixture, distinct from newTestOriginRepo's plain README.md since the
// mirror step only looks at a fixed name allowlist.
func newFleetSharedOriginRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("CLAUDE.md", "fleet context")
	write("skills/doubt-driven-development/SKILL.md", "doubt skill")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	return dir
}

func TestSyncFleetShared_ClonesThenMirrorsIntoClaudeHome(t *testing.T) {
	origin := newFleetSharedOriginRepo(t)
	m := NewManager(t.TempDir())
	ctx := context.Background()
	claudeHome := filepath.Join(t.TempDir(), "claude-home")

	if err := m.SyncFleetShared(ctx, origin, "main", claudeHome); err != nil {
		t.Fatalf("SyncFleetShared: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(claudeHome, "CLAUDE.md")); err != nil || string(b) != "fleet context" {
		t.Fatalf("expected CLAUDE.md mirrored, got %q err %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(claudeHome, "skills", "doubt-driven-development", "SKILL.md")); err != nil {
		t.Fatalf("expected skill mirrored: %v", err)
	}
}

// TestSyncFleetShared_UpdateAddsAndRemovesFiles is the concrete proof that
// --delete is actually wired: a skill removed upstream (e.g. via PR) must
// actually disappear from claudeHomeDir on the next dispatch's sync, not
// just accumulate additions forever.
func TestSyncFleetShared_UpdateAddsAndRemovesFiles(t *testing.T) {
	origin := newFleetSharedOriginRepo(t)
	m := NewManager(t.TempDir())
	ctx := context.Background()
	claudeHome := filepath.Join(t.TempDir(), "claude-home")

	if err := m.SyncFleetShared(ctx, origin, "main", claudeHome); err != nil {
		t.Fatalf("first SyncFleetShared: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(origin, "skills", "architecture-interview"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(origin, "skills", "architecture-interview", "SKILL.md"), []byte("interview skill"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(origin, "skills", "doubt-driven-development")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	runGit(t, origin, "add", ".")
	runGit(t, origin, "commit", "-m", "swap skill")

	if err := m.SyncFleetShared(ctx, origin, "main", claudeHome); err != nil {
		t.Fatalf("second SyncFleetShared: %v", err)
	}
	if _, err := os.Stat(filepath.Join(claudeHome, "skills", "architecture-interview", "SKILL.md")); err != nil {
		t.Fatalf("expected new skill to appear: %v", err)
	}
	if _, err := os.Stat(filepath.Join(claudeHome, "skills", "doubt-driven-development")); !os.IsNotExist(err) {
		t.Fatalf("expected removed skill to be deleted from claudeHome, stat err: %v", err)
	}
}

// TestSyncFleetShared_PreservesUnrelatedClaudeHomeContent is the concrete
// regression test for the collision risk between fleet-shared content and
// CLAUDE_CONFIG_DIR's other use — the Agent SDK's own per-task resume state
// under projects/ (docs/adr/0029 point 5) must survive every sync.
func TestSyncFleetShared_PreservesUnrelatedClaudeHomeContent(t *testing.T) {
	origin := newFleetSharedOriginRepo(t)
	m := NewManager(t.TempDir())
	ctx := context.Background()
	claudeHome := filepath.Join(t.TempDir(), "claude-home")

	projectsFile := filepath.Join(claudeHome, "projects", "fake-task", "transcript.json")
	if err := os.MkdirAll(filepath.Dir(projectsFile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(projectsFile, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := m.SyncFleetShared(ctx, origin, "main", claudeHome); err != nil {
		t.Fatalf("SyncFleetShared: %v", err)
	}
	if _, err := os.Stat(projectsFile); err != nil {
		t.Fatalf("expected unrelated projects/ content to survive a sync, stat err: %v", err)
	}
}

func TestListWorktrees_ReflectsUpstreamTrackAndMtime(t *testing.T) {
	origin := newTestOriginRepo(t)
	m := NewManager(t.TempDir())
	ctx := context.Background()

	if err := m.EnsureRepoCloned(ctx, "repo1", origin); err != nil {
		t.Fatalf("EnsureRepoCloned: %v", err)
	}
	path, branch, err := m.CreateWorktree(ctx, "repo1", "task-1", "main")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	runGit(t, path, "push", "-u", "origin", branch)

	infos, err := m.ListWorktrees(ctx, "repo1")
	if err != nil {
		t.Fatalf("ListWorktrees: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("expected 1 worktree, got %d: %+v", len(infos), infos)
	}
	info := infos[0]
	if info.TaskID != "task-1" || info.Repo != "repo1" || info.Branch != branch {
		t.Errorf("unexpected worktree info: %+v", info)
	}
	if info.MtimeUnix == 0 {
		t.Errorf("expected a non-zero mtime, got 0")
	}
}
