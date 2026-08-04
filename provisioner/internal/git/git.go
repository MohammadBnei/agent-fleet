// Package git ports worker/src/git.ts's clone/worktree lifecycle into the
// provisioner (docs/adr/0019 point 2: the provisioner owns the entire git
// lifecycle on the shared PVC — clone, fetch, worktree add/remove — worker
// pods never touch git themselves). Since the provisioner is a single
// process, every mutation here is naturally serialized by Manager's
// per-repo mutex — no PVC-level file lock is needed.
package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Manager runs git commands against repo clones rooted at root (the shared
// PVC's mount path inside the provisioner's own pod, config.WorktreesRoot).
// Layout: <root>/repos/<repo>/ (the clone), <root>/worktrees/<taskId>/ (one
// worktree per task, keyed by the already-globally-unique task ID, not
// nested per repo — docs/adr/0019 point 1).
type Manager struct {
	root string

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per-repo, held only while cloning/fetching/adding a worktree for that repo
}

func NewManager(root string) *Manager {
	return &Manager{root: root, locks: make(map[string]*sync.Mutex)}
}

// ConfigureAuth ports worker/src/git.ts's configureGitAuth — the
// provisioner now does the clone/fetch that used to be the worker's job
// (docs/adr/0019 point 2), so it needs the same GH_TOKEN-backed
// authentication the worker configured for itself. A no-op if GH_TOKEN
// isn't set (falls back to whatever ambient git auth is configured), same
// as the TS original. Call once at startup, before any clone/fetch.
func (m *Manager) ConfigureAuth(ctx context.Context) error {
	if os.Getenv("GH_TOKEN") == "" {
		return nil
	}
	if _, err := m.runGh(ctx, "auth", "setup-git"); err != nil {
		return fmt.Errorf("gh auth setup-git: %w", err)
	}
	login, err := m.runGh(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return fmt.Errorf("gh api user: %w", err)
	}
	if _, err := m.run(ctx, "", "config", "--global", "user.name", login); err != nil {
		return err
	}
	_, err = m.run(ctx, "", "config", "--global", "user.email", login+"@users.noreply.github.com")
	return err
}

func (m *Manager) runGh(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (m *Manager) repoLock(repo string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[repo]
	if !ok {
		l = &sync.Mutex{}
		m.locks[repo] = l
	}
	return l
}

func (m *Manager) repoPath(repo string) string {
	return filepath.Join(m.root, "repos", repo)
}

func (m *Manager) worktreePath(taskID string) string {
	return filepath.Join(m.root, "worktrees", taskID)
}

func (m *Manager) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s (in %s): %w: %s", strings.Join(args, " "), dir, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// EnsureRepoCloned clones repoURL into <root>/repos/<repo> if missing, else
// fetches. Serialized per-repo (Manager.repoLock) so two concurrent
// CreateWorkerPod calls for the same never-before-seen repo can never race
// a clone into the same not-yet-existing path — the data-race risk flagged
// during ADR-0019's doubt-driven review.
func (m *Manager) EnsureRepoCloned(ctx context.Context, repo, repoURL string) error {
	lock := m.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()

	path := m.repoPath(repo)
	if _, err := m.run(ctx, path, "rev-parse", "--is-inside-work-tree"); err == nil {
		_, err := m.run(ctx, path, "fetch", "origin")
		return err
	}

	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	_, err := m.run(ctx, "", "clone", repoURL, path)
	return err
}

// CreateWorktree adds a worktree for taskID off origin/<baseBranch> (or
// reuses the branch if it already exists — e.g. a retry after a transient
// dispatch failure). baseBranch defaults to "main" when empty, mirroring
// worker/src/git.ts's BASE_BRANCH default.
func (m *Manager) CreateWorktree(ctx context.Context, repo, taskID, baseBranch string) (path, branch string, err error) {
	if baseBranch == "" {
		baseBranch = "main"
	}
	lock := m.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()

	repoPath := m.repoPath(repo)
	branch = "agent/" + taskID
	path = m.worktreePath(taskID)

	if err := os.MkdirAll(filepath.Join(m.root, "worktrees"), 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir worktrees root: %w", err)
	}
	_ = os.RemoveAll(path) // stale leftovers from a prior failed attempt, if any

	// Clear stale admin metadata after the RemoveAll, not before — a
	// directory we just deleted ourselves still looks "registered" to git
	// otherwise (same ordering worker/src/git.ts's createWorktree used).
	_, _ = m.run(ctx, repoPath, "worktree", "prune")

	branchExists := false
	if out, err := m.run(ctx, repoPath, "branch", "--list", branch); err == nil && out != "" {
		branchExists = true
	}
	if branchExists {
		_, err = m.run(ctx, repoPath, "worktree", "add", path, branch)
	} else {
		_, err = m.run(ctx, repoPath, "worktree", "add", "-b", branch, path, "origin/"+baseBranch)
	}
	if err != nil {
		return "", "", err
	}
	return path, branch, nil
}

// RemoveWorktree tears down a task's worktree — called after the task
// reaches a terminal state (docs/adr/0019 point 2), same lifecycle
// position teardown already occupies for e2e pods.
func (m *Manager) RemoveWorktree(ctx context.Context, repo, taskID, branch string) error {
	lock := m.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()

	repoPath := m.repoPath(repo)
	path := m.worktreePath(taskID)

	if _, err := m.run(ctx, repoPath, "worktree", "remove", "--force", path); err != nil {
		_ = os.RemoveAll(path)
	}
	_, _ = m.run(ctx, repoPath, "branch", "-D", branch)
	return nil
}
