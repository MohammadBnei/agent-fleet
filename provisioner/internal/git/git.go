// Package git maintains the fleet's shared clone cache.
//
// It used to own the whole git lifecycle on one shared PVC — clone, fetch,
// worktree add/remove, branch sweeps, disk accounting (docs/adr/0019 point 2).
// docs/adr/0048 §5 took nearly all of that away: the fleet does not create
// working trees or name branches, a session's tree is a --shared clone made by
// an init container in its own pod, and the retention GC deletes a PVC rather
// than an `agent/<id>` worktree.
//
// What is left is the cache those clones read from, and the per-session
// claude-home the provisioner seeds before a pod exists. Since the provisioner
// is a single process, every mutation here is serialized by Manager's per-repo
// mutex — no PVC-level file lock is needed.
package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/metrics"
)

// Manager runs git commands against repo clones rooted at root (the shared
// PVC's mount path inside the provisioner's own pod, config.WorkspaceRoot).
//
// Layout: <root>/repos/<repo>/ is the clone cache every session clones from,
// mounted read-only into session pods; <root>/claude-home/<sessionId>/ is that
// session's SDK state. There is no <root>/worktrees/ any more.
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

func (m *Manager) run(ctx context.Context, dir string, args ...string) (string, error) {
	// Every git command in this package routes through here, so one timer
	// covers clone/fetch/worktree add+remove/branch without touching a
	// single caller. Only the subcommand is labelled — the rest of the argv
	// carries branch and task names, which are unbounded.
	defer metrics.ObserveGit(args, time.Now())

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s (in %s): %w: %s", strings.Join(args, " "), dir, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// SyncResult describes what a call to EnsureRepoCloned actually did. It exists
// for the dashboard's manual sync button (SyncRepoCache), which is a human
// asking "did that do anything?" and deserves an answer better than "no error".
//
// Every field is best-effort by construction — see EnsureRepoCloned.
type SyncResult struct {
	// The cache did not exist and this call cloned it. Advanced is meaningless.
	Cloned bool
	// origin/HEAD after the sync, or "" if it could not be resolved.
	Head string
	// Commits origin/HEAD moved by this fetch. 0 when Cloned, and 0 when Head
	// is "" — absence of a number, not a claim that nothing moved.
	Advanced int
}

// EnsureRepoCloned clones repoURL into <root>/repos/<repo> if missing, else
// fetches. Serialized per-repo (Manager.repoLock) so two concurrent
// CreateWorkerPod calls for the same never-before-seen repo can never race
// a clone into the same not-yet-existing path — the data-race risk flagged
// during ADR-0019's doubt-driven review.
//
// The SyncResult reads are deliberately error-discarding. A cache with no
// origin/HEAD symref (every cache cloned before this existed is a candidate)
// must still sync; failing a fetch that worked because a statistic about it
// could not be computed would turn a reporting feature into an outage.
func (m *Manager) EnsureRepoCloned(ctx context.Context, repo, repoURL string) (SyncResult, error) {
	lock := m.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()

	path := m.repoPath(repo)
	if _, err := m.run(ctx, path, "rev-parse", "--is-inside-work-tree"); err == nil {
		before := m.originHead(ctx, path)
		if _, err := m.run(ctx, path, "fetch", "origin"); err != nil {
			return SyncResult{}, err
		}
		after := m.originHead(ctx, path)
		res := SyncResult{Head: after}
		if before != "" && after != "" && before != after {
			if out, err := m.run(ctx, path, "rev-list", "--count", before+".."+after); err == nil {
				res.Advanced, _ = strconv.Atoi(out)
			}
		}
		return res, m.disableAutoGC(ctx, path)
	}

	if err := os.MkdirAll(path, 0o755); err != nil {
		return SyncResult{}, fmt.Errorf("mkdir %s: %w", path, err)
	}
	if _, err := m.run(ctx, "", "clone", repoURL, path); err != nil {
		return SyncResult{}, err
	}
	return SyncResult{Cloned: true, Head: m.originHead(ctx, path)}, m.disableAutoGC(ctx, path)
}

// originHead resolves the cache's origin/HEAD, or "" if it has none. `git
// clone` writes the refs/remotes/origin/HEAD symref, but a fetch never
// creates one, so a cache that lost it (or predates one) stays "" forever
// rather than being repaired here — repairing it means a `remote set-head`
// network round trip on a path whose only job is to be fast and idempotent.
func (m *Manager) originHead(ctx context.Context, path string) string {
	out, err := m.run(ctx, path, "rev-parse", "--verify", "--quiet", "origin/HEAD")
	if err != nil {
		return ""
	}
	return out
}

// disableAutoGC is the one sharp edge of the `--shared` clones the session
// pods make from this cache (docs/adr/0048 §5).
//
// A `--shared` clone writes an `alternates` file pointing back here instead of
// copying objects, so every live session is reading objects out of this
// directory. `git gc` running here — and it runs automatically, on fetch, once
// enough loose objects accumulate — can prune an object no branch in THIS repo
// references but a session's own new commits do. The session then has a
// corrupt repository, at an arbitrary later moment, with no connection to the
// fetch that caused it. This is a documented git hazard, not a theory.
//
// Set on every call rather than only at clone time: the config lives in the
// cache directory, which outlives any one provisioner and predates this rule.
func (m *Manager) disableAutoGC(ctx context.Context, path string) error {
	_, err := m.run(ctx, path, "config", "gc.auto", "0")
	return err
}

// CreateWorktree is gone (docs/adr/0048 §5). The fleet no longer creates a
// working tree, names a branch, or knows an `agent/<id>` convention — the
// session pod's own init container clones from this cache into its per-session
// volume, and the agent runs `git checkout -b` for whatever it likes.
//
// It could not have survived §4 in any case: the working tree now lives on a
// per-session `local-path` PVC that this process never mounts, so a worktree
// added here would have landed on a different volume entirely, silently.
//
// ClaudeHomePath is what remains of the per-session filesystem the provisioner
// still owns: the SDK's resume state, which lives on the shared RWX volume
// because it has to survive the node its pod ran on and has to be seeded
// before that pod exists.
func (m *Manager) ClaudeHomePath(sessionID string) string {
	return filepath.Join(m.root, "claude-home", sessionID)
}

// EnsureClaudeHome creates a session's claude-home and makes it writable by the
// worker container, which runs as uid 1000 while everything this process
// creates is root's (main.go's umask(0) comment covers the policy).
//
// It is not optional, and it is not part of the fleet-shared sync: the worker's
// entrypoint copies its plugins in here on first boot and the Agent SDK writes
// its whole resume state here. Left root-owned 0755, the copy fails with
// "cp: cannot create directory '/claude-home/plugins': Permission denied" and
// `set -e` kills the container before the session ever starts — a crash loop
// that says nothing about the actual fault. Confirmed live 2026-08-15.
//
// fsGroup does not cover this the way it covers the per-session volume: the
// shared PVC is RWX Longhorn, which is NFS underneath, and kubelet applies no
// ownership pass to it.
//
// Chmod as well as MkdirAll's mode, because MkdirAll is umask-filtered and does
// nothing whatsoever to a directory that already exists — every session seeded
// before this fix has a 0755 claude-home that would otherwise stay broken
// across every warm.
func (m *Manager) EnsureClaudeHome(sessionID string) error {
	path := m.ClaudeHomePath(sessionID)
	if err := os.MkdirAll(path, 0o777); err != nil {
		return fmt.Errorf("mkdir claude home %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o777); err != nil {
		return fmt.Errorf("chmod claude home %s: %w", path, err)
	}
	return nil
}

// DeleteSessionDir removes a session's SDK state. Its working tree is a PVC
// and is deleted by the k8s client, not from here — the two halves of a
// session's disk now live on two different volumes, which is the point of
// splitting them by access pattern.
//
// Idempotent: sweeping something already gone is a correct no-op, and the
// retention GC will call this again on any pass where the PVC delete failed.
func (m *Manager) DeleteSessionDir(sessionID string) error {
	if err := os.RemoveAll(m.ClaudeHomePath(sessionID)); err != nil {
		return fmt.Errorf("remove claude home for %s: %w", sessionID, err)
	}
	return nil
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

	// 0o777, matching EnsureClaudeHome — this runs first on a kind-local or
	// test path where nothing called that yet, and a 0755 claude-home is a
	// worker crash loop.
	if err := os.MkdirAll(claudeHomeDir, 0o777); err != nil {
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
