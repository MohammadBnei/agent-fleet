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
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/metrics"
	"github.com/MohammadBnei/agent-fleet/provisioner/internal/spans"
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
	// GitEnv, when set, authenticates clone/fetch with the App's read-only
	// installation tokens (docs/adr/0075 §9), passed as GIT_CONFIG_* env so
	// no token reaches argv, which run() quotes in errors. An error falls back
	// to the ambient git credentials.
	GitEnv func(context.Context) ([]string, error)

	// fleetSharedReady: <root>/fleet-shared holds a usable checkout this
	// process has refreshed at least once (possibly stale, if the last
	// fetch failed), so a warm lays it in without fetching.
	fleetSharedReady atomic.Bool
	// fleetSharedRefresh runs one fleet-shared refresh at a time. Taken
	// before the fleet-shared lock, never while holding it.
	fleetSharedRefresh sync.Mutex

	// lastUsed is when a warm or manual sync last touched each repo's cache
	// (in memory: a restart forgets it, see PrefetchRepos). Guarded by mu.
	lastUsed map[string]time.Time
}

type prefixKey struct{}

// spanPrefix names the spans of git work: "warm" on the warm path, "prefetch"
// for the background refresh, so Tempo's warm.* never counts background fetches.
func spanPrefix(ctx context.Context) string {
	if p, ok := ctx.Value(prefixKey{}).(string); ok {
		return p
	}
	return "warm"
}

func NewManager(root string) *Manager {
	return &Manager{root: root, locks: make(map[string]*sync.Mutex), lastUsed: make(map[string]time.Time)}
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

// LockRepo holds repo's cache lock until the returned func runs: what a
// concurrent sync looks like to a pod create. For tests in other packages.
func (m *Manager) LockRepo(repo string) (unlock func()) {
	l := m.repoLock(repo)
	l.Lock()
	return l.Unlock
}

func (m *Manager) touch(repo string) {
	m.mu.Lock()
	m.lastUsed[repo] = time.Now()
	m.mu.Unlock()
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
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	ctx, end := spans.Start(ctx, spanPrefix(ctx)+".git."+sub)
	var runErr error
	defer func() { end(runErr) }()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if m.GitEnv != nil && len(args) > 0 && (args[0] == "clone" || args[0] == "fetch") {
		envCtx, endEnv := spans.Start(ctx, spanPrefix(ctx)+".git_credentials")
		env, err := m.GitEnv(envCtx)
		endEnv(err)
		if err != nil {
			slog.Warn("git: no App credentials, using ambient git auth", "error", err)
		} else {
			cmd.Env = append(os.Environ(), env...)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		runErr = err
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
	// is "" — absence of a number, not a claim that nothing moved. Read
	// Changed for that.
	Advanced int
	// origin/HEAD is not the commit it was before the fetch. A force-push that
	// rewinds the branch changes it while advancing it by nothing.
	Changed bool
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
	m.touch(repo)
	lock := m.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	return m.ensureLocked(ctx, repo, repoURL)
}

// EnsureRepoFetched always fetches, waiting for the repo lock no longer than
// ctx. A definition pod's clone reads the cache's origin/<base> and never
// contacts the remote (docs/adr/0064), so a fetch skipped under a busy lock
// would start the session on a stale base, or kill it (BackoffLimit 0) when the
// base branch is newer than the cache's last fetch. The plain sync.Mutex
// cannot be waited on with a deadline, so this polls TryLock, and a wait past
// ctx returns before any git runs.
//
// ponytail: 200ms poll; a channel semaphore if the latency ever shows.
func (m *Manager) EnsureRepoFetched(ctx context.Context, repo, repoURL string) error {
	m.touch(repo)
	return m.ensureFetched(ctx, repo, repoURL)
}

// ensureFetched is EnsureRepoFetched's body, shared with the background
// prefetch (which sets a "prefetch" span prefix on ctx and does not touch).
func (m *Manager) ensureFetched(ctx context.Context, repo, repoURL string) (err error) {
	p := spanPrefix(ctx)
	ctx, end := spans.Start(ctx, p+".ensure_repo_fetched", attribute.String("repo", repo))
	defer func() { end(err) }()
	lock := m.repoLock(repo)
	_, endWait := spans.Start(ctx, p+".repo_lock_wait", attribute.String("repo", repo))
	err = lockCtx(ctx, lock)
	endWait(err)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	// docs/adr/0065 §6: the URL a definition clones from is bound to repos.url.
	// ensureLocked never re-points a warm cache, so check it did not come from
	// somewhere else first. No cache yet: the clone below uses repoURL.
	if origin, err := m.run(ctx, m.repoPath(repo), "config", "--get", "remote.origin.url"); err == nil &&
		!strings.EqualFold(strings.TrimSuffix(origin, ".git"), strings.TrimSuffix(repoURL, ".git")) {
		return fmt.Errorf("cache %s: origin %q does not match repos.url %q; delete %s on the provisioner's workspace PVC and the next warm re-clones from repos.url (the console's sync only fetches)", repo, origin, repoURL, m.repoPath(repo))
	}
	_, err = m.ensureLocked(ctx, repo, repoURL)
	return err
}

// ensureLocked is EnsureRepoCloned's body. Callers must hold the repo's lock.
func (m *Manager) ensureLocked(ctx context.Context, repo, repoURL string) (SyncResult, error) {
	path := m.repoPath(repo)
	if _, err := m.run(ctx, path, "rev-parse", "--is-inside-work-tree"); err == nil {
		before := m.originHead(ctx, path)
		if _, err := m.run(ctx, path, "fetch", "origin"); err != nil {
			return SyncResult{}, err
		}
		after := m.originHead(ctx, path)
		res := SyncResult{Head: after}
		if before != "" && after != "" && before != after {
			res.Changed = true
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
	// docs/adr/0063: the genome is a sibling, so it is swept separately.
	leftovers, _ := filepath.Glob(m.GenomePath(sessionID) + ".new-*")
	for _, p := range append(leftovers, m.GenomePath(sessionID)) {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("remove genome for %s: %w", sessionID, err)
		}
	}
	return nil
}

// RemoveFleetShared deletes fleetSharedNames from a session's claude-home,
// leaving projects/ (its resume state) alone. For a definition pod
// (docs/adr/0064): an older provisioner replica serving one of its creates in
// a rollout overlap ran the sync unconditionally, and that settings.json
// pre-approves arbitrary-exec Bash past canUseTool for the session's life.
//
// Only when a sync left something behind. The entrypoint writes plugins/ (the
// image's own copy, once per claude-home) and none of the other names, so
// without them plugins/ is the image's and stays: removing it would re-run
// that cp -r, the dominant cold-start cost (plan §5c spike 7), on every warm.
// After a sync plugins/ goes too, and the entrypoint re-seeds the image's.
func (m *Manager) RemoveFleetShared(sessionID string) error {
	home := m.ClaudeHomePath(sessionID)
	synced := false
	for _, name := range fleetSharedNames {
		if _, err := os.Lstat(filepath.Join(home, name)); name != "plugins" && !errors.Is(err, fs.ErrNotExist) {
			synced = true
		}
	}
	// docs/adr/0067 §4: an empty skills/ is the mount point kubelet made for
	// the genome's read-only skills mount, not a sync's leftover.
	if entries, err := os.ReadDir(filepath.Join(home, "skills")); err == nil && len(entries) == 0 {
		synced = false
		for _, name := range fleetSharedNames {
			if _, err := os.Lstat(filepath.Join(home, name)); name != "plugins" && name != "skills" && !errors.Is(err, fs.ErrNotExist) {
				synced = true
			}
		}
	}
	if !synced {
		return nil
	}
	for _, name := range fleetSharedNames {
		if err := os.RemoveAll(filepath.Join(home, name)); err != nil {
			return fmt.Errorf("remove fleet-shared %s for %s: %w", name, sessionID, err)
		}
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

// RefreshFleetShared brings <root>/fleet-shared up to branch. The
// provisioner's background tick calls it (cmd/provisioner,
// fleetSharedRefreshInterval), so a warm only lays the local copy into its
// claude-home: a warm sees fleet-shared at most one tick stale. Fleet-shared is
// reviewed config that changes rarely; the cache a session clones from is not,
// and EnsureRepoFetched still fetches on every warm.
//
// Detached and bounded: shutdown must not SIGKILL git mid-reset, and a hung
// fetch ends at refreshTimeout. The fetch holds no lock a warm waits on: only
// the reset (or a re-clone's swap) takes the fleet-shared lock.
func (m *Manager) RefreshFleetShared(ctx context.Context, repoURL, branch string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	m.fleetSharedRefresh.Lock()
	defer m.fleetSharedRefresh.Unlock()
	return m.refreshFleetShared(ctx, repoURL, branch)
}

// refreshFleetShared is RefreshFleetShared's body; the caller holds
// fleetSharedRefresh. An intact checkout whose fetch fails is kept, stale
// and still ready: a GitHub outage must not delete the copy every warm lays
// in. The re-clone that then runs (it also repairs a clone a killed fetch
// left with a stuck ref lock) builds beside the checkout and swaps in only on
// success. A missing or broken checkout is re-cloned the same way.
func (m *Manager) refreshFleetShared(ctx context.Context, repoURL, branch string) error {
	path := m.fleetSharedPath()
	lock := m.repoLock(fleetSharedSentinel)
	if m.checkoutIntact(ctx, path) {
		err := m.fetchAndReset(ctx, path, branch, lock)
		if err == nil {
			m.fleetSharedReady.Store(true)
			return nil
		}
		m.fleetSharedReady.Store(true)
		if cerr := m.recloneFleetShared(ctx, path, repoURL, branch, lock); cerr != nil {
			return fmt.Errorf("fleet-shared kept stale: %w", errors.Join(err, cerr))
		}
		return nil
	}
	err := m.recloneFleetShared(ctx, path, repoURL, branch, lock)
	m.fleetSharedReady.Store(err == nil)
	return err
}

// checkoutIntact: path is a checkout a warm can read: HEAD resolves, the
// index exists and no index.lock says a reset or clone died mid-write.
func (m *Manager) checkoutIntact(ctx context.Context, path string) bool {
	if _, err := m.run(ctx, path, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
		return false
	}
	gitDir := filepath.Join(path, ".git")
	if _, err := os.Stat(filepath.Join(gitDir, "index")); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(gitDir, "index.lock"))
	return errors.Is(err, fs.ErrNotExist)
}

// fetchAndReset fetches outside the fleet-shared lock (a fetch never touches
// the working tree a warm rsyncs from), then resets under it when the ref
// moved.
func (m *Manager) fetchAndReset(ctx context.Context, path, branch string, lock *sync.Mutex) error {
	if _, err := m.run(ctx, path, "fetch", "origin", branch); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if err := lockCtx(ctx, lock); err != nil {
		return err
	}
	defer lock.Unlock()
	if fetchedIsCheckedOut(ctx, m, path) {
		return nil
	}
	if _, err := m.run(ctx, path, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	return nil
}

// recloneFleetShared clones beside path and swaps the clone in under the
// fleet-shared lock, so a failed clone never costs the copy already there.
func (m *Manager) recloneFleetShared(ctx context.Context, path, repoURL, branch string, lock *sync.Mutex) error {
	tmp, old := path+".new", path+".old"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		return err
	}
	if _, err := m.run(ctx, "", "clone", "--branch", branch, repoURL, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("clone: %w", err)
	}
	if err := lockCtx(ctx, lock); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	err := os.RemoveAll(old)
	if err == nil {
		if err = os.Rename(path, old); errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	lock.Unlock()
	if err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("swap fleet-shared clone: %w", err)
	}
	return os.RemoveAll(old)
}

func (m *Manager) fleetSharedPath() string { return filepath.Join(m.root, "fleet-shared") }

// SyncFleetShared mirrors fleetSharedNames from <root>/fleet-shared — a
// second, fleet-owned git clone, deliberately outside <root>/repos, so it
// never appears in ListClonedRepos (the sweep loop) or gets treated as a
// target repo anywhere keyed on the repo string — into claudeHomeDir via
// `rsync -a --delete`, one item at a time, so each rsync call only ever
// touches its own subtree of claudeHomeDir.
//
// The clone is refreshed off the warm path (RefreshFleetShared). Only when
// this process has no usable checkout yet (cold start, or the clone is gone)
// does the warm refresh it itself, as it always used to. Every wait is
// bounded by ctx: a warm that cannot get the clone in time returns the error
// (its span records it) and the caller goes on without the sync.
//
// This is what makes docs/adr/0019 point 6 real: the Agent SDK's
// settingSources: ["user"] (worker/src/session.ts) natively discovers
// CLAUDE.md/settings.json/skills/plugins under claudeHomeDir, so adding a
// skill becomes a PR merge here, not a worker image rebuild.
func (m *Manager) SyncFleetShared(ctx context.Context, repoURL, branch, claudeHomeDir string) (err error) {
	ctx, end := spans.Start(ctx, "warm.sync_fleet_shared")
	defer func() { end(err) }()
	if !m.fleetSharedReady.Load() {
		if err := spans.Do(ctx, "warm.fleet_shared_refresh", func(ctx context.Context) error {
			if err := lockCtx(ctx, &m.fleetSharedRefresh); err != nil {
				return err
			}
			defer m.fleetSharedRefresh.Unlock()
			if m.fleetSharedReady.Load() { // a refresh finished while this waited
				return nil
			}
			return m.refreshFleetShared(ctx, repoURL, branch)
		}); err != nil && !m.fleetSharedReady.Load() {
			return fmt.Errorf("fleet-shared: %w", err)
		}
	}
	lock := m.repoLock(fleetSharedSentinel)
	_, endWait := spans.Start(ctx, "warm.fleet_shared_lock_wait")
	err = lockCtx(ctx, lock)
	endWait(err)
	if err != nil {
		return fmt.Errorf("fleet-shared lock: %w", err)
	}
	defer lock.Unlock()
	path := m.fleetSharedPath()

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
	if _, err := os.Stat(contentRoot); err != nil {
		// A source without the directory at all is a wrong URL or ref, not
		// a move: deleting on its word would wipe every warmed session.
		return fmt.Errorf("fleet-shared source: %w", err)
	}
	var errs []error
	for _, name := range fleetSharedNames {
		src := filepath.Join(contentRoot, name)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			// docs/adr/0063 §8: CLAUDE.md and skills/ moved to packs/, and a
			// resumed session's claude-home still holds the last synced copy.
			// Never plugins/: without a source it is the image's copy, which
			// the entrypoint seeds once per claude-home.
			if name == "CLAUDE.md" || name == "skills" {
				if err := os.RemoveAll(filepath.Join(claudeHomeDir, name)); err != nil {
					errs = append(errs, fmt.Errorf("remove %s: %w", name, err))
				}
			}
			continue
		}
		// One name failing never skips the rest: settings.json is the
		// session's permission rules.
		_, endRsync := spans.Start(ctx, "warm.rsync", attribute.String("name", name))
		cmd := exec.CommandContext(ctx, "rsync", "-a", "--delete", src, claudeHomeDir+string(filepath.Separator))
		out, err := cmd.CombinedOutput()
		endRsync(err)
		if err != nil {
			errs = append(errs, fmt.Errorf("rsync %s: %w: %s", name, err, strings.TrimSpace(string(out))))
		}
	}
	return errors.Join(errs...)
}

// lockCtx takes lock, giving up when ctx ends; a sync.Mutex cannot be waited
// on with a deadline, so it polls TryLock. On success ctx is still live, so
// no git starts for a caller that already left.
//
// ponytail: 200ms poll; a channel semaphore if the latency ever shows.
func lockCtx(ctx context.Context, lock *sync.Mutex) error {
	for !lock.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		lock.Unlock()
		return err
	}
	return nil
}

// refreshDetached runs refreshClone on a context detached from the caller,
// under its own bound. core's ListDefinitionFiles has a 30s deadline, and a
// cancelled ctx SIGKILLs git mid-reset on a slow PVC, leaving index.lock
// (2026-10-20, v6.2.0 rollout: ~3 min of failed syncs). The caller holds
// path's lock, so the next call waits for it, then reads a ready clone.
func (m *Manager) refreshDetached(ctx context.Context, path, repoURL, ref string, fetchArgs ...string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	return m.refreshClone(ctx, path, repoURL, ref, fetchArgs...)
}

// refreshTimeout bounds one detached refresh, re-clone included.
const refreshTimeout = 5 * time.Minute

// refreshClone makes path a provisioner-owned clone of repoURL at ref (a
// branch or a tag): `clone --branch` when absent, else fetch (with any extra
// fetchArgs) + hard reset to FETCH_HEAD. The caller holds path's lock.
//
// Hard reset, not just fetch: this working tree is provisioner-owned and
// never written to by a worker, so there is no uncommitted work to protect —
// and a plain fetch alone would not update the tree at all.
func (m *Manager) refreshClone(ctx context.Context, path, repoURL, ref string, fetchArgs ...string) error {
	err := m.updateClone(ctx, path, repoURL, ref, fetchArgs...)
	if err == nil || ctx.Err() != nil {
		return err
	}
	// The clone is disposable, so a failed refresh is retried once from
	// scratch. A provisioner killed mid-git (a rollout, an OOM) can leave
	// index.lock, a ref lock, packed-refs.lock or a half-built .git on the
	// PVC, and every later refresh failed on it until someone cleaned up by
	// hand (2026-10-20: definitions sync down ~2h after the 6.0.2 rollout).
	// ponytail: a GitHub outage also re-clones once per call; cheap at this
	// size.
	slog.Warn("git: refresh failed, re-cloning from scratch", "path", path, "error", err)
	if rmErr := os.RemoveAll(path); rmErr != nil {
		return errors.Join(err, fmt.Errorf("remove %s: %w", path, rmErr))
	}
	if retry := m.updateClone(ctx, path, repoURL, ref, fetchArgs...); retry != nil {
		return errors.Join(err, fmt.Errorf("re-clone: %w", retry))
	}
	return nil
}

func (m *Manager) updateClone(ctx context.Context, path, repoURL, ref string, fetchArgs ...string) error {
	if _, err := m.run(ctx, path, "rev-parse", "--is-inside-work-tree"); err != nil {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", path, err)
		}
		if _, err := m.run(ctx, "", "clone", "--branch", ref, repoURL, path); err != nil {
			return fmt.Errorf("clone: %w", err)
		}
		return nil
	}
	if _, err := m.run(ctx, path, append(append([]string{"fetch"}, fetchArgs...), "origin", ref)...); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if fetchedIsCheckedOut(ctx, m, path) {
		return nil
	}
	if _, err := m.run(ctx, path, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	return nil
}

// fetchedIsCheckedOut: HEAD is already FETCH_HEAD and the checkout finished,
// so the hard reset would rewrite nothing. It still costs: `reset --hard`
// refreshes the index, an lstat of every tracked file of the monorepo on the
// RWX (NFS) volume, ~1.4s live for a no-op. reset moves HEAD only after the
// index and tree are written, and clone writes the index only once its
// checkout is done, so a killed reset or clone never reads as checked out;
// an index.lock left by one fails the next reset, which re-clones.
func fetchedIsCheckedOut(ctx context.Context, m *Manager, path string) bool {
	out, err := m.run(ctx, path, "rev-parse", "HEAD", "FETCH_HEAD")
	heads := strings.Fields(out)
	if err != nil || len(heads) != 2 || heads[0] != heads[1] {
		return false
	}
	gitDir := filepath.Join(path, ".git")
	if _, err := os.Stat(filepath.Join(gitDir, "index")); err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(gitDir, "index.lock"))
	return errors.Is(err, fs.ErrNotExist)
}

// packEntryProblem says why one packs/ entry is not read, or "" when it is
// read. Its own budget, so packs never push definitions over theirs.
func packEntryProblem(rel string, f []string, packTotal int) string {
	if rel == "packs" {
		return "packs is not a directory"
	}
	if len(f) != 4 || (f[0] != "100644" && f[0] != "100755") {
		return rel + " is not a regular file"
	}
	size, err := strconv.Atoi(f[3])
	switch {
	case err != nil:
		return rel + ": size " + f[3]
	case size > maxDefinitionFile:
		return fmt.Sprintf("%s is %d bytes, over %d", rel, size, maxDefinitionFile)
	case packTotal+size > maxDefinitionTotal:
		return fmt.Sprintf("packs/ is over %d bytes at %s", maxDefinitionTotal, rel)
	}
	return ""
}

// definitionsSentinel keys ReadDefinitions' own lock, never fleet-shared's.
const definitionsSentinel = "_definitions"

// Size bounds on ListDefinitionFiles (gRPC's default limit is 4 MiB).
const (
	maxDefinitionFile  = 64 << 10
	maxDefinitionTotal = 1 << 20
)

// ReadDefinitions refreshes <root>/definitions-src to ref and returns every
// regular file under its definitions/ and packs/ (keys relative to the clone root) and
// the commit they were read at (docs/adr/0062).
//
// Its own clone and its own lock: a slow GitHub fetch here never stalls
// CreateWorkerPod, which takes the fleet-shared lock blocking. Holding the lock
// across fetch and read keeps the sha matched to the content.
//
// FAILS CLOSED: a missing directory, a symlink, a submodule, an oversize file
// or a git failure is an error, never an empty list.
//
// ponytail: a second full clone of the monorepo on the provisioner's disk;
// `--depth 1` if disk ever matters.
func (m *Manager) ReadDefinitions(ctx context.Context, repoURL, ref string) (map[string][]byte, string, error) {
	lock := m.repoLock(definitionsSentinel)
	if err := lockCtx(ctx, lock); err != nil {
		return nil, "", err
	}
	defer lock.Unlock()

	path := filepath.Join(m.root, "definitions-src")
	// A release tag never moves: once HEAD is at it there is nothing to fetch,
	// so the steady state (core asks every 60s) makes no GitHub round-trip. A
	// branch ref (kind, dev) has no local tag and always fetches. --tags keeps
	// a fetched tag as a local ref, which this check needs.
	sha, err := m.run(ctx, path, "rev-parse", "HEAD")
	if tag, tagErr := m.run(ctx, path, "rev-parse", "-q", "--verify", "refs/tags/"+ref+"^{commit}"); err != nil || tagErr != nil || tag != sha {
		if err := m.refreshDetached(ctx, path, repoURL, ref, "--tags"); err != nil {
			return nil, "", fmt.Errorf("definitions clone at %q: %w", ref, err)
		}
		if sha, err = m.run(ctx, path, "rev-parse", "HEAD"); err != nil {
			return nil, "", err
		}
	}
	// Read from the commit, never the working tree: the sha is the pin, so
	// the content must be exactly that commit's, whatever the checkout on
	// this PVC looks like (the steady state above never resets it).
	// packs/ rides along (docs/adr/0063): reviewed context packs are read at
	// the same release tag, under the same caps.
	ls, err := m.run(ctx, path, "ls-tree", "-r", "-l", "-z", sha, "--", "definitions", "packs")
	if err != nil {
		return nil, "", err
	}
	// docs/adr/0063: packs/ never fails the definitions read. A bad entry
	// there becomes packs/<name>/.invalid.md carrying the reason, which core
	// turns into that pack's error; packs/.listed.md says this provisioner
	// read packs/ at all, so core can tell "deleted" from "older provisioner".
	// Both end in .md, which an older core's definitions loader skips.
	files := map[string][]byte{"packs/.listed.md": nil}
	total, packTotal := 0, 0
	for _, entry := range strings.Split(ls, "\x00") {
		meta, rel, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta) // mode type object size
		if rel == "packs" || strings.HasPrefix(rel, "packs/") {
			if reason := packEntryProblem(rel, f, packTotal); reason != "" {
				name, _, _ := strings.Cut(strings.TrimPrefix(rel, "packs/"), "/")
				files["packs/"+name+"/.invalid.md"] = []byte(reason)
				continue
			}
			size, _ := strconv.Atoi(f[3])
			packTotal += size
		} else {
			switch {
			case rel == "definitions":
				return nil, "", fmt.Errorf("definitions/ is not a directory at %s (%.7s)", ref, sha)
			case len(f) != 4 || (f[0] != "100644" && f[0] != "100755"):
				return nil, "", fmt.Errorf("read definitions: %s is not a regular file", rel)
			}
			size, err := strconv.Atoi(f[3])
			if err != nil {
				return nil, "", fmt.Errorf("read definitions: %s: size %q", rel, f[3])
			}
			if size > maxDefinitionFile {
				return nil, "", fmt.Errorf("read definitions: %s is %d bytes, over %d", rel, size, maxDefinitionFile)
			}
			if total += size; total > maxDefinitionTotal {
				return nil, "", fmt.Errorf("read definitions: definitions/ is over %d bytes", maxDefinitionTotal)
			}
		}
		// Not m.run: that trims, and this is file content.
		start, args := time.Now(), []string{"cat-file", "blob", f[2]}
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = path
		b, err := cmd.Output()
		metrics.ObserveGit(args, start)
		if err != nil {
			return nil, "", fmt.Errorf("read definitions: %s: %w", rel, err)
		}
		files[rel] = b
	}
	if !slices.ContainsFunc(slices.Collect(maps.Keys(files)), func(p string) bool { return strings.HasPrefix(p, "definitions/") }) {
		return nil, "", fmt.Errorf("no definitions/ at %s (%.7s)", ref, sha)
	}
	return files, sha, nil
}

// PrefetchRepos fetches every repo cache a warm is likely to need soon, so the
// warm's own synchronous fetch is a small delta (a 20-merge-stale cache took
// 19.9 s to fetch on a warm, a fresh one ~3.2 s; Tempo, 2026-11-16).
//
// Selection: a cache directory exists (never clone here: a cache exists only
// because a warm or sync already wanted the repo) AND it was not last used
// more than idle ago. Caches not used since this process started count as
// active: lastUsed is in memory, and a restart must not park every repo.
// Only repos that sat idle through a whole window of this process's life drop
// out; a warm on one of those fetches synchronously, as before.
//
// Each fetch is EnsureRepoFetched's exact fetch (same lock, refspec,
// credentials, origin check, gc.auto), on a context detached from ctx and
// bounded by perFetch, so shutdown does not SIGKILL git mid-write. A repo's
// failure is joined into the result and never stops the others.
func (m *Manager) PrefetchRepos(ctx context.Context, idle, perFetch time.Duration) error {
	ctx = context.WithValue(ctx, prefixKey{}, "prefetch")
	entries, err := os.ReadDir(filepath.Join(m.root, "repos"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var errs []error
	for _, e := range entries {
		repo := e.Name()
		if ctx.Err() != nil {
			break
		}
		if !e.IsDir() || !m.prefetchWanted(repo, idle) {
			continue
		}
		if err := m.prefetchOne(ctx, repo, perFetch); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repo, err))
		}
	}
	return errors.Join(errs...)
}

// prefetchWanted: a real clone (has .git) not idle past the window.
func (m *Manager) prefetchWanted(repo string, idle time.Duration) bool {
	if _, err := os.Stat(filepath.Join(m.repoPath(repo), ".git")); err != nil {
		return false
	}
	m.mu.Lock()
	t, seen := m.lastUsed[repo]
	m.mu.Unlock()
	return !seen || time.Since(t) <= idle
}

func (m *Manager) prefetchOne(ctx context.Context, repo string, perFetch time.Duration) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), perFetch)
	defer cancel()
	// The cache's own origin is the URL: the origin check in ensureFetched
	// then passes by construction, and the cache is never re-pointed.
	url, err := m.run(ctx, m.repoPath(repo), "config", "--get", "remote.origin.url")
	if err != nil {
		return err
	}
	return m.ensureFetched(ctx, repo, url)
}
