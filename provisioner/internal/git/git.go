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
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
//
// Reuse, don't wipe, don't validate (reliability-findings.md #2): if the
// worktree path already exists, it's returned as-is — zero git commands,
// zero validity check. The old unconditional os.RemoveAll here destroyed
// uncommitted work on every same-task-ID retry (e.g. after a transient
// dispatch failure), even though the branch itself was correctly reused.
// Stale .git/index.lock, half-written files — that's Claude Code's own
// problem to notice via `git status`/`git log` inside its own pod (it has
// Bash access), not this package's job to pre-empt.
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

	if _, statErr := os.Stat(path); statErr == nil {
		return path, branch, nil
	}

	if err := os.MkdirAll(filepath.Join(m.root, "worktrees"), 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir worktrees root: %w", err)
	}

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

// fleetSharedSentinel keys the repoLock used to serialize SyncFleetShared
// calls — not a real repo name, so it can never collide with one.
const fleetSharedSentinel = "_fleet-shared"

// fleetSharedNames is the exact allowlist of top-level entries mirrored
// from the fleet-shared git source into claudeHomeDir. Deliberately not
// "the whole tree with --delete": claudeHomeDir (CLAUDE_CONFIG_DIR,
// provisioner/internal/k8s/pod.go) also holds projects/, the Agent SDK's
// per-task resume state (docs/adr/0029 point 5) — a stray commit under
// fleet-shared/ must never be able to wipe that.
var fleetSharedNames = []string{"CLAUDE.md", "settings.json", "skills", "plugins"}

// SyncFleetShared clones (or hard-resets) a second, fleet-owned git source
// into <root>/fleet-shared — deliberately outside <root>/repos, so it never
// appears in ListClonedRepos (the sweep loop) or gets treated as a target
// repo anywhere keyed on the repo string — then mirrors fleetSharedNames
// into claudeHomeDir via `rsync -a --delete`, one item at a time, so each
// rsync call only ever touches its own subtree of claudeHomeDir.
//
// This is what makes docs/adr/0019 point 6 real: the Agent SDK's
// settingSources: ["user"] (worker/src/session.ts) natively discovers
// CLAUDE.md/settings.json/skills/plugins under claudeHomeDir, so adding a
// skill becomes a PR merge here, not a worker image rebuild.
func (m *Manager) SyncFleetShared(ctx context.Context, repoURL, branch, claudeHomeDir string) error {
	lock := m.repoLock(fleetSharedSentinel)
	lock.Lock()
	defer lock.Unlock()

	path := filepath.Join(m.root, "fleet-shared")
	if _, err := m.run(ctx, path, "rev-parse", "--is-inside-work-tree"); err != nil {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", path, err)
		}
		if _, err := m.run(ctx, "", "clone", "--branch", branch, repoURL, path); err != nil {
			return fmt.Errorf("clone fleet-shared: %w", err)
		}
	} else {
		// Hard reset, not just fetch: this working tree is provisioner-owned
		// and never written to by a worker, so there's no uncommitted-work
		// concern the way CreateWorktree's reuse-not-wipe logic has to guard
		// against — and a plain fetch alone wouldn't update the tree at all.
		if _, err := m.run(ctx, path, "fetch", "origin", branch); err != nil {
			return fmt.Errorf("fetch fleet-shared: %w", err)
		}
		if _, err := m.run(ctx, path, "reset", "--hard", "origin/"+branch); err != nil {
			return fmt.Errorf("reset fleet-shared: %w", err)
		}
	}

	if err := os.MkdirAll(claudeHomeDir, 0o755); err != nil {
		return fmt.Errorf("mkdir claude home %s: %w", claudeHomeDir, err)
	}
	// repoURL today is this same monorepo (config.FleetSharedRepoURL
	// defaults to it), so the cloned working tree's root is the repo root,
	// not the fleet-shared/ content itself — confirmed live via kind-local,
	// where mirroring straight from the clone root picked up the repo's own
	// top-level CLAUDE.md (a same-named, unrelated file) instead of
	// fleet-shared/CLAUDE.md, and silently skipped skills/ (doesn't exist at
	// the repo root). contentRoot descends into the one subdirectory that
	// actually holds this content.
	contentRoot := filepath.Join(path, "fleet-shared")
	for _, name := range fleetSharedNames {
		src := filepath.Join(contentRoot, name)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		cmd := exec.CommandContext(ctx, "rsync", "-a", "--delete", src, claudeHomeDir+string(filepath.Separator))
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("rsync %s: %w: %s", name, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// ListClonedRepos returns the repo names with a local clone under
// <root>/repos — the sweep's own repo discovery, since the provisioner
// holds no DB credentials to ask core's tasks.KnownRepos instead
// (docs/adr/0020 point 1).
func (m *Manager) ListClonedRepos() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(m.root, "repos"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read repos dir: %w", err)
	}
	repos := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			repos = append(repos, e.Name())
		}
	}
	return repos, nil
}

// SweepGoneBranches deletes agent/<taskId> branches (and their worktrees)
// that were pushed to origin and whose remote branch has since
// disappeared — i.e. already merged/closed on GitHub
// (reliability-findings.md #2's automated half; the dashboard's manual
// DeleteWorktree is the other half, and the fallback for the accepted gaps
// below).
//
// This deliberately does NOT look at %(upstream:track) == "[gone]", which
// is what it used to do and why it never deleted anything in production:
// `git worktree add -b agent/<id> <path> origin/<base>` auto-tracks
// origin/<base>, so the track state reads "[ahead N]" relative to
// origin/main and stays that way after the PR is merged — [gone] would
// require origin/main itself to vanish. The old unit test only passed
// because it pushed with `-u` (repointing the upstream at
// origin/agent/<id>), which the real agent doesn't do.
//
// So instead: a branch is only ever deleted once we have positively
// observed its remote counterpart existing, and later found it pruned. The
// upstream ref doubles as that durable "was pushed" record — pointing it
// at origin/agent/<id> the first time we see the remote branch is both the
// correct upstream for a feature branch and the marker that makes a later
// [gone] real.
//
// Accepted gaps, both falling back to the dashboard's manual delete:
// a branch abandoned *before* its first push never gets a remote
// counterpart to observe, so it leaks; and one merged+deleted inside a
// single sweep interval of its only push is never observed as pushed, so
// it leaks too.
func (m *Manager) SweepGoneBranches(ctx context.Context, repo string) error {
	repoPath := m.repoPath(repo)

	// Network I/O — deliberately outside the per-repo mutex (held only
	// below, for the mutations) so a slow fetch doesn't stall live task
	// dispatch on this repo (CreateWorktree/EnsureRepoCloned take the same
	// lock).
	if _, err := m.run(ctx, repoPath, "fetch", "--prune", "origin"); err != nil {
		return fmt.Errorf("fetch --prune: %w", err)
	}

	out, err := m.run(ctx, repoPath, "for-each-ref", "--format=%(refname:short) %(upstream:short)", "refs/heads/agent/")
	if err != nil {
		return fmt.Errorf("for-each-ref: %w", err)
	}
	if out == "" {
		return nil
	}

	lock := m.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()

	for line := range strings.SplitSeq(out, "\n") {
		branch, upstream, _ := strings.Cut(line, " ")
		if branch == "" {
			continue
		}
		remote := "origin/" + branch
		if _, err := m.run(ctx, repoPath, "rev-parse", "--verify", "--quiet", "refs/remotes/"+remote); err == nil {
			// Remote branch still there: not merged/closed yet. Record it as
			// pushed so a future sweep can act once it's pruned.
			if upstream != remote {
				if _, err := m.run(ctx, repoPath, "branch", "--set-upstream-to="+remote, branch); err != nil {
					slog.Error("sweep: set-upstream failed", "repo", repo, "branch", branch, "error", err)
				}
			}
			continue
		}
		if upstream != remote {
			continue // never observed pushed — accepted gap, leave it alone
		}
		taskID := strings.TrimPrefix(branch, "agent/")
		path := m.worktreePath(taskID)
		// Order matters: worktree remove (updates git's own metadata)
		// before branch -D — git refuses -D on a checked-out branch, the
		// wrong order silently deletes nothing.
		if _, err := m.run(ctx, repoPath, "worktree", "remove", "--force", path); err != nil {
			_ = os.RemoveAll(path)
			_, _ = m.run(ctx, repoPath, "worktree", "prune")
		}
		if _, err := m.run(ctx, repoPath, "branch", "-D", branch); err != nil {
			slog.Error("sweep: branch -D failed", "repo", repo, "branch", branch, "error", err)
		}
	}
	return nil
}

// WorktreeInfo describes one on-disk agent/<taskId> worktree — the
// dashboard's manual cleanup view (reliability-findings.md #2).
type WorktreeInfo struct {
	TaskID        string
	Repo          string
	Branch        string
	UpstreamTrack string // e.g. "[gone]", "[ahead 2]", ""
	MtimeUnix     int64
	Path          string // absolute path on the shared PVC
	DirtyFiles    int32  // uncommitted entries, i.e. `git status --porcelain` lines
	SizeBytes     int64  // recursive size on disk
}

// dirtyFiles counts uncommitted entries in a worktree. Deleting an orphaned
// worktree throws this work away, so it's the warning the dashboard shows
// next to the delete button — not decoration.
func (m *Manager) dirtyFiles(ctx context.Context, path string) int32 {
	out, err := m.run(ctx, path, "status", "--porcelain")
	if err != nil || out == "" {
		return 0
	}
	return int32(strings.Count(out, "\n") + 1)
}

// dirSize sums a worktree's on-disk bytes.
//
// ponytail: a full WalkDir per worktree per call, no cache. Affordable only
// because ListWorktrees has no poller behind it — the dashboard's Worktrees
// page fetches on mount and on an explicit refresh click, so this is paid per
// human action, not per tick. If anything ever polls ListWorktrees, cache this
// by (path, mtime) or move it behind a request flag first.
func dirSize(root string) int64 {
	var total int64
	// Errors are ignored per-entry rather than aborting: a worktree being
	// written to by a live agent will race us, and a partial size is more
	// useful to a human deciding what to delete than no size at all.
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // skip unreadable entries, keep walking
		}
		if fi, statErr := d.Info(); statErr == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// DiskUsage reports total/free bytes for the filesystem holding the
// worktrees root — the shared workspace PVC. Free space is what decides
// whether an orphan has to be pruned now or can wait.
func (m *Manager) DiskUsage() (total uint64, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(filepath.Join(m.root, "worktrees"), &st); err != nil {
		return 0, 0, fmt.Errorf("statfs: %w", err)
	}
	return st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize), nil
}

// ListWorktrees enumerates agent/<taskId> worktrees for repo, read-only
// (no mutex — nothing here mutates git state). %(worktreepath) is empty
// for a branch with no worktree currently checked out (e.g. already
// removed by the sweep but the branch itself somehow survived) — skipped,
// since there's nothing on disk to show or delete for it.
func (m *Manager) ListWorktrees(ctx context.Context, repo string) ([]WorktreeInfo, error) {
	repoPath := m.repoPath(repo)
	out, err := m.run(ctx, repoPath, "for-each-ref", "--format=%(refname:short)|%(upstream:track)|%(worktreepath)", "refs/heads/agent/")
	if err != nil {
		return nil, fmt.Errorf("for-each-ref: %w", err)
	}
	if out == "" {
		return nil, nil
	}

	var infos []WorktreeInfo
	for line := range strings.SplitSeq(out, "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 || parts[2] == "" {
			continue
		}
		branch, track, path := parts[0], parts[1], parts[2]
		info := WorktreeInfo{
			TaskID:        strings.TrimPrefix(branch, "agent/"),
			Repo:          repo,
			Branch:        branch,
			UpstreamTrack: track,
			Path:          path,
			DirtyFiles:    m.dirtyFiles(ctx, path),
			SizeBytes:     dirSize(path),
		}
		if fi, statErr := os.Stat(path); statErr == nil {
			info.MtimeUnix = fi.ModTime().Unix()
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// DeleteWorktree is the dashboard's manual, per-call cleanup action
// (reliability-findings.md #2) — unlike SweepGoneBranches (which only
// ever acts once a branch's upstream is confirmed [gone], so always
// deletes both), a human might want to keep the branch after clearing
// just the worktree's disk space, hence alsoDeleteBranch as a separate
// choice rather than always-both.
func (m *Manager) DeleteWorktree(ctx context.Context, repo, taskID string, alsoDeleteBranch bool) error {
	lock := m.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()

	repoPath := m.repoPath(repo)
	path := m.worktreePath(taskID)

	if _, err := m.run(ctx, repoPath, "worktree", "remove", "--force", path); err != nil {
		_ = os.RemoveAll(path)
		_, _ = m.run(ctx, repoPath, "worktree", "prune")
	}
	if alsoDeleteBranch {
		_, _ = m.run(ctx, repoPath, "branch", "-D", "agent/"+taskID)
	}
	return nil
}
