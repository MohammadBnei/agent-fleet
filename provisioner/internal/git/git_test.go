package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MohammadBnei/agent-fleet/provisioner/internal/spans"
)

// newOriginRepo builds a real local git repo to clone from — same "real temp
// git repo, no mocking" convention the grpcserver tests use.
func newOriginRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"add", "README.md"}, {"commit", "-m", "init"}} {
		if args[0] == "add" {
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.io", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.io")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func commitIn(t *testing.T, dir string) string {
	t.Helper()
	for _, args := range [][]string{{"commit", "--allow-empty", "-m", "next"}, {"rev-parse", "HEAD"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.io", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.io")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		if args[0] == "rev-parse" {
			return string(out[:len(out)-1])
		}
	}
	return ""
}

// No pod's clone contacts the remote (docs/adr/0064, 0075), so the cache
// fetch is load-bearing: EnsureRepoFetched waits out a busy lock and then
// fetches, bounded by ctx.
func TestEnsureRepoFetched_WaitsForTheLockThenFetches(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := context.Background()
	origin := newOriginRepo(t)
	if _, err := m.EnsureRepoCloned(ctx, "r", origin); err != nil {
		t.Fatal(err)
	}
	tip := commitIn(t, origin)

	lock := m.repoLock("r")
	lock.Lock()
	done := make(chan error, 1)
	go func() { done <- m.EnsureRepoFetched(ctx, "r", origin) }()
	select {
	case err := <-done:
		t.Fatalf("returned while the lock was held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	lock.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := m.originHead(ctx, m.repoPath("r")); got != tip {
		t.Fatalf("origin/HEAD %s, want the remote's new tip %s", got, tip)
	}

	lock.Lock()
	defer lock.Unlock()
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	commitIn(t, origin)
	if err := m.EnsureRepoFetched(short, "r", origin); err != context.DeadlineExceeded {
		t.Fatalf("got %v, want the ctx error", err)
	}
	if got := m.originHead(ctx, m.repoPath("r")); got != tip {
		t.Fatal("no git may run past the deadline")
	}
}

// docs/adr/0064: the scrub runs on every definition warm, but plugins/ alone is
// the image's copy from the entrypoint's last boot. Removing it would re-run
// that cp -r every warm, so it goes only when a sync left settings.json & co.
func TestRemoveFleetShared_KeepsTheImagePluginsWithoutASync(t *testing.T) {
	m := NewManager(t.TempDir())
	home := m.ClaudeHomePath("s")
	write := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(home, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, p), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(p string) bool {
		_, err := os.Stat(filepath.Join(home, p))
		return err == nil
	}
	write("plugins/p/plugin.json")
	write("projects/p/s.jsonl")
	if err := m.RemoveFleetShared("s"); err != nil {
		t.Fatal(err)
	}
	if !exists("plugins/p/plugin.json") {
		t.Fatal("no sync happened, yet the image's plugins/ was removed")
	}

	write("settings.json")
	write("skills/x/SKILL.md")
	if err := m.RemoveFleetShared("s"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"settings.json", "skills", "plugins"} {
		if exists(p) {
			t.Errorf("%s survived the scrub of a synced claude-home", p)
		}
	}
	if !exists("projects/p/s.jsonl") {
		t.Error("resume state removed")
	}
}

// docs/adr/0067 §4: the empty skills/ kubelet makes as the genome mount point
// is not a sync, so plugins/ stays and the entrypoint skips its cp -r.
func TestRemoveFleetShared_EmptySkillsMountPointIsNotASync(t *testing.T) {
	m := NewManager(t.TempDir())
	home := m.ClaudeHomePath("s")
	for _, d := range []string{"plugins/x", "skills"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.RemoveFleetShared("s"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "plugins/x")); err != nil {
		t.Fatalf("plugins/ removed for an empty skills mount point: %v", err)
	}
	// A real sync leftover still goes.
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveFleetShared("s"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("settings.json survived: %v", err)
	}
}

// docs/adr/0065 §6: a definition's cache must have been cloned from the repo
// URL the warm names. ensureLocked never re-points a warm cache, so a
// repos.url repointed A -> B -> A would otherwise leave a cache cloned from B.
func TestEnsureRepoFetched_RefusesACacheFromAnotherOrigin(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := context.Background()
	a, b := newOriginRepo(t), newOriginRepo(t)
	if err := m.EnsureRepoFetched(ctx, "r", a); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureRepoFetched(ctx, "r", a+".git"); err != nil {
		t.Errorf("a trailing .git is the same repo: %v", err)
	}
	if err := m.EnsureRepoFetched(ctx, "r", b); err == nil || !strings.Contains(err.Error(), "re-clone") || !strings.Contains(err.Error(), m.repoPath("r")) {
		t.Errorf("a cache cloned from another URL was accepted: %v", err)
	}
}

// A refresh whose fetch moved nothing skips the hard reset (~1.4s of index
// refresh on the RWX volume for a no-op); a moved ref, or a checkout with no
// index (a clone killed mid-checkout), still resets.
func TestRefreshFleetShared_ResetsOnlyWhenSomethingMoved(t *testing.T) {
	m := NewManager(t.TempDir())
	origin := newOriginRepo(t)
	refresh := func() string {
		t.Helper()
		ctx := spans.WithRecorder(context.Background())
		if err := m.RefreshFleetShared(ctx, origin, "main"); err != nil {
			t.Fatal(err)
		}
		return spans.Summary(ctx)
	}
	if got := refresh(); !strings.Contains(got, "warm.git.clone") {
		t.Fatalf("first refresh did not clone: %s", got)
	}
	if got := refresh(); !strings.Contains(got, "warm.git.fetch") || strings.Contains(got, "warm.git.reset") {
		t.Errorf("unchanged refresh: want a fetch and no reset, got %s", got)
	}
	head := commitIn(t, origin)
	if got := refresh(); !strings.Contains(got, "warm.git.reset") {
		t.Errorf("moved ref did not reset: %s", got)
	}
	if got, err := m.run(context.Background(), m.fleetSharedPath(), "rev-parse", "HEAD"); err != nil || got != head {
		t.Errorf("HEAD = %q (%v), want %q", got, err, head)
	}
	if err := os.Remove(filepath.Join(m.fleetSharedPath(), ".git", "index")); err != nil {
		t.Fatal(err)
	}
	// A broken checkout (no index: a clone killed mid-checkout) is re-cloned.
	if got := refresh(); !strings.Contains(got, "warm.git.clone") {
		t.Errorf("checkout without an index was not re-cloned: %s", got)
	}
	if _, err := os.Stat(filepath.Join(m.fleetSharedPath(), ".git", "index")); err != nil {
		t.Errorf("re-clone left no index: %v", err)
	}
}

// Once refreshed, a warm lays in the local copy without fetching: a commit
// on the source reaches claude-home only after the next refresh. A fresh
// Manager (cold start) refreshes on the warm itself.
func TestSyncFleetShared_LaysInTheRefreshedCopyWithoutFetching(t *testing.T) {
	m := NewManager(t.TempDir())
	origin := newOriginRepo(t)
	write := func(body string) {
		t.Helper()
		dst := filepath.Join(origin, "fleet-shared", "CLAUDE.md")
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", body}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = origin
			cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.io", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.io")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
		}
	}
	sync := func() (string, string) {
		t.Helper()
		ctx := spans.WithRecorder(context.Background())
		home := m.ClaudeHomePath("s")
		if err := m.SyncFleetShared(ctx, origin, "main", home); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(home, "CLAUDE.md"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b), spans.Summary(ctx)
	}
	write("v1")
	if body, steps := sync(); body != "v1" || !strings.Contains(steps, "warm.fleet_shared_refresh") {
		t.Fatalf("cold sync: body %q, steps %s", body, steps)
	}
	write("v2, longer")
	if body, steps := sync(); body != "v1" || strings.Contains(steps, "warm.git.") {
		t.Errorf("warm sync fetched: body %q, steps %s", body, steps)
	}
	if err := m.RefreshFleetShared(context.Background(), origin, "main"); err != nil {
		t.Fatal(err)
	}
	if body, _ := sync(); body != "v2, longer" {
		t.Errorf("after refresh: body %q, want v2, longer", body)
	}
	// A failed fetch (and failed re-clone: an outage) keeps the intact
	// checkout, still ready: the warm lays in the stale copy.
	if err := m.RefreshFleetShared(context.Background(), origin, "no-such-branch"); err == nil {
		t.Fatal("refresh of a missing branch succeeded")
	}
	if !m.fleetSharedReady.Load() {
		t.Error("a failed fetch over an intact checkout cleared ready")
	}
	if body, steps := sync(); body != "v2, longer" || strings.Contains(steps, "warm.git.") {
		t.Errorf("warm after a failed fetch: body %q, steps %s", body, steps)
	}
	// A missing clone in a process that had one: the next refresh re-clones.
	if err := os.RemoveAll(m.fleetSharedPath()); err != nil {
		t.Fatal(err)
	}
	if err := m.RefreshFleetShared(context.Background(), origin, "main"); err != nil {
		t.Fatal(err)
	}
	if body, _ := sync(); body != "v2, longer" {
		t.Errorf("after a re-clone: body %q", body)
	}
}

// A warm never waits on the fleet-shared lock past its own ctx: it gives up
// and returns the error, so the caller goes on without the sync.
func TestSyncFleetShared_GivesUpAtTheWarmsDeadline(t *testing.T) {
	m := NewManager(t.TempDir())
	origin := newOriginRepo(t)
	if err := m.RefreshFleetShared(context.Background(), origin, "main"); err != nil {
		t.Fatal(err)
	}
	unlock := m.LockRepo(fleetSharedSentinel)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := m.SyncFleetShared(ctx, origin, "main", m.ClaudeHomePath("s")); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("sync behind a held lock: %v, want the deadline", err)
	}
}

// docs/adr/0063 §8: once fleet-shared/CLAUDE.md and skills/ leave the source,
// a resumed session's synced copies go too. settings.json and the image's
// plugins/ stay.
func TestSyncFleetShared_RemovesWhatLeftTheSource(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := context.Background()
	origin := newOriginRepo(t)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = origin
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.io", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.io")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	for _, p := range []string{"CLAUDE.md", "settings.json", "skills/x/SKILL.md"} {
		dst := filepath.Join(origin, "fleet-shared", p)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-m", "fleet-shared")

	// The background tick's job (RefreshFleetShared): a warm lays in the
	// local copy without fetching.
	refresh := func() {
		t.Helper()
		if err := m.RefreshFleetShared(ctx, origin, "main"); err != nil {
			t.Fatal(err)
		}
	}
	home := m.ClaudeHomePath("s")
	if err := os.MkdirAll(filepath.Join(home, "plugins", "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncFleetShared(ctx, origin, "main", home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "skills", "x", "SKILL.md")); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	git("rm", "-rq", "fleet-shared/CLAUDE.md", "fleet-shared/skills")
	git("commit", "-m", "moved to packs")
	refresh()
	if err := m.SyncFleetShared(ctx, origin, "main", home); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{"CLAUDE.md": false, "skills": false, "settings.json": true, "plugins/p": true} {
		if _, err := os.Stat(filepath.Join(home, p)); (err == nil) != want {
			t.Errorf("%s: present=%v, want %v", p, err == nil, want)
		}
	}
	// A source without fleet-shared/ at all is a wrong ref: it errors and
	// deletes nothing.
	if err := os.WriteFile(filepath.Join(home, "CLAUDE.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("rm", "-rq", "fleet-shared")
	git("commit", "-m", "wrong ref")
	refresh()
	if err := m.SyncFleetShared(ctx, origin, "main", home); err == nil {
		t.Error("a source with no fleet-shared/ synced without error")
	}
	if _, err := os.Stat(filepath.Join(home, "CLAUDE.md")); err != nil {
		t.Errorf("CLAUDE.md removed on a source with no fleet-shared/: %v", err)
	}
}

// PrefetchRepos fetches caches that exist (never clones), keeps going past a
// broken one, and leaves a cache idle past the window alone.
func TestPrefetchRepos_SelectionAndErrors(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := context.Background()
	origins := map[string]string{}
	for _, r := range []string{"a", "idle", "zbroken"} {
		origins[r] = newOriginRepo(t)
		if _, err := m.EnsureRepoCloned(ctx, r, origins[r]); err != nil {
			t.Fatal(err)
		}
	}
	// "nocache" has a directory but is no clone.
	if err := os.MkdirAll(m.repoPath("nocache"), 0o755); err != nil {
		t.Fatal(err)
	}
	// zbroken: origin gone, so its fetch fails. "a" sorts before it, "idle"
	// was last used long ago.
	if err := os.RemoveAll(origins["zbroken"]); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.lastUsed["idle"] = time.Now().Add(-48 * time.Hour)
	m.mu.Unlock()
	tipA, tipIdle := commitIn(t, origins["a"]), commitIn(t, origins["idle"])

	if err := m.PrefetchRepos(ctx, 24*time.Hour, time.Minute); err != nil {
		t.Fatalf("a failing repo must not fail the cycle: %v", err)
	}
	m.mu.Lock()
	failed := m.prefetchFails["zbroken"]
	m.mu.Unlock()
	if failed != 1 {
		t.Fatalf("zbroken failures %d, want 1 counted", failed)
	}
	if got := m.originHead(ctx, m.repoPath("a")); got != tipA {
		t.Errorf("a: origin/HEAD %s, want %s", got, tipA)
	}
	if got := m.originHead(ctx, m.repoPath("idle")); got == tipIdle {
		t.Error("idle repo was fetched")
	}
	if _, err := os.Stat(filepath.Join(m.repoPath("nocache"), ".git")); err == nil {
		t.Error("prefetch cloned into a directory that was no cache")
	}
	if _, err := os.Stat(m.repoPath("zbroken")); err != nil {
		t.Error("a failed prefetch must leave the cache in place")
	}
}

func TestPrefetchRepos_NoReposDir(t *testing.T) {
	if err := NewManager(t.TempDir()).PrefetchRepos(context.Background(), time.Hour, time.Minute); err != nil {
		t.Fatal(err)
	}
}

// A cache no warm has touched gets one idle window from the Manager's start,
// then drops out.
func TestPrefetchWanted_UnseenRepoAgesFromStart(t *testing.T) {
	m := NewManager(t.TempDir())
	if _, err := m.EnsureRepoCloned(context.Background(), "r", newOriginRepo(t)); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	delete(m.lastUsed, "r")
	m.mu.Unlock()
	if !m.prefetchWanted("r", time.Hour) {
		t.Fatal("fresh start: want grace")
	}
	m.start = time.Now().Add(-2 * time.Hour)
	if m.prefetchWanted("r", time.Hour) {
		t.Fatal("unseen for a whole window: want dropped")
	}
}

// failLsRemote puts a git on PATH that refuses ls-remote and runs the real
// git for everything else.
func failLsRemote(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	shim := "#!/bin/sh\n[ \"$1\" = ls-remote ] && { echo 'fatal: unable to access remote' >&2; exit 128; }\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The warm fetch is skipped only when ls-remote shows the cache already holds
// every branch and tag the remote has; any change, or an ls-remote failure,
// fetches as before, so the cache stays exactly as fresh as an always-fetch.
func TestEnsureRepoFetched_FetchesOnlyWhenTheRemoteMoved(t *testing.T) {
	cases := []struct {
		name      string
		change    func(t *testing.T, origin string)
		wantFetch bool
	}{
		{"head equal, annotated tag on both: no fetch", func(*testing.T, string) {}, false},
		{"head differs: fetch", func(t *testing.T, o string) { commitIn(t, o) }, true},
		{"new branch: fetch", func(t *testing.T, o string) { gitIn(t, o, "branch", "feature") }, true},
		{"new tag, no branch moved: fetch", func(t *testing.T, o string) { gitIn(t, o, "tag", "v2") }, true},
		{"ls-remote fails, head differs: fetch", func(t *testing.T, o string) { failLsRemote(t); commitIn(t, o) }, true},
		{"ls-remote fails, nothing moved: still fetch", func(t *testing.T, _ string) { failLsRemote(t) }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(t.TempDir())
			origin := newOriginRepo(t)
			gitIn(t, origin, "tag", "-a", "v1", "-m", "v1")
			if err := m.EnsureRepoFetched(context.Background(), "r", origin); err != nil {
				t.Fatal(err)
			}
			tc.change(t, origin)
			ctx := spans.WithRecorder(context.Background())
			if err := m.EnsureRepoFetched(ctx, "r", origin); err != nil {
				t.Fatal(err)
			}
			steps := spans.Summary(ctx)
			if got := strings.Contains(steps, "warm.git.fetch="); got != tc.wantFetch {
				t.Fatalf("fetched = %v, want %v: %s", got, tc.wantFetch, steps)
			}
			if !strings.Contains(steps, "warm.git.ls-remote=") {
				t.Errorf("no ls-remote span: %s", steps)
			}
			if want, got := gitIn(t, origin, "rev-parse", "HEAD"), m.originHead(context.Background(), m.repoPath("r")); got != want {
				t.Errorf("origin/HEAD %s, want the remote's %s", got, want)
			}
		})
	}
	// After any sync the cache matches the remote, so the next one skips.
	m := NewManager(t.TempDir())
	origin := newOriginRepo(t)
	for range 2 {
		if err := m.EnsureRepoFetched(context.Background(), "r", origin); err != nil {
			t.Fatal(err)
		}
		gitIn(t, origin, "tag", "-a", "v"+commitIn(t, origin)[:7], "-m", "x")
	}
	if err := m.EnsureRepoFetched(context.Background(), "r", origin); err != nil {
		t.Fatal(err)
	}
	if !m.remoteUnchanged(context.Background(), m.repoPath("r")) {
		t.Error("cache still differs from the remote after a fetch")
	}
}
