package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if err := m.SyncFleetShared(ctx, origin, "main", home); err == nil {
		t.Error("a source with no fleet-shared/ synced without error")
	}
	if _, err := os.Stat(filepath.Join(home, "CLAUDE.md")); err != nil {
		t.Errorf("CLAUDE.md removed on a source with no fleet-shared/: %v", err)
	}
}
